# guardian-sentinel — Build Spec v2.1

*v2.1 (2026-07-28): heartbeat goes multi-peer + HMAC-signed; Windows box agent
added as Deployment C (uptime/reboot, internet speed, CPU temp, disk); k8s
livenessProbe; registry→cluster pull auth; `time/tzdata` embed; receiver clock
semantics defined.*

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

**Time budget:** ~12 hours core (v2), structured so the first 8 (velma v1) stand
alone; + ~2.5h for the Windows agent (v2.1, post-M8).

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
- **Agents are open-ended (v2.1).** The heartbeat receiver tracks a *peers map*,
  so adding a machine = the same binary + a config file + one peer entry on the
  receiver. First new agent: Kevin's Windows box (uptime/reboot, internet speed,
  CPU temp, disk free). Growth is a config append, never a code change.
- **Dead-man's switch, both directions:** every on-prem agent POSTs a heartbeat
  to the DOKS instance; if a peer's heartbeats go silent past its threshold, DOKS
  alerts "`<peer>` is down" — the one failure no on-box watcher can report.
  Symmetrically, if an agent's heartbeat POSTs fail repeatedly, that agent alerts
  "cloud monitor unreachable". No inbound access anywhere beyond one
  HMAC-authenticated NodePort endpoint on the DOKS node. (Known wrinkle,
  accepted: the DOKS node's public IP is not stable across node
  replacement/upgrades — a changed IP shows up as the sender WARN, fix is a
  config update; 🔴 verify at M7 that DO's default firewall exposes NodePort
  30080 publicly.)
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
3. **Single static binary.** Build matrix: `darwin/arm64` (velma),
   `linux/amd64` (DOKS node), `windows/amd64` (Windows box, v2.1).
   `CGO_ENABLED=0`. Import `_ "time/tzdata"` — the `FROM scratch` image and
   Windows ship no zoneinfo, and without the embed the ET timestamps in alerts
   silently fall back to UTC or error.
4. **Public repo** (`github.com/kbadinger/guardian-sentinel`). No personal data;
   private-network defaults documented in examples are fine, tokens never.

## Probe types

