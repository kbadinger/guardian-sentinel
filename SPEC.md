# guardian-sentinel — Build Spec v2

**One-liner:** one small Go monitoring service, deployed twice: on velma (launchd)
watching Guardian's internals from inside, and on a $12/mo DigitalOcean Kubernetes
cluster watching every live site Kevin has shipped from the real internet — the two
instances covering each other with a dead-man's switch, all alerts to Telegram.

**Why it exists:** (a) real operational value — today NOTHING monitors Kevin's
public products (ThisWeekEats, PenForge, driftnightly, kevinbadinger.com,
ReadReceipt, eRail, qtown), Guardian's watchers have no watcher, and no design
except an off-box monitor can catch "velma itself is down"; (b) it is the vehicle
for two job-search skill gaps at once — **Go** (Guardian task `9c4f6f61`) and
**Kubernetes in production** (the claims-ladder rung from the K8s plan) — closed
by one artifact that does a genuine job. Decided with Kevin 2026-07-28.

**The resume claims this build makes true (and nothing more):**
- "I wrote a Go monitoring service and run it in production on managed Kubernetes
  (DigitalOcean) — container registry, CI/CD deploys, rolling updates."
- "It monitors my whole product portfolio and pages my phone; the on-prem and
  cloud instances watch each other with a dead-man's switch."
- Never claim: "enterprise scale", multi-cluster, service mesh, "expert Go".

**Time budget:** ~12 hours total, structured so the first 8 (velma v1) stand alone.

---

## Architecture — one binary, config-driven, two deployments

```
                    ┌──────────────────────────────────────────┐
                    │  DOKS (1× $12/mo node, linux/amd64)      │
   internet ──────► │  sentinel --config fleet.json            │──► Telegram
   (probes out)     │  probes: public sites + cert expiry      │
                    │  heartbeat RECEIVER (NodePort + token)   │
                    └────────────────▲─────────────────────────┘
                                     │ heartbeat POST every 5 min
                    ┌────────────────┴─────────────────────────┐
                    │  velma (launchd, darwin/arm64)           │
                    │  sentinel --config internal.json         │──► Telegram
                    │  probes: API/web/MCP/backup freshness    │
                    └──────────────────────────────────────────┘
```

- **Same binary, same repo.** Behavior differs only by config file. No forked code.
- **Dead-man's switch, both directions:** the velma instance POSTs a heartbeat to
  the DOKS instance; if heartbeats go silent past a threshold, DOKS alerts
  "velma is down" — the one failure no on-box watcher can report. Symmetrically,
  if velma's heartbeat POSTs fail repeatedly, velma alerts "cloud monitor
  unreachable". Neither side needs inbound access to the other's network beyond
  one tokened NodePort endpoint on the DOKS node.
- **Read-only stance everywhere:** probes observe, never mutate, never restart
  anything. Only side effects: Telegram messages, heartbeats, its own logs.
- **Guardian itself never moves to Kubernetes.** launchd is correct for it, and
  saying so is itself a good interview answer.

## Hard constraints

1. **Stdlib only — zero third-party Go dependencies.** JSON config via
   `encoding/json` (this is why config is JSON, not YAML — stdlib has no YAML).
   Telegram via `net/http`. Cert expiry via `crypto/tls` connection state.
2. **Config file declares probes; env carries secrets.** `SENTINEL_CONFIG` env →
   path to a JSON file (plain file on velma, ConfigMap in k8s). Secrets
   (Telegram token, MCP bearer, heartbeat token) come ONLY from env (launchd env
   file on velma, k8s Secret on DOKS) and are referenced from config by env-var
   NAME, never by value. No secret ever appears in a config file, image, or repo.
3. **Single static binary.** Build matrix: `darwin/arm64` (velma) and
   `linux/amd64` (DOKS node). `CGO_ENABLED=0`.
4. **Public repo** (`github.com/kbadinger/guardian-sentinel`). No personal data;
   private-network defaults documented in examples are fine, tokens never.

## Probe types

| type | config fields | success criteria |
|------|--------------|------------------|
| `http` | `url`, `expect` (body substring), `warn_cert_days` (optional, default 14), `timeout` | status 200 AND body contains `expect`; if HTTPS and cert expires within `warn_cert_days`, emit a WARN alert (does not mark DOWN). Per Kevin's rule, `expect` is REQUIRED — a bare 200 is never proof the right app answered. |
| `mcp` | `url`, `token_env`, `expect` (default `guardian-memory`) | JSON-RPC `initialize` handshake (headers: Authorization from `token_env`, `Content-Type: application/json`, `Accept: application/json, text/event-stream`); response body contains `expect`. Exact probe the proven `guardian-health-check.sh` hook uses; catches up-but-dropping-responses (the 2026-07-23 MCP incident), which a port check can't. |
| `backup` | `dir`, `glob` (e.g. `guardian-*`), `file` (e.g. `guardian.db.gz`), `max_age` | newest matching subdir contains `file` with mtime < `max_age`. Watchman-for-the-watchman: the existing 04:00 restore-verify alerts when a restore FAILS; nothing alerts when backup jobs silently stop RUNNING. |
| `heartbeat` (receiver) | `listen` (`:8080`), `token_env`, `max_silence` (e.g. `15m`), `peer_name` | runs a tiny `net/http` handler; a valid tokened POST resets the timer; silence past `max_silence` → DOWN alert "`peer_name` is down"; next valid POST → RECOVERY. |
| `heartbeat_send` | `url`, `token_env`, `interval` (e.g. `5m`), `fail_threshold` | POSTs to the receiver; `fail_threshold` consecutive send failures → WARN "cloud monitor unreachable" (WARN not DOWN — the sites may be fine). |

