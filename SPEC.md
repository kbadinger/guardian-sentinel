# guardian-sentinel — Build Spec v1

**One-liner:** a small Go daemon that runs 24/7 on velma (the always-on Mac mini),
probes Guardian's real surfaces every minute, and sends a Telegram alert when
something goes down — and a recovery notice when it comes back.

**Why it exists:** (a) real operational value — Guardian had a backup/corruption
incident in July 2026 and its watchers have no watcher; (b) it is the vehicle for
learning Go by shipping production software (Guardian task `9c4f6f61`, project
`career`). The resume claim this build must make true: *"Go service running in
production since July 2026."* Nothing more ("expert Go" is NOT claimed).

**Time budget:** ~8 hours total. Scope is ruthless; anything not listed under
Milestones is out (see Non-goals).

---

## Hard constraints

1. **Stdlib only. Zero third-party dependencies.** Telegram is a plain HTTPS POST;
   probes are `net/http` + `os` stat calls. This is idiomatic for a tool this size,
   removes all supply-chain risk, and is a deliberate portfolio flex.
2. **Read-only prober.** Sentinel never writes to Guardian's DB, API, or files.
   Its only side effect is Telegram messages (and its own log).
3. **Config via environment variables only.** No config files, no flags (except
   `-version` and `-dry-run` if convenient). Secrets live in
   `~/.guardian-sentinel.env` on velma (gitignored pattern), loaded by the launchd
   wrapper script — never committed anywhere, never baked into the plist.
4. **Single static binary**, built for `darwin/arm64` (velma is an arm64 Mac on
   macOS 26.3 — verified 2026-07-28). The laptop is also darwin/arm64, so a
   laptop-built binary runs on velma unmodified.
5. **Public repo.** No personal data in code, comments, tests, or fixtures. The
   Tailscale IP and paths in this spec are fine as *defaults documented in
   README/env.example* (they're private-network addresses), but tokens never.

## Architecture & layout

```
cmd/guardian-sentinel/main.go   — flag/env parsing, wiring, run loop entry
internal/probe/probe.go         — Probe interface + Result type
internal/probe/httpprobe.go     — generic HTTP probe (status + body substring)
internal/probe/mcpprobe.go      — MCP JSON-RPC initialize handshake probe
internal/probe/backupprobe.go   — backup-freshness probe (filesystem)
internal/monitor/monitor.go     — scheduler + per-probe state machine
internal/alert/telegram.go      — Telegram sendMessage alerter (+ dry-run)
deploy/                         — launchd plist, wrapper script, env example
```

`Probe` interface (shape, adjust as idiomatic):

```go
type Result struct {
    OK      bool
    Detail  string        // human line for the alert message
    Latency time.Duration
}

type Probe interface {
    Name() string
    Check(ctx context.Context) Result
}
```

## The four probes (all facts below verified on 2026-07-28)

Sentinel runs ON velma, so all probes hit localhost — deliberately independent of
Tailscale, which is its own failure domain.

| # | Name     | Method | Success criteria |
|---|----------|--------|------------------|
| 1 | `api`    | GET `http://127.0.0.1:8002/health` | HTTP 200 **and** body contains `"status":"healthy"`. The endpoint exists in `backend/api/routes/health.py`, mounted without the `/api` prefix; it returns 503 when a critical service is down — treat non-200 as DOWN with the body's detail. |
| 2 | `web`    | GET `http://127.0.0.1:3100/` | HTTP 200 **and** body contains `SENTINEL_WEB_EXPECT` (default `Guardian`). 🔴 Builder: curl the live page once at deploy time and pick a marker string actually present — per Kevin's rule, a 200 alone is NOT proof the right app answered. |
| 3 | `mcp`    | POST `http://127.0.0.1:8765/mcp` — JSON-RPC `initialize` handshake, headers `Authorization: <SENTINEL_MCP_TOKEN>`, `Content-Type: application/json`, `Accept: application/json, text/event-stream`. Body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"guardian-sentinel","version":"<ver>"}}}` | Response body contains `guardian-memory`. This is the exact probe the proven `guardian-health-check.sh` hook uses (response may be SSE-framed — substring match, don't parse). A real handshake matters: the 2026-07-23 incident was the MCP being up-but-dropping-responses, which a TCP/port check would miss. |
| 4 | `backup` | Filesystem stat under `SENTINEL_BACKUP_DIR` (default `/Users/velma/projects/guardian/backups`) | The newest `guardian-*` subdirectory contains `guardian.db.gz` with mtime < `SENTINEL_BACKUP_MAX_AGE` (default 26h). Verified layout: nightly launchd job (03:30) writes `guardian-<YYYYMMDD-HHMMSS>/guardian.db.gz`; last night's snapshot confirmed present. NOTE: `~/guardian-backups` is a DIFFERENT directory (old worktree tarballs) — do not point there. |

**Why probe 4 despite the existing 04:00 backup-verify job:** that job alerts when
a restore-test *fails*, but nothing alerts when the backup or verify jobs silently
*stop running*. Freshness-of-artifact is the watchman-for-the-watchman. Do not
duplicate the restore-verify logic.

## Monitor loop & state machine

- Tick every `SENTINEL_INTERVAL` (default 60s). Each tick runs all probes
  concurrently (goroutine per probe, `sync.WaitGroup`), each with its own
  `context.WithTimeout` of `SENTINEL_PROBE_TIMEOUT` (default 10s).
- Per-probe state: consecutive-failure counter.
  - UP → DOWN when failures reach `SENTINEL_FAIL_THRESHOLD` (default 3
    consecutive) → send DOWN alert. Threshold exists so a single timeout blip
    doesn't page.
  - While DOWN: re-alert every `SENTINEL_REALERT_COOLDOWN` (default 6h), not
    every tick.
  - DOWN → UP on first success → send RECOVERY alert (include outage duration).
- On startup: run one probe round; send a single startup summary message
  ("sentinel online — api ✓ web ✓ mcp ✓ backup ✓") so a dead sentinel is itself
  noticeable by silence, and log to stdout (launchd captures to file).
- The state machine lives in `internal/monitor` with NO real I/O — it takes probe
  results in and returns "alerts to send" out. This is where the table-driven
  tests earn their keep.

## Alerting (Telegram)

- `POST https://api.telegram.org/bot<TELEGRAM_BOT_TOKEN>/sendMessage` with
  `chat_id=TELEGRAM_CHAT_ID`, plain text. Reuses Guardian's existing bot; the
  chat id is Kevin's Telegram user id (same value as Guardian's
  `TELEGRAM_ALLOWED_USER_ID` on velma).
