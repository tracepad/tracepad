"""Assert what the two real SDKs left in the database.

Direct SQL, on purpose, even now that the read API exists (spec 004): this is
the drift detector for SDK conventions, and what it is checking is what a
real export *stored* — the mapping, not the rendering. Reading it back through
the API would test both at once and blame the wrong one when either moved.

Uses only the standard library so it runs with any Python, including the one
that never saw the SDKs.
"""

import json
import sqlite3
import sys

db_path, otel_trace_id_file, langfuse_trace_id_file, tracepad_trace_id_file = sys.argv[1:5]

failures = []


def check(condition, message):
    if not condition:
        failures.append(message)


def read_id(path):
    with open(path) as f:
        return f.read().strip()


db = sqlite3.connect(db_path)
db.row_factory = sqlite3.Row


def trace(trace_id):
    row = db.execute("SELECT * FROM traces WHERE id = ?", (trace_id,)).fetchone()
    if row is None:
        raise SystemExit(f"FAIL: no trace row for {trace_id}")
    return row


def observations(trace_id):
    return db.execute(
        "SELECT * FROM observations WHERE trace_id = ? ORDER BY start_time", (trace_id,)
    ).fetchall()


def payload(payload_id):
    if payload_id is None:
        return None
    row = db.execute(
        "SELECT compression, body FROM payloads WHERE id = ?", (payload_id,)
    ).fetchone()
    body = row["body"]
    if row["compression"] == "zstd":
        # Only the raw bodies and oversized payloads are compressed; a
        # smoke payload is small, so a zstd frame here means the threshold
        # moved and this script needs a real decoder.
        raise SystemExit("FAIL: payload is zstd-compressed, cannot decode with stdlib")
    return json.loads(body)


# --- plain OpenTelemetry SDK, GenAI semconv -------------------------------
otel_id = read_id(otel_trace_id_file)
row = trace(otel_id)
check(row["name"] == "handle-request", f"otel trace name = {row['name']!r}")
check(row["user_id"] == "smoke-user", f"otel user_id = {row['user_id']!r}")
check(row["session_id"] == "smoke-session", f"otel session_id = {row['session_id']!r}")
check(row["environment"] == "smoke", f"otel environment = {row['environment']!r}")
check(row["observation_count"] == 2, f"otel observation_count = {row['observation_count']}")
check(row["error_count"] == 0, f"otel error_count = {row['error_count']}")
check(row["timestamp"] is not None and row["timestamp"] > 0, "otel trace has no timestamp")
check(row["latency_ms"] is not None, "otel trace has no latency")

spans = observations(otel_id)
generations = [s for s in spans if s["type"] == "generation"]
check(len(generations) == 1, f"otel generations = {len(generations)}")
if generations:
    generation = generations[0]
    check(
        generation["model"] == "gpt-4o-mini",
        f"otel generation model = {generation['model']!r}",
    )
    usage = json.loads(generation["usage"] or "{}")
    check(usage.get("input_tokens") == 42, f"otel usage = {usage}")
    check(usage.get("output_tokens") == 7, f"otel usage = {usage}")
    params = json.loads(generation["model_parameters"] or "{}")
    check(params.get("max_tokens") == 128, f"otel model_parameters = {params}")
    check(generation["provided_cost"] == 0, "otel generation must not claim a cost")
    check(payload(generation["input_id"]) is not None, "otel generation has no input")
    check(payload(generation["output_id"]) is not None, "otel generation has no output")

# --- Langfuse SDK, langfuse.* dialect -------------------------------------
langfuse_id = read_id(langfuse_trace_id_file)
row = trace(langfuse_id)
check(row["name"] == "smoke-trace", f"langfuse trace name = {row['name']!r}")
check(row["user_id"] == "smoke-user", f"langfuse user_id = {row['user_id']!r}")
check(row["session_id"] == "smoke-session", f"langfuse session_id = {row['session_id']!r}")
check(row["environment"] == "smoke", f"langfuse environment = {row['environment']!r}")
check(sorted(json.loads(row["tags"] or "[]")) == ["smoke", "spec-002"],
      f"langfuse tags = {row['tags']!r}")
