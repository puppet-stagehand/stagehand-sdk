#!/usr/bin/env bash
# Builds the local git fixtures the harness deploys from. Runs inside the runner
# container as the non-root user "deploy". Idempotent: it recreates both bare
# repos each time. Author, committer and dates are fixed so the commit SHAs are
# stable across runs. Prints the control repo's production HEAD SHA as the last
# line of stdout.
set -euo pipefail

export GIT_AUTHOR_NAME="Stagehand Harness"
export GIT_AUTHOR_EMAIL="harness@example.invalid"
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
export GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
export GIT_AUTHOR_DATE="2026-01-01T00:00:00+0000"
export GIT_COMMITTER_DATE="$GIT_AUTHOR_DATE"
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

MODULE_BARE=/srv/git/module-fixture.git
CONTROL_BARE=/srv/git/control.git
BUILD=/srv/work/gitbuild

rm -rf "$MODULE_BARE" "$CONTROL_BARE" "$BUILD"
mkdir -p "$BUILD/module" "$BUILD/control"

git init --quiet --bare --initial-branch=main "$MODULE_BARE"
git init --quiet --bare --initial-branch=production "$CONTROL_BARE"

# A tiny module the control repo's Puppetfile pulls in from the file:// remote.
cd "$BUILD/module"
git init --quiet --initial-branch=main
mkdir -p manifests
cat > metadata.json <<'JSON'
{
  "name": "stagehand-m",
  "version": "0.1.0",
  "author": "stagehand",
  "summary": "Harness fixture module",
  "license": "Apache-2.0",
  "source": "file:///srv/git/module-fixture.git",
  "dependencies": []
}
JSON
cat > manifests/init.pp <<'PP'
class m {
}
PP
git add -A
git commit --quiet -m "module fixture"
git push --quiet "file://$MODULE_BARE" main

# The control repo: branch "production", one Git module in the Puppetfile.
cd "$BUILD/control"
git init --quiet --initial-branch=production
mkdir -p manifests
cat > environment.conf <<'CONF'
modulepath = modules:$basemodulepath
CONF
cat > manifests/site.pp <<'PP'
node default {
}
PP
cat > Puppetfile <<'PF'
mod 'm',
  :git => 'file:///srv/git/module-fixture.git',
  :ref => 'main'
PF
git add -A
git commit --quiet -m "control fixture"
git push --quiet "file://$CONTROL_BARE" production

git --git-dir="$CONTROL_BARE" rev-parse production