- Message format (keep it plain, one glance):
  - `🚨 guardian-sentinel: API DOWN — 3 consecutive failures. Last error: connection refused (probe latency 10s timeout). 11:42 ET`
  - `✅ guardian-sentinel: API RECOVERED after 14m. 11:56 ET`
- Send failures: log and carry on (never crash the loop because Telegram is down).
- `SENTINEL_DRY_RUN=1` prints messages to stdout instead of sending — used in
  development and in the live-fire rehearsal.

## Configuration (env vars, all with defaults except secrets)

| Var | Default | Notes |
|-----|---------|-------|
| `SENTINEL_INTERVAL` | `60s` | Go duration syntax |
| `SENTINEL_PROBE_TIMEOUT` | `10s` | per probe per tick |
| `SENTINEL_FAIL_THRESHOLD` | `3` | consecutive failures → DOWN |
| `SENTINEL_REALERT_COOLDOWN` | `6h` | re-page cadence while DOWN |
| `SENTINEL_API_URL` | `http://127.0.0.1:8002/health` | |
| `SENTINEL_WEB_URL` | `http://127.0.0.1:3100/` | |
| `SENTINEL_WEB_EXPECT` | `Guardian` | 🔴 verify real marker at deploy |
| `SENTINEL_MCP_URL` | `http://127.0.0.1:8765/mcp` | |
| `SENTINEL_MCP_TOKEN` | — (secret) | full `Bearer <token>` header value |
| `SENTINEL_BACKUP_DIR` | `/Users/velma/projects/guardian/backups` | verified path |
| `SENTINEL_BACKUP_MAX_AGE` | `26h` | nightly 03:30 + slack |
| `TELEGRAM_BOT_TOKEN` | — (secret) | reuse Guardian's bot |
| `TELEGRAM_CHAT_ID` | — (secret-ish) | Kevin's Telegram user id |
| `SENTINEL_DRY_RUN` | unset | `1` = print, don't send |

## Testing (table-driven, no network)

- `internal/monitor`: the centerpiece. Table-driven tests covering: blip below
  threshold (no alert), threshold crossing (one DOWN alert), continued failure
  inside cooldown (no re-alert), past cooldown (re-alert), recovery (RECOVERY
  alert + counter reset), multiple probes independent.
- `internal/probe`: `httptest.NewServer` for api/web/mcp probes (status codes,
  body markers, timeout via a hanging handler); `t.TempDir()` fixtures for the
  backup probe (fresh dir, stale dir, empty dir, wrong-layout dir).
- `internal/alert`: dry-run path + a `httptest` Telegram endpoint asserting the
  outgoing request shape.
- `go test -race ./...` must pass. No test touches the real network, velma, or
  real Telegram.

## CI (GitHub Actions)

