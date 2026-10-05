#!/usr/bin/env bash
# What a script must not change about the repository it runs in (spec 020 #35):
# which repository it is, whether it is bare, and its local configuration — all
# of it except the `branch.*` and `remote.*` keys, which other sessions working
# in the same shared repository change legitimately while a gate runs (`git push
# -u`, `git branch --set-upstream-to`); the damage this exists to catch is in the
# other keys (`core.bare`, `user.*`, `*.gpgsign`, …). Printed one fact per line,
# for scripts/gate-guard.sh to compare. Reads the repository the caller's
# environment names, which under a git hook is the real one.
set -euo pipefail

echo "common-dir: $(cd "$(git rev-parse --git-common-dir)" && pwd -P)"
echo "bare: $(git rev-parse --is-bare-repository)"
git config --local --list | grep -Ev '^(branch|remote)\.' | sed 's/^/config: /' | LC_ALL=C sort || true
