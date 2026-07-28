# guardian-sentinel

A small, dependency-free Go daemon that watches my self-hosted AI memory system
([Guardian](https://github.com/kbadinger)) from the inside: every minute it
probes the API, web UI, MCP server, and nightly-backup freshness on the box they
run on, and sends me a Telegram alert on failure — and a recovery notice when
things come back.

**Status: spec complete, build not started.** The full build spec — probes,
state machine, config, testing, deployment, milestones — is in [SPEC.md](SPEC.md).

- Go stdlib only, zero third-party dependencies
- Single static binary, runs 24/7 under launchd on an always-on Mac mini
- Read-only by design: it observes, it never touches the system it watches