| type | config fields | success criteria |
|------|--------------|------------------|
| `http` | `url`, `expect` (body substring), `warn_cert_days` (optional, default 14), `timeout` | status 200 AND body contains `expect`; if HTTPS and cert expires within `warn_cert_days`, emit a WARN alert (does not mark DOWN). Per Kevin's rule, `expect` is REQUIRED — a bare 200 is never proof the right app answered. |
| `mcp` | `url`, `token_env`, `expect` (default `guardian-memory`) | JSON-RPC `initialize` handshake (headers: Authorization from `token_env`, `Content-Type: application/json`, `Accept: application/json, text/event-stream`); response body contains `expect`. Exact probe the proven `guardian-health-check.sh` hook uses; catches up-but-dropping-responses (the 2026-07-23 MCP incident), which a port check can't. |
| `backup` | `dir`, `glob` (e.g. `guardian-*`), `file` (e.g. `guardian.db.gz`), `max_age` | newest matching subdir contains `file` with mtime < `max_age`. Watchman-for-the-watchman: the existing 04:00 restore-verify alerts when a restore FAILS; nothing alerts when backup jobs silently stop RUNNING. |
| `heartbeat` (receiver) | `listen` (`:8080`), `token_env`, `peers` (list of `{name, max_silence}`) | runs a tiny `net/http` handler with a **per-peer last-seen map**. POST body `{"instance": "...", "uptime_s": N}`, authenticated by HMAC-SHA256 over timestamp+body (`crypto/hmac`, key from `token_env`, ±5 min timestamp window, `crypto/subtle` constant-time compare — a static bearer token over plain HTTP would be sniffable and replayable, defeating the dead-man's switch). Unknown instance → rejected. Silence past that peer's `max_silence` → DOWN "`<name>` is down"; next valid POST → RECOVERY; `uptime_s` drop → INFO "`<name>` rebooted". Timers count from **process start** (every restart, incl. rolling deploys, resets them — documented). Also serves `GET /healthz` for the k8s livenessProbe. |
| `heartbeat_send` | `url`, `token_env`, `interval` (e.g. `5m`), `fail_threshold` | signs POSTs as above, includes the host's uptime; `fail_threshold` consecutive send failures → WARN "cloud monitor unreachable" (WARN not DOWN — the sites may be fine). |
| `exec` (v2.1) | `cmd` (argv array), `parse` (regex with one numeric capture group), `warn_below` / `warn_above`, `interval` (optional) | runs the command via `os/exec` (stdlib); exit 0 and captured value inside bounds = OK, otherwise WARN (never DOWN). Powers Windows CPU temp and disk-free via PowerShell one-liners; generic enough for any future scalar check. |
| `speedtest` (v2.1) | `url` (large test payload, e.g. Cloudflare's `speed.cloudflare.com/__down?bytes=…`), `min_mbps`, `interval` (e.g. `30m`) | timed HTTP download via `net/http`, Mbps = bytes/elapsed; below `min_mbps` → WARN. Runs on its own interval, never the global tick — a bandwidth test every 60s would be self-inflicted congestion. |

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
  `context.WithTimeout` (default 10s). Probes that declare their own `interval`
  (`speedtest`, `heartbeat_send`, optionally `exec`) run on their own schedule,
  not the global tick.
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
internal/probe/{probe,http,mcp,backup,heartbeat,exec,speedtest}.go
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
(fresh → silent → alert → resume, now per-peer: one silent peer must not mask
another), HMAC sign/verify round-trip + stale-timestamp rejection, cert-expiry
evaluation against a locally generated short-lived test cert, `exec` against a
stub script, `speedtest` against an `httptest` server serving N bytes.
`go test -race ./...` required. No test touches the real network, velma, DO,
or Telegram.

## CI / CD

- **CI (from M0):** gofmt clean, `go vet`, `go test -race`, staticcheck (pinned
  `2025.1.1` — 🔴 verify ≥7 days old + toolchain support per house supply-chain
  rule). Actions pinned to long-established tags (`actions/checkout@v4`,
  `actions/setup-go@v5`).
- **CD (at M7):** GitHub Action on push to main — `docker build` (multi-stage,
  `FROM scratch` + ca-certificates for the final image), push to the DO container
  registry (included with DOKS), `kubectl set image` with a scoped kubeconfig
  stored as an encrypted repo secret → rolling update, `kubectl rollout status`
  gate. DO registry auth quirk: the API token is BOTH username and password for
  `docker login` — a placeholder username fails. This pipeline IS the resume
  line — keep it boring and standard.

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
   tag + push `guardian-sentinel:v1` (linux/amd64), then connect it to the
   cluster (`doctl kubernetes cluster registry add` or the console checkbox) so
   the pod can pull — skipping this = ImagePullBackOff on first deploy.
3. **Manifests** (`deploy/k8s/`): Deployment (1 replica, requests 50m/64Mi,
   limits 200m/128Mi — it's a tiny prober), ConfigMap from
   `fleet-config.example.json`, Secret (Telegram token/chat id + heartbeat
   token), Service `type: NodePort` exposing ONLY the heartbeat port on the
   node's public IP. **No LoadBalancer** — probes are outbound; skipping the LB
   saves $12/mo and shrinks attack surface to one authenticated endpoint.
4. **Liveness:** an explicit `livenessProbe` (`GET /healthz` on the heartbeat
   port — the server is already listening there) so k8s restarts the pod on
   hang, not just crash; cluster-wide death is covered by "Telegram went quiet"
   + every agent's heartbeat-send WARN. Accepted residual risk, documented.
5. **Kill switch:** delete cluster + registry; total exposure $12/mo while alive.

## Deployment C — Windows box agent (v2.1, after M8)

1. **Build:** `GOOS=windows GOARCH=amd64` — same binary, third target in the
   matrix. No Windows-specific code beyond what `exec` already provides.
2. **Run:** Task Scheduler, "At startup", restart-on-failure — built into
   Windows and keeps the zero-dependency rule (a real Windows *service* would
   need `golang.org/x/sys`; NSSM is the optional upgrade if Kevin wants one).
   Secrets via a user-level env file read the same way as velma's.
3. **Config** (`deploy/windows-config.example.json`), instance `winbox`:
   - `heartbeat_send` → the same DOKS receiver (new peer entry `winbox`);
     carries uptime, so the receiver reports reboots for free, and the startup
     summary ("sentinel[winbox] online — uptime 3m") doubles as a boot notice.
   - `speedtest` every 30m against Cloudflare's test endpoint, WARN below
     `min_mbps`.
   - `exec` CPU temp: PowerShell `Get-CimInstance MSAcpi_ThermalZoneTemperature`
     (converted to °C in the command). ⚠️ Motherboard-dependent — many consumer
     boards don't populate the ACPI thermal zone; 🔴 verify on the target box
     BEFORE promising this probe, else fall back to LibreHardwareMonitor's local
     JSON endpoint via a plain `http` probe.
   - `exec` disk free (GB on C:), WARN below threshold.
4. **Monitoring the box itself** (uptime, "is it down") is the dead-man's
   switch doing its job — no extra probe needed.

## Milestones (~12h; M0–M5 = the standalone 8h velma v1)

| # | Time | Deliverable |
|---|------|-------------|
| M0 | 0.5h | Repo builds; CI green on empty test |
| M1 | 1.5h | Go essentials pass; `internal/config` (JSON schema + validation, table-driven tests) — config-driven from the start, no later refactor |
| M2 | 2.0h | http/mcp/backup probes + tests (httptest, tempdir, test cert for expiry) |
| M3 | 1.5h | Monitor loop + state machine + full test table |
| M4 | 1.0h | Telegram alerter + dry-run; laptop dry-run against velma URLs |
| M5 | 1.5h | Velma deploy (launchd) + live-fire + README — **v1 DONE, stands alone** |
| M6 | 1.5h | Heartbeat pair: multi-peer receiver (peers map, HMAC, /healthz, reboot detect) + signed sender + tests; internal config gains heartbeat_send |
| M7 | 2.0h | DOKS: cluster, registry (+ cluster pull auth), manifests, CD pipeline, fleet config live |
| M8 | 0.5h | Dead-man live-fire: stop velma sentinel → cloud alerts "velma is down" → restart → recovery. Screenshot → README |
| M9 | 2.5h | **v2.1 Windows agent:** exec + speedtest probes + tests; windows/amd64 build; Task Scheduler deploy; `winbox` peer live on the receiver |

## Definition of done

v1 (M5): public repo, CI green, running under launchd on velma, live-fire
evidence in README. v2 (M8): fleet instance in production on DOKS deployed via
CI/CD, all 7 sites monitored with real-content markers + cert warnings, dead-man
proven both directions, README shows the architecture diagram + both live-fire
screenshots, task `9c4f6f61` closed with actual_minutes. v2.1 (M9): winbox agent
reporting via the same receiver — net speed, disk, temp (if the box supports
it), reboot notices — proven by pulling its network cable for 20 minutes. THEN (separate):
JobForge resume/profile update with the new truthful claims; blog-post task in
`[blog]` ("my uptime monitor watches my AI's brain — and they watch each other").

## Non-goals

No dashboard/UI in v1 (Telegram + logs; a dashboard can be cluster tenant #2
later). No Prometheus/metrics endpoint, no LoadBalancer, no multi-cluster, no
service mesh, no autoscaling (1 replica is correct for a prober), no probing FROM
velma of public sites (that's the fleet instance's job), no restarting/healing of
anything watched, no third-party Go modules (incl. no `x/sys` — Task Scheduler
over a Windows service wrapper), Guardian never on k8s.

## 🔴 Open questions for Kevin

1. **License** — MIT recommended; confirm before repo goes public.
2. **Repo visibility timing** — public from first push, or at M5?
3. **Live-fire windows** — OK to bounce guardian-web ~2 min (M5) and the velma
   sentinel ~15 min (M8)?
4. **DigitalOcean billing** — confirm the eRail DO account carries the $12/mo.
5. **URLs to confirm** — ReadReceipt and eRail TrackTrace public URLs.
6. ~~K8s stretch?~~ Superseded — Kubernetes is now core scope (Deployment B).
7. **Windows box (v2.1)** — which machine (hostname), thresholds (min Mbps —
   what's the plan speed?, max CPU °C, min free GB on C:), and whether its ACPI
   thermal zone actually reports temp (verify before building the probe).
