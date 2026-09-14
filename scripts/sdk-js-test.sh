#!/usr/bin/env bash
# The Node package's suite: the type check, the unit tests, then end-to-end
# against a real binary (spec 032, Testing) — the twin of `sdk-test.sh`.
#
#   scripts/sdk-js-test.sh [vitest arguments…]
#
# Set SDK_SKIP_E2E=1 to run the unit half alone (no Go toolchain needed).
# `npm ci` runs only when `sdk/js/node_modules` is missing, the way the
# interface's toolchain is installed.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
package="$repo_root/sdk/js"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

if [ ! -d "$package/node_modules" ]; then
    echo "==> npm ci"
    (cd "$package" && npm ci)
fi

if [ -z "${SDK_SKIP_E2E:-}" ]; then
    echo "==> building tracepad"
    go build -o "$work/tracepad" "$repo_root/cmd/tracepad"
    export TRACEPAD_BINARY="$work/tracepad"
fi

cd "$package"
echo "==> tsc"
npx tsc --noEmit
echo "==> vitest"
npx vitest run "$@"