`.github/workflows/ci.yml` (scaffolded in this repo): `gofmt -l` clean,
`go vet`, `go test -race ./...`, `staticcheck ./...`. Actions pinned to
long-established major tags (`actions/checkout@v4`, `actions/setup-go@v5`);
staticcheck pinned to `2025.1.1` — 🔴 builder: verify that version is ≥7 days old
and supports the Go toolchain in use (house supply-chain rule), bump the pin if not.

## Deployment (velma, launchd)

Velma has NO Go installed (verified 2026-07-28). Two options; prefer A for
matching the house git-based deploy discipline:

- **A (preferred):** `ssh velma 'brew install go'` (mature package — fine under
  the 7-day rule), clone the repo to `/Users/velma/projects/guardian-sentinel`
  (sibling of the live guardian checkout at `/Users/velma/projects/guardian`),
  `go build -o ~/bin/guardian-sentinel ./cmd/guardian-sentinel`.
- **B (fallback):** build on the laptop (same darwin/arm64) and `scp` the binary.

Then:
1. Create `~/.guardian-sentinel.env` on velma from
   `deploy/guardian-sentinel.env.example` (secrets: MCP token from velma's
   Guardian env, Telegram token/chat-id from the existing Telegram runner's env).
   `chmod 600`.
2. Install `deploy/com.local.guardian-sentinel.plist` →
   `~/Library/LaunchAgents/`, which runs `deploy/start-guardian-sentinel.sh`
   (sources the env file, execs the binary). `launchctl bootstrap gui/$(id -u) …`.
   KeepAlive=true, logs to `~/Library/Logs/guardian-sentinel.log`.
3. **Live-fire acceptance test (required):** stop the web service
   (`launchctl bootout` the guardian-web agent or equivalent kickstart trick),
   watch the DOWN alert arrive on Telegram within ~3 intervals, restart it, watch
   the RECOVERY alert. Screenshot goes in the README. Do this at a moment Kevin
   is aware — it briefly takes the web UI down. Do NOT live-fire against the API
   or MCP (other agents may be mid-write); web is the safe victim.

## Optional stretch (NOT in the 8h; only if Kevin wants the K8s literacy signal)

`deploy/k8s/` with a plain Deployment + Secret + Kustomize overlay for the
sentinel container (plus a minimal Dockerfile), validated in CI with kubeconform
— **no cluster anywhere**. This demonstrates current K8s fluency in a public
repo. It deliberately does NOT claim "Kubernetes at production scale" — that
claim only comes from real work history, and a hobby manifest must never be
dressed up as it.

## Non-goals (v1)

No Prometheus/metrics endpoint, no web dashboard, no config file, no multi-host
support, no probing of Tailscale reachability, no restarting/self-healing of
Guardian services (read-only rule), no rewrite of any existing backup/verify
logic, no third-party Go modules.

## Milestones (~8h)

| # | Time | Deliverable |
|---|------|-------------|
| M0 | 0.5h | Repo builds: `go vet`/`gofmt`/empty test green locally and in CI |
| M1 | 1.5h | Go essentials pass (Tour of Go + Effective Go skim), then `Probe` interface + `Result` + monitor state machine **types** written as the learning vehicle |
| M2 | 2.0h | All four probes implemented with table-driven tests (httptest + tempdir) |
| M3 | 1.5h | Monitor loop + state machine complete with the full test table |
| M4 | 1.0h | Telegram alerter + dry-run; end-to-end dry-run on the laptop against velma URLs (over Tailscale, just for the rehearsal) |
| M5 | 1.5h | Deploy on velma via launchd, live-fire test, README with screenshot |

## Definition of done

- Public GitHub repo (`github.com/kbadinger/guardian-sentinel`), CI green,
  staticcheck clean, `go test -race` passing.
- Running on velma under launchd with KeepAlive; survived a reboot or a manual
  `kickstart -k`.
- Live-fire evidence in README (DOWN + RECOVERY screenshots).
- README states plainly what it is, how it's configured, and shows the
  architecture in ~10 lines — written for a hiring engineer skimming in 90s.
- Guardian task `9c4f6f61` updated: done-criteria met, `actual_minutes` recorded.
- THEN (separate follow-ups, not this build): resume/profile update via JobForge;
  blog post task in the `[blog]` project.

## Open questions for Kevin 🔴

1. **License** — MIT recommended for a portfolio repo; confirm before the repo
   goes public.
2. **Repo visibility timing** — public from first push, or public at M5? (Spec
   assumed public early; either is fine, done-criteria just needs public at end.)
3. **Live-fire window** — say when it's OK to bounce the web service for ~2 min.
4. **K8s stretch** — want the `deploy/k8s/` + Dockerfile literacy add-on (~1h
   extra), or keep v1 pure?
