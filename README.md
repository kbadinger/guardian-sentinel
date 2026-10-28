# guardian-sentinel

One small, dependency-free Go monitoring service, deployed twice:

- **On the box** (a Mac mini under launchd): watches my self-hosted AI memory
  system from the inside — API, web UI, MCP server, nightly-backup freshness.
- **In the cloud** (a 1-node DigitalOcean Kubernetes cluster): watches every
  live site I've shipped from the real internet — content checks, cert-expiry
  warnings — deployed via a registry + CI/CD rolling-update pipeline.

The two instances cover each other with a **dead-man's switch**: the on-prem
instance heartbeats the cloud one, so "the whole box is down" — the one failure
no on-box watcher can report — still pages my phone. All alerts via Telegram.

![guardian-sentinel architecture](docs/architecture.svg)

**Status:** the cloud (fleet) instance is built and tested: HTTP content checks, cert-expiry warnings, the alert state machine, Telegram alerts, and the signed heartbeat receiver with a `/healthz` that fails when the monitor loop stalls. It runs in production on a 1-node DigitalOcean Kubernetes cluster: every push to main that passes CI is built, pushed to the registry and rolled out as a rolling update, and Kubernetes restarts the watchdog if it crashes or its loop stalls. The on-box instance and the Windows agent come next. The full build spec is in [SPEC.md](SPEC.md).

- Go stdlib only, zero third-party dependencies; single static binary
- Config-driven probes (JSON): same binary, different config per deployment
- Read-only by design: it observes, it never touches what it watches

## Daily spend report (optional)

Add a `spend` block to the config and the instance sends one Telegram message a
day at `report_at` (America/New_York): yesterday's spend and the month to date
for each billing provider, biggest first, with the model or service that cost
the most. It only reads billing data; it never changes a limit or pauses
anything. Supported now: the Anthropic API cost report (needs an Admin API key)
and Vercel billing charges (a token whose role can read billing). Keys come
from the environment variables named in `key_env`, never from the config file.
`untracked` lists providers you pay for that aren't read yet, so the report
says so. `SENTINEL_SPEND_ONCE=1` sends one report now and exits (add
`SENTINEL_DRY_RUN=1` to print it). See `deploy/fleet-config.example.json`.
