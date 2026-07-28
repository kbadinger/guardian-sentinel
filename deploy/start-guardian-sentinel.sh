#!/usr/bin/env bash
# launchd wrapper: load secrets from the gitignored env file, then exec the binary.
set -euo pipefail

ENV_FILE="$HOME/.guardian-sentinel.env"
BINARY="$HOME/bin/guardian-sentinel"

if [ ! -f "$ENV_FILE" ]; then
    echo "$(date '+%F %T') FATAL: $ENV_FILE missing — copy deploy/guardian-sentinel.env.example and fill in secrets" >&2
    exit 1
fi

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

exec "$BINARY"