check(row["observation_count"] == 2, f"langfuse observation_count = {row['observation_count']}")
check(
    row["total_cost"] is not None and abs(row["total_cost"] - 0.0003) < 1e-9,
    f"langfuse total_cost = {row['total_cost']}",
)
metadata = payload(row["metadata_id"]) or {}
check(metadata.get("suite") == "smoke", f"langfuse trace metadata = {metadata}")

spans = observations(langfuse_id)
generations = [s for s in spans if s["type"] == "generation"]
check(len(generations) == 1, f"langfuse generations = {len(generations)}")
if generations:
    generation = generations[0]
    check(
        generation["model"] == "claude-sonnet-5",
        f"langfuse generation model = {generation['model']!r}",
    )
    check(generation["provided_cost"] == 1, "langfuse generation must carry provided_cost")
    usage = json.loads(generation["usage"] or "{}")
    check(usage.get("total") == 16, f"langfuse usage = {usage}")
    params = json.loads(generation["model_parameters"] or "{}")
    check(params.get("temperature") in (0.1, "0.1"), f"langfuse model_parameters = {params}")
    check(payload(generation["input_id"]) is not None, "langfuse generation has no input")
    check(payload(generation["output_id"]) is not None, "langfuse generation has no output")

# --- the tracepad package, tracepad.* dialect (spec 017) -------------------
tracepad_id = read_id(tracepad_trace_id_file)
row = trace(tracepad_id)
check(row["name"] == "tracepad-smoke", f"tracepad trace name = {row['name']!r}")
check(row["user_id"] == "smoke-user", f"tracepad user_id = {row['user_id']!r}")
check(row["session_id"] == "smoke-session", f"tracepad session_id = {row['session_id']!r}")
check(row["environment"] == "smoke", f"tracepad environment = {row['environment']!r}")
check(row["release"] == "smoke-1", f"tracepad release = {row['release']!r}")
check(sorted(json.loads(row["tags"] or "[]")) == ["smoke", "spec-017"],
      f"tracepad tags = {row['tags']!r}")
check(row["observation_count"] == 2, f"tracepad observation_count = {row['observation_count']}")
check(
    row["total_cost"] is not None and abs(row["total_cost"] - 0.0003) < 1e-9,
    f"tracepad total_cost = {row['total_cost']}",
)
check((payload(row["metadata_id"]) or {}).get("suite") == "smoke",
      "tracepad trace metadata lost its entry")

spans = observations(tracepad_id)
generations = [s for s in spans if s["type"] == "generation"]
check(len(generations) == 1, f"tracepad generations = {len(generations)}")
if generations:
    generation = generations[0]
    check(
        generation["model"] == "claude-sonnet-5",
        f"tracepad generation model = {generation['model']!r}",
    )
    usage = json.loads(generation["usage"] or "{}")
    check(usage.get("input_tokens") == 11, f"tracepad usage = {usage}")
    check(usage.get("output_tokens") == 5, f"tracepad usage = {usage}")
    params = json.loads(generation["model_parameters"] or "{}")
    check(params.get("max_tokens") == 64, f"tracepad model_parameters = {params}")
    check(generation["provided_cost"] == 1, "tracepad generation must carry provided_cost")
    check(generation["completion_start_time"] is not None,
          "tracepad generation has no completion start")
    check(payload(generation["input_id"]) is not None, "tracepad generation has no input")
    check(payload(generation["output_id"]) is not None, "tracepad generation has no output")

# --- raw bodies (spec 002 #9) ---------------------------------------------
raw = db.execute("SELECT dialect, content_encoding, body FROM raw_batches").fetchall()
check(len(raw) >= 3, f"raw_batches = {len(raw)}, want one per accepted export")
dialects = {r["dialect"] for r in raw}
check("langfuse" in dialects, f"raw dialects = {dialects}")
check("genai" in dialects, f"raw dialects = {dialects}")
check("tracepad" in dialects, f"raw dialects = {dialects}")
for r in raw:
    # zstd frame magic: the stored body must be compressed, whatever the
    # request's own Content-Encoding was.
    check(r["body"][:4] == b"\x28\xb5\x2f\xfd", "raw batch body is not a zstd frame")

if failures:
    print("SMOKE FAILED:")
    for failure in failures:
        print(f"  - {failure}")
    raise SystemExit(1)

print(f"smoke OK: 3 exports, {len(raw)} raw batches stored")
