#!/bin/sh
# Content hash of a directory tree, used by the harness to prove whether a tool
# changed a live environment (for example g10k -dryrun, D-11).
#
#   tree-hash.sh <dir> [--exclude-markers]
#
# Every regular file under <dir> is hashed with sha256, excluding any .git
# directory. With --exclude-markers the deploy markers .r10k-deploy.json and
# .g10k-deploy.json are left out as well, so a change to the working files can be
# told apart from a change to the marker alone. The "hash  path" lines are sorted
# by path and the output is the first 16 hex characters of the sha256 of that
# list. Content only: file times and modes do not count, because mtimes lie.
# The script writes nothing.
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: tree-hash.sh <dir> [--exclude-markers]" >&2
  exit 2
fi

dir=$1
exclude=0
if [ "$#" -eq 2 ]; then
  if [ "$2" != "--exclude-markers" ]; then
    echo "tree-hash.sh: unknown option: $2" >&2
    exit 2
  fi
  exclude=1
fi

if [ ! -d "$dir" ]; then
  echo "tree-hash.sh: not a directory: $dir" >&2
  exit 2
fi

cd "$dir"
if [ "$exclude" -eq 1 ]; then
  find . -type f ! -path '*/.git/*' ! -name '.r10k-deploy.json' ! -name '.g10k-deploy.json' -exec sha256sum {} +
else
  find . -type f ! -path '*/.git/*' -exec sha256sum {} +
fi | LC_ALL=C sort -k2 | sha256sum | cut -c1-16
