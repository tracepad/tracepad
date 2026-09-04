#!/usr/bin/env bash
# The Python package's suite: unit tests, then end-to-end against a real binary
# (spec 017, Testing).
#
#   scripts/sdk-test.sh [pytest arguments…]
#
# Set SDK_PYTHON to an interpreter that already has the package and pytest
# installed to skip the environment setup. Set SDK_SKIP_E2E=1 to run the unit
# half alone (no Go toolchain needed).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
package="$repo_root/sdk/python"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

python_bin="${SDK_PYTHON:-}"
if [ -z "$python_bin" ]; then
    echo "==> preparing python environment"
    if command -v uv >/dev/null 2>&1; then
        uv venv "$work/venv" >/dev/null
        uv pip install --quiet --python "$work/venv/bin/python" -e "$package" pytest
    else
        python3 -m venv "$work/venv"
        "$work/venv/bin/pip" install --quiet -e "$package" pytest
    fi
    python_bin="$work/venv/bin/python"
fi

if [ -z "${SDK_SKIP_E2E:-}" ]; then
    echo "==> building tracepad"
    go build -o "$work/tracepad" "$repo_root/cmd/tracepad"
    export TRACEPAD_BINARY="$work/tracepad"
fi

echo "==> pytest"
cd "$package"
"$python_bin" -m pytest "$@"
