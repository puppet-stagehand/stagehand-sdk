#!/usr/bin/env bash
# The one entry point for the real-tool harness (D-07).
#
#   ./harness/run.sh                      run every harness test in compare mode
#   ./harness/run.sh -run TestHarnessX    pass any go test flag through
#   HARNESS_RECORD=1 ./harness/run.sh     re-record the fixtures from the real tools
#
# It reads harness/.env (your registry host and project, values only, gitignored)
# and harness/versions.env (the tool pins), builds and starts the containers,
# runs the harness-tagged Go tests against them, and always removes the
# containers and volumes on the way out.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd -P)"

if [ ! -f harness/.env ]; then
  echo "harness/.env is missing. Copy harness/.env.example to harness/.env and fill in" >&2
  echo "your registry host and project (values only, never credentials)." >&2
  exit 2
fi

set -a
# shellcheck disable=SC1091
. harness/.env
# shellcheck disable=SC1091
. harness/versions.env
set +a

# One compose project per checkout, so parallel executors or worktrees never
# share containers or volumes.
if [ -z "${HARNESS_PROJECT:-}" ]; then
  if command -v shasum >/dev/null 2>&1; then
    hash="$(printf '%s' "$ROOT" | shasum -a 256 | cut -c1-8)"
  else
    hash="$(printf '%s' "$ROOT" | sha256sum | cut -c1-8)"
  fi
  HARNESS_PROJECT="stagehand-harness-${hash}"
fi
export HARNESS_PROJECT
export COMPOSE_PROJECT_NAME="$HARNESS_PROJECT"

trap 'docker compose -f harness/compose.yaml down -v --remove-orphans' EXIT

docker compose -f harness/compose.yaml up --build --wait

go vet -tags harness ./harness/
go test -tags harness ./harness/ -count=1 -timeout 15m "$@"
