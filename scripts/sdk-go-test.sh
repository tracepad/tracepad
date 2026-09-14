#!/usr/bin/env bash
# The Go package's suite: vet and unit tests inside the nested module, then
# the end-to-end package against a real binary (spec 033 #11, Testing).
#
#   scripts/sdk-go-test.sh [go test arguments…]
#
# Set SDK_SKIP_E2E=1 to run the unit half alone (the e2e package then skips
# itself, since TRACEPAD_BINARY is unset), or TRACEPAD_BINARY to a binary
# already built to skip the build (what CI does, with a different Go).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
package="$repo_root/sdk/go"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

if [ -z "${SDK_SKIP_E2E:-}" ] && [ -z "${TRACEPAD_BINARY:-}" ]; then
    echo "==> building tracepad"
    go build -o "$work/tracepad" "$repo_root/cmd/tracepad"
    export TRACEPAD_BINARY="$work/tracepad"
fi

cd "$package"
echo "==> go vet"
go vet ./...
echo "==> go test"
go test "$@" ./...
