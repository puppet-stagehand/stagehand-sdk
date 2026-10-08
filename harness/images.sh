#!/usr/bin/env bash
# Build, record and (only when a human asks) push the two harness images (D-18).
#
#   ./harness/images.sh build runner     build the runner image locally, rewrite images.lock
#   ./harness/images.sh build server     build Puppet Server 9 locally (needs your Puppet Core
#                                        login in PUPPET_CORE_USER and PUPPET_CORE_API_KEY)
#   ./harness/images.sh push --confirm   push both images to your private Harbor project
#
# The registry host and project come only from harness/.env (values, never
# credentials). There is no default. The Puppet Core credential reaches the build
# only as a BuildKit secret mount: it is never a build argument, never an
# environment line in the image, and never printed. This script never runs
# "docker login"; log in yourself.
#
# harness/images.lock records tags, platforms, digests and the Puppet Server
# version. It never records the registry host or project.
set -euo pipefail
# Nothing below may ever trace: a trace would print secret-carrying commands.
set +x

cd "$(dirname "$0")/.."

LOCK="harness/images.lock"

die() {
  echo "$1" >&2
  exit "${2:-1}"
}

if [ ! -f harness/.env ]; then
  die "harness/.env is missing. Copy harness/.env.example to harness/.env and fill in your registry host and project (values only, never credentials)." 2
fi

set -a
# shellcheck disable=SC1091
. harness/.env
# shellcheck disable=SC1091
. harness/versions.env
set +a

if [ -z "${STAGEHAND_HARBOR_HOST:-}" ]; then
  die "STAGEHAND_HARBOR_HOST is empty. Set it in harness/.env (no default, D-18)." 2
fi
if [ -z "${STAGEHAND_HARBOR_PROJECT:-}" ]; then
  die "STAGEHAND_HARBOR_PROJECT is empty. Set it in harness/.env (no default, D-18)." 2
fi

REG="${STAGEHAND_HARBOR_HOST}/${STAGEHAND_HARBOR_PROJECT}"
RUNNER_REF="${REG}/${RUNNER_IMAGE_NAME}:${RUNNER_TAG}"
SERVER_IMAGE_NAME="${SERVER_IMAGE_NAME:-}"
SERVER_TAG="${SERVER_TAG:-}"
SERVER_REF="${REG}/${SERVER_IMAGE_NAME}:${SERVER_TAG}"

# Build and push output can echo the image name. Show a placeholder instead so
# a pasted log never carries the registry host.
redact() {
  sed -e "s|${STAGEHAND_HARBOR_HOST}|<registry-host>|g"
}

native_platform() {
  case "$(uname -m)" in
    arm64 | aarch64) echo "linux/arm64" ;;
    x86_64 | amd64) echo "linux/amd64" ;;
    *) die "unsupported host architecture: $(uname -m)" 1 ;;
  esac
}

# ---------------------------------------------------------------- lock file --
# The lock is read with a loop, not sourced, so a value in it can never
# override a pin from versions.env.
L_RUNNER_TAG=""
L_RUNNER_PLATFORMS=""
L_RUNNER_DIGEST=""
L_SERVER_TAG=""
L_SERVER_PLATFORMS=""
L_SERVER_ARM64_NATIVE=""
L_SERVER_DIGEST=""
L_PUPPETSERVER_VERSION=""
L_UPDATED_AT=""

lock_load() {
  [ -f "$LOCK" ] || return 0
  local key value
  while IFS='=' read -r key value; do
    case "$key" in
      RUNNER_TAG | RUNNER_PLATFORMS | RUNNER_DIGEST | SERVER_TAG | SERVER_PLATFORMS | \
        SERVER_ARM64_NATIVE | SERVER_DIGEST | PUPPETSERVER_VERSION | UPDATED_AT)
        printf -v "L_${key}" '%s' "$value"
        ;;
    esac
  done <"$LOCK"
}

