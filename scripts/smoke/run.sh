#!/usr/bin/env bash
# Real-SDK smoke test (spec 002, Testing #3).
#
# Builds the binary, runs it, points a pinned opentelemetry-sdk script and a
# pinned Langfuse SDK script at it, and asserts the rows they produced. This
# is the drift detector for SDK conventions: when an SDK changes what it
# emits, this fails before a user notices.
#
#   scripts/smoke/run.sh
#
# Set SMOKE_PYTHON to an interpreter that already has requirements.txt
# installed to skip the environment setup (that is what CI does).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
smoke_dir="$repo_root/scripts/smoke"
work="$(mktemp -d)"
trap 'cleanup' EXIT

server_pid=""
cleanup() {
    if [ -n "$server_pid" ] && kill -0 "$server_pid" 2>/dev/null; then
        kill "$server_pid" 2>/dev/null || true
        wait "$server_pid" 2>/dev/null || true
    fi
    rm -rf "$work"
}

python_bin="${SMOKE_PYTHON:-}"
if [ -z "$python_bin" ]; then
    echo "==> preparing python environment"
    if command -v uv >/dev/null 2>&1; then
        uv venv "$work/venv" >/dev/null
        uv pip install --quiet --python "$work/venv/bin/python" -r "$smoke_dir/requirements.txt"
    else
        python3 -m venv "$work/venv"
        "$work/venv/bin/pip" install --quiet -r "$smoke_dir/requirements.txt"
    fi
    python_bin="$work/venv/bin/python"
fi

echo "==> building tracepad"
go build -o "$work/tracepad" "$repo_root/cmd/tracepad"

port="${SMOKE_PORT:-14318}"
export TRACEPAD_DATA_DIR="$work/data"
export TRACEPAD_LISTEN="127.0.0.1:$port"
export TRACEPAD_PROJECTS="smoke:tp-pk-smoke:tp-sk-smoke"
# Deliberately not TRACEPAD_*: the server warns about unknown variables under
# that prefix, and the exporters' own settings are not its configuration.
export SMOKE_PUBLIC_KEY="tp-pk-smoke"
export SMOKE_SECRET_KEY="tp-sk-smoke"
export SMOKE_HOST="http://127.0.0.1:$port"
export SMOKE_OTLP_ENDPOINT="$SMOKE_HOST/v1/traces"

echo "==> starting tracepad on $TRACEPAD_LISTEN"
"$work/tracepad" serve >"$work/server.log" 2>&1 &
server_pid=$!

for _ in $(seq 1 50); do
    if curl -fsS "$SMOKE_HOST/health" >/dev/null 2>&1; then
        break
    fi
    sleep 0.2
done
if ! curl -fsS "$SMOKE_HOST/health" >/dev/null 2>&1; then
    echo "server did not come up:" >&2
    cat "$work/server.log" >&2
    exit 1
fi

echo "==> exporting with opentelemetry-sdk"
"$python_bin" "$smoke_dir/export_otel.py" "$work/otel-trace-id"

echo "==> exporting with the langfuse SDK"
"$python_bin" "$smoke_dir/export_langfuse.py" "$work/langfuse-trace-id"

echo "==> checking the database"
python3 "$smoke_dir/check.py" "$TRACEPAD_DATA_DIR/tracepad.db" \
    "$work/otel-trace-id" "$work/langfuse-trace-id"

echo "==> server log"
cat "$work/server.log"