## Verified facts baked into the configs (all verified 2026-07-28)

**Internal config (velma — probes hit localhost, deliberately independent of Tailscale):**
1. API — GET `http://127.0.0.1:8002/health`, expect `"status":"healthy"`
   (`backend/api/routes/health.py`, no `/api` prefix, returns 503 when unhealthy).
2. Web — GET `http://127.0.0.1:3100/`, expect marker (🔴 builder curls the live
   page at deploy and picks a real marker string).
3. MCP — `http://127.0.0.1:8765/mcp` handshake as above.
4. Backup — dir `/Users/velma/projects/guardian/backups`, glob `guardian-*`,
   file `guardian.db.gz`, max_age `26h`. Verified layout: nightly 03:30 job
   writes `guardian-<YYYYMMDD-HHMMSS>/guardian.db.gz`; NOTE `~/guardian-backups`
   is a DIFFERENT dir (old worktree tarballs) — wrong target.
5. heartbeat_send → DOKS node, every 5m.

**Fleet config (DOKS):** one `http` probe per live surface — thisweekeats.com,
app.penforge.io, driftnightly.com, kevinbadinger.com, ReadReceipt (🔴 confirm
URL), eRail TrackTrace (🔴 confirm URL — runs on DigitalOcean), v1.qtown.ai —
each with a real content marker (🔴 verify each at build time) and cert-expiry
warnings; plus the `heartbeat` receiver watching velma. Growth = append a JSON
object; no code change.

## Monitor loop & state machine (unchanged from v1 except config-driven)

- Tick every `interval` (default 60s, per-config override); probes run
  concurrently (goroutine per probe, `sync.WaitGroup`), each with its own
  `context.WithTimeout` (default 10s).
- Per-probe consecutive-failure counter. UP→DOWN at `fail_threshold` (default 3)
  → DOWN alert. While DOWN: re-alert every `realert_cooldown` (default 6h).
  DOWN→UP on first success → RECOVERY alert with outage duration. WARN-class
  events (cert expiry, heartbeat-send failures) alert once per cooldown, never
  mark DOWN.
