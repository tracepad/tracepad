#!/usr/bin/env bash
# What a script must not change about the repository it runs in (spec 020 #35):
# its local configuration, which repository it is, whether it is bare, and its
# tags. Printed one fact per line, for scripts/gate-guard.sh to compare. Reads
# the repository the caller's environment names, which under a git hook is the
# real one.
set -euo pipefail

echo "common-dir: $(cd "$(git rev-parse --git-common-dir)" && pwd -P)"
echo "bare: $(git rev-parse --is-bare-repository)"
git config --local --list | sed 's/^/config: /' | LC_ALL=C sort
echo "tags: $(git tag --list | cksum)"
