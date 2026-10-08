#!/usr/bin/env bash
# The one entry point for the real-tool harness (D-07).
#
#   ./harness/run.sh                      run every harness test in compare mode
#   ./harness/run.sh -run TestHarnessX    pass any go test flag through
#   HARNESS_RECORD=1 ./harness/run.sh     re-record the fixtures from the real tools
#
# It reads harness/.env (your registry host and project, values only, gitignored),
# harness/versions.env (the tool pins) and harness/images.lock (what
# harness/images.sh built or pushed). It resolves the runner and Puppet Server 9
# images (by digest when the lock has one, otherwise the local tag, building the
# runner with images.sh when it is missing), starts the containers, runs the
# harness-tagged Go tests against them, and always removes the containers and
# volumes on the way out. It never builds the Puppet Server 9 image: that needs
# your Puppet Core credential and is a human step (harness/images.sh build server).
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
# The lock is read first so a tag in versions.env always wins over a recorded one.
if [ -f harness/images.lock ]; then
  # shellcheck disable=SC1091
  . harness/images.lock
fi
# shellcheck disable=SC1091
. harness/versions.env
set +a

if [ -z "${STAGEHAND_HARBOR_HOST:-}" ] || [ -z "${STAGEHAND_HARBOR_PROJECT:-}" ]; then
  echo "STAGEHAND_HARBOR_HOST and STAGEHAND_HARBOR_PROJECT must both be set in harness/.env (no default, D-18)." >&2
  exit 2
fi
REG="${STAGEHAND_HARBOR_HOST}/${STAGEHAND_HARBOR_PROJECT}"

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

# --- Runner image: by digest when the lock has one, else the local tag ---
if [ -n "${RUNNER_DIGEST:-}" ]; then
  RUNNER_IMAGE_REF="${REG}/${RUNNER_IMAGE_NAME}@${RUNNER_DIGEST}"
  export RUNNER_IMAGE_REF
  if ! docker compose -f harness/compose.yaml pull runner; then
    echo "Could not pull the runner image by digest (are you logged in to the registry?)." >&2
    echo "Falling back to a local build from the pinned recipe." >&2
    RUNNER_IMAGE_REF="${REG}/${RUNNER_IMAGE_NAME}:${RUNNER_TAG}"
    export RUNNER_IMAGE_REF
  fi
else
  RUNNER_IMAGE_REF="${REG}/${RUNNER_IMAGE_NAME}:${RUNNER_TAG}"
  export RUNNER_IMAGE_REF
fi
if [ "${RUNNER_IMAGE_REF#*@}" = "${RUNNER_IMAGE_REF}" ]; then
  if ! docker image inspect "$RUNNER_IMAGE_REF" >/dev/null 2>&1; then
    ./harness/images.sh build runner
  fi
fi

# --- Puppet Server 9 image: only used when it is resolvable ---
# The reference is always exported because compose interpolates every service,
# including the ones in an inactive profile. The profile and HARNESS_SERVER are
# set only when the image really is there.
SERVER_TAG_REF="${REG}/${SERVER_IMAGE_NAME:-stagehand-harness-server}:${SERVER_TAG:-unset}"
SERVER_IMAGE_REF="$SERVER_TAG_REF"
SERVER_AVAILABLE=0
export SERVER_IMAGE_REF
if [ -n "${SERVER_DIGEST:-}" ]; then
  SERVER_IMAGE_REF="${REG}/${SERVER_IMAGE_NAME:-stagehand-harness-server}@${SERVER_DIGEST}"
  export SERVER_IMAGE_REF
  if docker compose -f harness/compose.yaml --profile server pull puppetserver; then
    SERVER_AVAILABLE=1
  else
    echo "Could not pull the Puppet Server 9 image by digest (are you logged in to the registry?)." >&2
    SERVER_IMAGE_REF="$SERVER_TAG_REF"
    export SERVER_IMAGE_REF
  fi
fi
if [ "$SERVER_AVAILABLE" = 0 ] && docker image inspect "$SERVER_IMAGE_REF" >/dev/null 2>&1; then
  SERVER_AVAILABLE=1
fi
if [ "$SERVER_AVAILABLE" = 1 ]; then
  # Platform to run the server on: the shell's DOCKER_DEFAULT_PLATFORM when the
  # image has it, else the native one, else the first recorded platform.
  case "$(uname -m)" in
    arm64 | aarch64) native_platform="linux/arm64" ;;
    *) native_platform="linux/amd64" ;;
  esac
  SERVER_PLATFORM=""
  for want in "${DOCKER_DEFAULT_PLATFORM:-}" "$native_platform"; do
    [ -n "$want" ] || continue
    case ",${SERVER_PLATFORMS:-}," in
      *",${want},"*) SERVER_PLATFORM="$want"; break ;;
    esac
  done
  if [ -z "$SERVER_PLATFORM" ]; then
    SERVER_PLATFORM="${SERVER_PLATFORMS:-}"
    SERVER_PLATFORM="${SERVER_PLATFORM%%,*}"
  fi
  export SERVER_PLATFORM
  export COMPOSE_PROFILES=server
  export HARNESS_SERVER=1
else
  echo "Puppet Server 9 image not available: server scenarios cannot run (see docs/harness.md)" >&2
fi

trap 'docker compose -f harness/compose.yaml down -v --remove-orphans' EXIT

docker compose -f harness/compose.yaml up --wait

go vet -tags harness ./harness/
go test -tags harness ./harness/ -count=1 -timeout 15m "$@"
