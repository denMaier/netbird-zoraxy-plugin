#!/bin/sh
# Run the sync against a real NetBird server. Needs Docker.
set -eu
cd "$(dirname "$0")"
export NETBIRD_E2E_PORT="${NETBIRD_E2E_PORT:-18080}"
trap 'docker compose --profile setup down -v >/dev/null 2>&1' EXIT
docker compose up -d --quiet-pull server
NETBIRD_E2E_TOKEN=$(docker compose --profile setup run --rm -T setup)
export NETBIRD_E2E_TOKEN
export NETBIRD_E2E_URL="http://127.0.0.1:$NETBIRD_E2E_PORT"
(cd .. && go test -count=1 -run '^TestRealNetBird$' -v .)