- Startup: one probe round + a single summary message ("sentinel[fleet] online —
  7/7 ✓"), so a dead sentinel is noticeable by silence.
- State machine lives in `internal/monitor` with NO real I/O — probe results in,
  "alerts to send" out. Centerpiece of the table-driven tests.

## Alerting

Telegram `sendMessage` via HTTPS POST, reusing Guardian's existing bot
(`TELEGRAM_BOT_TOKEN` + `TELEGRAM_CHAT_ID` — chat id = Kevin's user id, same
value as Guardian's `TELEGRAM_ALLOWED_USER_ID`). Messages are plain text, one
glance: `🚨 sentinel[fleet]: thisweekeats.com DOWN — 3 consecutive failures
(connection refused). 12:41 ET` / `✅ sentinel[fleet]: thisweekeats.com RECOVERED
after 14m` / `⚠️ sentinel[fleet]: cert for driftnightly.com expires in 9 days`.
Send failures: log and continue — never crash the loop. `SENTINEL_DRY_RUN=1`
prints instead of sending.

## Repo layout

```
cmd/guardian-sentinel/main.go     — config load, wiring, run loop
internal/config/config.go         — JSON schema, validation (unknown type = error)
internal/probe/{probe,http,mcp,backup,heartbeat}.go
internal/monitor/monitor.go       — scheduler + state machine (pure logic)
internal/alert/telegram.go
deploy/                           — launchd plist, wrapper, env example (velma)
deploy/k8s/                       — Deployment/Service/ConfigMap/Secret templates (DOKS)
deploy/internal-config.example.json
deploy/fleet-config.example.json
.github/workflows/ci.yml          — fmt/vet/test -race/staticcheck
.github/workflows/deploy.yml.example — build → DO registry → rollout (enable at M7)
```

## Testing (table-driven, no network)

Everything from v1 (monitor state machine: blip/threshold/cooldown/re-alert/
recovery/independence; probes via `httptest` + `t.TempDir()`; alerter against a
fake Telegram endpoint) PLUS: config parsing (valid, unknown probe type, missing
required field, secret-by-env-name resolution), heartbeat receiver state machine
(fresh → silent → alert → resume), cert-expiry evaluation against a locally
generated short-lived test cert. `go test -race ./...` required. No test touches
the real network, velma, DO, or Telegram.

## CI / CD

- **CI (from M0):** gofmt clean, `go vet`, `go test -race`, staticcheck (pinned
  `2025.1.1` — 🔴 verify ≥7 days old + toolchain support per house supply-chain
  rule). Actions pinned to long-established tags (`actions/checkout@v4`,
  `actions/setup-go@v5`).
- **CD (at M7):** GitHub Action on push to main — `docker build` (multi-stage,
  `FROM scratch` + ca-certificates for the final image), push to the DO container
  registry (included with DOKS), `kubectl set image` with a scoped kubeconfig
  stored as an encrypted repo secret → rolling update, `kubectl rollout status`
  gate. This pipeline IS the resume line — keep it boring and standard.

## Deployment A — velma (launchd, internal mode)

Unchanged from v1: velma has no Go (verified) → `brew install go` (mature
package), clone to `/Users/velma/projects/guardian-sentinel`, build, env file
`~/.guardian-sentinel.env` (chmod 600) from `deploy/guardian-sentinel.env.example`,
install `deploy/com.local.guardian-sentinel.plist` (KeepAlive, logs to
`~/Library/Logs/guardian-sentinel.log`). Fallback: laptop build + scp (same
darwin/arm64). **Live-fire acceptance:** stop guardian-web, see DOWN on Telegram
within ~3 intervals, restart, see RECOVERY (screenshot → README). Web is the safe
victim — never live-fire API or MCP (agents may be mid-write). 🔴 Kevin approves
the ~2-min window.

## Deployment B — DOKS (fleet mode)

1. **Cluster:** DO console (or `doctl`) → Kubernetes → 1 node pool × 1 Basic
   node ($12/mo; control plane free — verified in the 2026-07-23 K8s plan).
   🔴 Confirm the eRail DigitalOcean account takes the billing.
2. **Registry:** DO Container Registry (included) — `doctl registry login`,
   tag + push `guardian-sentinel:v1` (linux/amd64).
3. **Manifests** (`deploy/k8s/`): Deployment (1 replica, requests 50m/64Mi,
   limits 200m/128Mi — it's a tiny prober), ConfigMap from
   `fleet-config.example.json`, Secret (Telegram token/chat id + heartbeat
   token), Service `type: NodePort` exposing ONLY the heartbeat port on the
   node's public IP. **No LoadBalancer** — probes are outbound; skipping the LB
   saves $12/mo and shrinks attack surface to one tokened endpoint.
4. **Liveness:** k8s restarts the pod on hang (its own liveness probe = the
   process's health); cluster-wide death is covered by "Telegram went quiet" +
   velma's heartbeat-send WARN. Accepted residual risk, documented.
5. **Kill switch:** delete cluster + registry; total exposure $12/mo while alive.

## Milestones (~12h; M0–M5 = the standalone 8h velma v1)

| # | Time | Deliverable |
|---|------|-------------|
| M0 | 0.5h | Repo builds; CI green on empty test |
| M1 | 1.5h | Go essentials pass; `internal/config` (JSON schema + validation, table-driven tests) — config-driven from the start, no later refactor |
| M2 | 2.0h | http/mcp/backup probes + tests (httptest, tempdir, test cert for expiry) |
| M3 | 1.5h | Monitor loop + state machine + full test table |
| M4 | 1.0h | Telegram alerter + dry-run; laptop dry-run against velma URLs |
| M5 | 1.5h | Velma deploy (launchd) + live-fire + README — **v1 DONE, stands alone** |
| M6 | 1.5h | Heartbeat pair (receiver + sender) + tests; internal config gains heartbeat_send |
| M7 | 2.0h | DOKS: cluster, registry, manifests, CD pipeline, fleet config live |
| M8 | 0.5h | Dead-man live-fire: stop velma sentinel → cloud alerts "velma is down" → restart → recovery. Screenshot → README |

## Definition of done

v1 (M5): public repo, CI green, running under launchd on velma, live-fire
evidence in README. v2 (M8): fleet instance in production on DOKS deployed via
CI/CD, all 7 sites monitored with real-content markers + cert warnings, dead-man
proven both directions, README shows the architecture diagram + both live-fire
screenshots, task `9c4f6f61` closed with actual_minutes. THEN (separate):
JobForge resume/profile update with the new truthful claims; blog-post task in
`[blog]` ("my uptime monitor watches my AI's brain — and they watch each other").

## Non-goals

No dashboard/UI in v1 (Telegram + logs; a dashboard can be cluster tenant #2
later). No Prometheus/metrics endpoint, no LoadBalancer, no multi-cluster, no
service mesh, no autoscaling (1 replica is correct for a prober), no probing FROM
velma of public sites (that's the fleet instance's job), no restarting/healing of
anything watched, no third-party Go modules, Guardian never on k8s.

## 🔴 Open questions for Kevin

1. **License** — MIT recommended; confirm before repo goes public.
2. **Repo visibility timing** — public from first push, or at M5?
3. **Live-fire windows** — OK to bounce guardian-web ~2 min (M5) and the velma
   sentinel ~15 min (M8)?
4. **DigitalOcean billing** — confirm the eRail DO account carries the $12/mo.
5. **URLs to confirm** — ReadReceipt and eRail TrackTrace public URLs.
6. ~~K8s stretch?~~ Superseded — Kubernetes is now core scope (Deployment B).