# lock_write rewrites the whole file through a temp file and a rename, so an
# interrupted run never leaves half a lock. It writes the tags from the current
# pins and never the registry host or project.
lock_write() {
  local tmp
  tmp="$(mktemp "${LOCK}.XXXXXX")"
  {
    echo "# Written by harness/images.sh. KEY=VALUE, shell-sourceable."
    echo "# An empty value means: not built or not pushed yet."
    echo "# This file never holds the registry host, the project or any credential."
    echo "RUNNER_TAG=${RUNNER_TAG}"
    echo "RUNNER_PLATFORMS=${L_RUNNER_PLATFORMS}"
    echo "RUNNER_DIGEST=${L_RUNNER_DIGEST}"
    echo "SERVER_TAG=${SERVER_TAG}"
    echo "SERVER_PLATFORMS=${L_SERVER_PLATFORMS}"
    echo "SERVER_ARM64_NATIVE=${L_SERVER_ARM64_NATIVE}"
    echo "SERVER_DIGEST=${L_SERVER_DIGEST}"
    echo "PUPPETSERVER_VERSION=${L_PUPPETSERVER_VERSION}"
    echo "UPDATED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"$tmp"
  chmod 0644 "$tmp"
  mv "$tmp" "$LOCK"
}

# A tag that changed since the last build means the recorded digest and version
# belong to an older image: drop them rather than keep a stale pin.
lock_reset_if_retagged() {
  if [ -n "$L_RUNNER_TAG" ] && [ "$L_RUNNER_TAG" != "$RUNNER_TAG" ]; then
    L_RUNNER_PLATFORMS=""
    L_RUNNER_DIGEST=""
  fi
  if [ -n "$L_SERVER_TAG" ] && [ "$L_SERVER_TAG" != "$SERVER_TAG" ]; then
    L_SERVER_PLATFORMS=""
    L_SERVER_ARM64_NATIVE=""
    L_SERVER_DIGEST=""
    L_PUPPETSERVER_VERSION=""
  fi
}

# ------------------------------------------------------------------ runner --
runner_build_args() {
  RUNNER_ARGS=(
    --build-arg "RUBY_IMAGE=${RUBY_IMAGE}"
    --build-arg "R10K_VERSION=${R10K_VERSION}"
    --build-arg "BOLT_VERSION=${BOLT_VERSION}"
    --build-arg "G10K_VERSION=${G10K_VERSION}"
    --build-arg "G10K_SHA256_LINUX_AMD64=${G10K_SHA256_LINUX_AMD64}"
    --build-arg "G10K_SHA256_LINUX_ARM64=${G10K_SHA256_LINUX_ARM64}"
    --build-arg "HTTP_REQUEST_VERSION=${HTTP_REQUEST_VERSION}"
  )
}

build_runner() {
  # The local image is built for the platform the harness will run it on:
  # HARNESS_BUILD_PLATFORM, else the shell's DOCKER_DEFAULT_PLATFORM (so an arm64
  # Mac that runs the harness under linux/amd64 emulation builds what it runs),
  # else the native architecture. The platform actually built is what the lock
  # records.
  local platform="${HARNESS_BUILD_PLATFORM:-${DOCKER_DEFAULT_PLATFORM:-$(native_platform)}}"
  runner_build_args
  echo "Building the runner image for ${platform} ..."
  docker buildx build --load --platform "$platform" \
    "${RUNNER_ARGS[@]}" -t "$RUNNER_REF" harness/runner 2>&1 | redact
  lock_load
  lock_reset_if_retagged
  L_RUNNER_PLATFORMS="$platform"
  # A fresh local build is not a pushed image: the old digest no longer describes it.
  L_RUNNER_DIGEST=""
  lock_write
  echo "Runner image built; ${LOCK} updated (RUNNER_PLATFORMS=${platform})."
}

# ------------------------------------------------------------------ server --
require_core_credential() {
  if [ -z "${PUPPET_CORE_USER:-}" ]; then
    die "PUPPET_CORE_USER is not set. Export your Puppet Core repository login in your own terminal (the value is never printed or stored)." 2
  fi
  if [ -z "${PUPPET_CORE_API_KEY:-}" ]; then
    die "PUPPET_CORE_API_KEY is not set. Export your Puppet Core API key in your own terminal (the value is never printed or stored)." 2
  fi
}

require_server_pins() {
  local name
  for name in SERVER_BASE_IMAGE PUPPET_RELEASE_SERIES PUPPET_RELEASE_DEB SERVER_IMAGE_NAME SERVER_TAG; do
    if [ -z "${!name:-}" ]; then
      die "${name} is missing from harness/versions.env." 2
    fi
  done
}

server_build_args() {
  SERVER_ARGS=(
    --build-arg "SERVER_BASE_IMAGE=${SERVER_BASE_IMAGE}"
    --build-arg "PUPPET_RELEASE_SERIES=${PUPPET_RELEASE_SERIES}"
    --build-arg "PUPPET_RELEASE_DEB=${PUPPET_RELEASE_DEB}"
    --secret "id=puppetcore_user,env=PUPPET_CORE_USER"
    --secret "id=puppetcore_key,env=PUPPET_CORE_API_KEY"
  )
}

# try_server_build PLATFORM: builds into the local image store, keeps the last
# log lines for a failure report. Returns docker's status.
try_server_build() {
  local platform="$1" log status
  log="$(mktemp)"
  echo "Building Puppet Server 9 for ${platform} ..."
  server_build_args
  set +e
  docker buildx build --load --platform "$platform" \
    "${SERVER_ARGS[@]}" -t "$SERVER_REF" harness/server 2>&1 | redact | tee "$log" | tail -n 5
  status="${PIPESTATUS[0]}"
  set -e
  if [ "$status" -ne 0 ]; then
    SERVER_FAIL_TAIL="$(tail -n 40 "$log")"
  fi
  rm -f "$log"
  return "$status"
}

build_server() {
  require_core_credential
  require_server_pins
  local native arm64_native
  native="$(native_platform)"
  SERVER_FAIL_TAIL=""
  lock_load
  lock_reset_if_retagged
  arm64_native="$L_SERVER_ARM64_NATIVE"
  local built=""

  if try_server_build "$native"; then
    built="$native"
    if [ "$native" = "linux/arm64" ]; then
      arm64_native="true"
    fi
  elif [ "$native" = "linux/arm64" ]; then
    echo "The native arm64 build failed; retrying under linux/amd64 emulation." >&2
    echo "Last log lines of the native attempt:" >&2
    printf '%s\n' "$SERVER_FAIL_TAIL" >&2
    arm64_native="false"
    if try_server_build "linux/amd64"; then
      built="linux/amd64"
    fi
  fi

  if [ -z "$built" ]; then
    echo "The Puppet Server 9 build failed on every platform tried. Last 40 log lines:" >&2
    printf '%s\n' "$SERVER_FAIL_TAIL" >&2
    echo "Report this result; do not switch to OpenVox (D-08, D-18)." >&2
    exit 1
  fi

  local raw version
  raw="$(docker run --rm --platform "$built" --entrypoint cat "$SERVER_REF" /etc/stagehand-harness/puppetserver-version)"
  version="$(printf '%s\n' "$raw" | grep -Eo '[0-9]+\.[0-9]+\.[0-9]+' | head -n 1 || true)"
  if [ -z "$version" ]; then
    echo "Built the image but could not read a version from /etc/stagehand-harness/puppetserver-version." >&2
    exit 1
  fi

  L_SERVER_PLATFORMS="$built"
  L_SERVER_ARM64_NATIVE="$arm64_native"
  L_SERVER_DIGEST=""
  L_PUPPETSERVER_VERSION="$version"
  lock_write
  echo "Puppet Server ${version} built for ${built}; ${LOCK} updated (SERVER_ARM64_NATIVE=${arm64_native:-unknown})."
}

# -------------------------------------------------------------------- push --
# digest_of REF: the manifest digest the registry now holds for REF.
digest_of() {
  docker buildx imagetools inspect "$1" | awk '/^Digest:/ { print $2; exit }'
}

push_images() {
  lock_load
  lock_reset_if_retagged

  echo "Pushing the runner image (linux/amd64, linux/arm64) ..."
  runner_build_args
  docker buildx build --push --platform linux/amd64,linux/arm64 \
    "${RUNNER_ARGS[@]}" -t "$RUNNER_REF" harness/runner 2>&1 | redact
  local runner_digest
  runner_digest="$(digest_of "$RUNNER_REF")"
  [ -n "$runner_digest" ] || die "Could not read the pushed runner digest." 1
  L_RUNNER_PLATFORMS="linux/amd64,linux/arm64"
  L_RUNNER_DIGEST="$runner_digest"
  lock_write

  require_core_credential
  require_server_pins
  local platforms="linux/amd64"
  if [ "$L_SERVER_ARM64_NATIVE" = "true" ]; then
    platforms="linux/arm64,linux/amd64"
  fi
  echo "Pushing the Puppet Server 9 image (${platforms}) ..."
  server_build_args
  docker buildx build --push --platform "$platforms" \
    "${SERVER_ARGS[@]}" -t "$SERVER_REF" harness/server 2>&1 | redact
  local server_digest
  server_digest="$(digest_of "$SERVER_REF")"
  [ -n "$server_digest" ] || die "Could not read the pushed server digest." 1
  L_SERVER_PLATFORMS="$platforms"
  L_SERVER_DIGEST="$server_digest"
  lock_write
  echo "Both images pushed; ${LOCK} now holds RUNNER_DIGEST and SERVER_DIGEST."
}

usage() {
  sed -n '2,14p' "$0" >&2
  exit 2
}

case "${1:-}" in
  build)
    case "${2:-}" in
      runner) build_runner ;;
      server) build_server ;;
      *) usage ;;
    esac
    ;;
  push)
    if [ "${2:-}" != "--confirm" ]; then
      die "push needs the literal --confirm argument: ./harness/images.sh push --confirm. Nothing was pushed." 2
    fi
    push_images
    ;;
  *) usage ;;
esac
