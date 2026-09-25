#!/usr/bin/env bash
# The Python package's suite: unit tests, then end-to-end against a real binary
# (spec 017, Testing) — on both ends of the OpenTelemetry range the package
# admits, the floor `pyproject.toml` names and the newest release (spec 042 #14).
#
#   scripts/sdk-test.sh [pytest arguments…]
#   scripts/sdk-test.sh --floor-pins     print the floor as pip requirements
#
# Set SDK_PYTHON to an interpreter that already has the package and pytest
# installed to skip the environment setup; the suite then runs once, on
# whatever OpenTelemetry that interpreter has (CI installs the floor into one
# of its two with `--floor-pins`). Set SDK_SKIP_E2E=1 to run the unit half
# alone (no Go toolchain needed).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
package="$repo_root/sdk/python"

# `"opentelemetry-sdk>=1.44,<2",` → `opentelemetry-sdk==1.44`, which pip reads as 1.44.0.
floor_pins() {
    sed -n 's/^ *"\(opentelemetry-[a-z-]*\)>=\([0-9.]*\),.*/\1==\2/p' "$package/pyproject.toml"
}
if [ "${1:-}" = "--floor-pins" ]; then
    floor_pins
    exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# One environment per end of the range: `latest` resolves as a fresh
# `pip install tracepad` would, `floor` pins OpenTelemetry to what we promise.
prepare() {
    local venv="$work/venv-$1" pins=()
    if [ "$1" = floor ]; then
        # shellcheck disable=SC2207 # requirement strings: no spaces, no globs
        pins=($(floor_pins))
        # Found nothing is a floor run that tests the newest release instead.
        [ ${#pins[@]} -gt 0 ] || { echo "no OpenTelemetry floor found in pyproject.toml" >&2; exit 1; }
    fi
    echo "==> preparing python environment (OpenTelemetry $1)"
    if command -v uv >/dev/null 2>&1; then
        uv venv "$venv" >/dev/null
        uv pip install --quiet --python "$venv/bin/python" -e "$package" pytest ${pins[@]+"${pins[@]}"}
    else
        python3 -m venv "$venv"
        "$venv/bin/pip" install --quiet -e "$package" pytest ${pins[@]+"${pins[@]}"}
    fi
    pythons+=("$venv/bin/python")
}

pythons=()
if [ -n "${SDK_PYTHON:-}" ]; then
    pythons=("$SDK_PYTHON")
else
    prepare floor
    prepare latest
fi

if [ -z "${SDK_SKIP_E2E:-}" ]; then
    echo "==> building tracepad"
    go build -o "$work/tracepad" "$repo_root/cmd/tracepad"
    export TRACEPAD_BINARY="$work/tracepad"
fi

cd "$package"
for python_bin in "${pythons[@]}"; do
    echo "==> pytest (opentelemetry-sdk $("$python_bin" -c \
        'from importlib.metadata import version; print(version("opentelemetry-sdk"))'))"
    "$python_bin" -m pytest "$@"
done
