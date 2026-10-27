#!/usr/bin/env bash
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"
docker info >/dev/null
if [[ -z "${POSTGRES_PASSWORD:-}" ]]; then
 read -r -s -p 'PostgreSQL password (reuse existing database password): ' POSTGRES_PASSWORD
 printf '\n'
fi
[[ -n "$POSTGRES_PASSWORD" ]] || { printf 'Password cannot be empty\n' >&2; exit 1; }
export POSTGRES_PASSWORD
bash scripts/build.sh
docker compose -p agenteval-runtime -f deployments/compose.postgres.yml up -d --wait --wait-timeout 90
encoded_password="$(printf '%s' "$POSTGRES_PASSWORD" | od -An -v -tx1 | tr -d ' \n' | sed 's/../%&/g')"
export AGENTEVAL_DATABASE_URL="postgres://agenteval:$encoded_password@127.0.0.1:5432/agenteval?sslmode=disable"
./bin/runtime prepare --project . --config configs/local.json
exec ./bin/runtime serve --project . --config configs/local.json "$@"
