#!/usr/bin/env bash
# Real-SDK smoke test (spec 002, Testing #3).
#
# Builds the binary, runs it, points a pinned opentelemetry-sdk script, a
# pinned Langfuse SDK script and our own packages — Python, Go and Node — at
# it, and asserts the rows they produced. This is the drift detector for SDK conventions: when an SDK
# changes what it emits, this fails before a user notices. Our own package is
# installed from this checkout rather than pinned, because the drift it detects
# is between the package and the mapper of the same commit (spec 017 #12).
#
#   scripts/smoke/run.sh
#   SMOKE_KEY_SCOPES=ingest scripts/smoke/run.sh
#
# By default every exporter sends with the project's first key, which holds all
# three scopes. With SMOKE_KEY_SCOPES=ingest they send with a key minted for
# ingest alone — what a production application should hold (spec 045 #16) —
# and only the check reads back with the first one.
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
        uv pip install --quiet --python "$work/venv/bin/python" \
            -r "$smoke_dir/requirements.txt" -e "$repo_root/sdk/python"
    else
        python3 -m venv "$work/venv"
        "$work/venv/bin/pip" install --quiet \
            -r "$smoke_dir/requirements.txt" -e "$repo_root/sdk/python"
    fi
    python_bin="$work/venv/bin/python"
fi

echo "==> building tracepad"
go build -o "$work/tracepad" "$repo_root/cmd/tracepad"

port="${SMOKE_PORT:-14318}"
export TRACEPAD_DATA_DIR="$work/data"
export TRACEPAD_LISTEN="127.0.0.1:$port"
export TRACEPAD_PROJECTS="smoke:tp-pk-smoke:tp-sk-smoke-000000000000000000000000"
# Deliberately not TRACEPAD_*: the server warns about unknown variables under
# that prefix, and the exporters' own settings are not its configuration.
export SMOKE_PUBLIC_KEY="tp-pk-smoke"
export SMOKE_SECRET_KEY="tp-sk-smoke-000000000000000000000000"
export SMOKE_HOST="http://127.0.0.1:$port"
export SMOKE_OTLP_ENDPOINT="$SMOKE_HOST/v1/traces"
key_scopes="${SMOKE_KEY_SCOPES:-all}"
if [ "$key_scopes" = "ingest" ]; then
    export TRACEPAD_ADMIN_TOKEN="tp-admin-smoke-000000000000000000000000"
fi

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

if [ "$key_scopes" = "ingest" ]; then
    echo "==> minting an ingest-only key for the exporters"
    export SMOKE_READ_KEY="$SMOKE_SECRET_KEY"
    project_id="$(curl -fsS -H "Authorization: Bearer $SMOKE_SECRET_KEY" "$SMOKE_HOST/api/v1/projects" |
        python3 -c 'import json, sys; print(json.load(sys.stdin)["projects"][0]["id"])')"
    minted="$(curl -fsS -H "Authorization: Bearer $TRACEPAD_ADMIN_TOKEN" -H 'Content-Type: application/json' \
        -d '{"scopes": ["ingest"], "name": "smoke"}' "$SMOKE_HOST/api/v1/projects/$project_id/keys")"
    SMOKE_PUBLIC_KEY="$(printf '%s' "$minted" | python3 -c 'import json, sys; print(json.load(sys.stdin)["public_key"])')"
    SMOKE_SECRET_KEY="$(printf '%s' "$minted" | python3 -c 'import json, sys; print(json.load(sys.stdin)["secret_key"])')"
    export SMOKE_PUBLIC_KEY SMOKE_SECRET_KEY
elif [ "$key_scopes" != "all" ]; then
    echo "SMOKE_KEY_SCOPES must be all or ingest, got $key_scopes" >&2
    exit 2
fi

echo "==> exporting with opentelemetry-sdk"
"$python_bin" "$smoke_dir/export_otel.py" "$work/otel-trace-id"

echo "==> exporting with the langfuse SDK"
"$python_bin" "$smoke_dir/export_langfuse.py" "$work/langfuse-trace-id"

echo "==> sending a picture through the langfuse SDK's media channel"
"$python_bin" "$smoke_dir/export_langfuse_media.py" "$work/langfuse-media.json"

echo "==> exporting with the tracepad package"
"$python_bin" "$smoke_dir/export_tracepad.py" "$work/tracepad-trace-id"

echo "==> exporting with the tracepad Go package"
(cd "$repo_root/sdk/go" && go run ./internal/smoke "$work/tracepad-go-trace-id")

echo "==> exporting with the tracepad Node package"
(cd "$repo_root/sdk/js" && { [ -d node_modules ] || npm ci; } && npm run --silent build)
node "$smoke_dir/export_tracepad.mjs" "$work/tracepad-js-trace-id"

echo "==> checking the database"
python3 "$smoke_dir/check.py" "$TRACEPAD_DATA_DIR/tracepad.db" \
    "$work/otel-trace-id" "$work/langfuse-trace-id" "$work/tracepad-trace-id" \
    "$work/tracepad-go-trace-id" "$work/tracepad-js-trace-id" "$work/langfuse-media.json"

echo "==> server log"
cat "$work/server.log"
