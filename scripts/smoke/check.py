"""Assert what the real SDKs and our own packages left in the database.

Direct SQL, on purpose, even now that the read API exists (spec 004): this is
the drift detector for SDK conventions, and what it is checking is what a
real export *stored* — the mapping, not the rendering. Reading it back through
the API would test both at once and blame the wrong one when either moved.

Uses only the standard library so it runs with any Python, including the one
that never saw the SDKs.
"""

import base64
import hashlib
import json
import os
import sqlite3
import sys
import urllib.request

(
    db_path,
    otel_trace_id_file,
    langfuse_trace_id_file,
    tracepad_trace_id_file,
    tracepad_go_trace_id_file,
    tracepad_js_trace_id_file,
    langfuse_media_file,
    env_only_readme_trace_id_file,
    env_only_quickstart_trace_id_file,
) = sys.argv[1:10]

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

# --- an application that configures nothing: the docs' environment recipe ---
# No SDK code in the script and no attribute that is not GenAI semconv, so a
# trace of the right shape here means the three variables the README and the
# quickstart print were, by themselves, enough to get a span into the store.
for label, id_file in (
    ("README", env_only_readme_trace_id_file),
    ("quickstart", env_only_quickstart_trace_id_file),
):
    row = db.execute("SELECT * FROM traces WHERE id = ?", (read_id(id_file),)).fetchone()
    if row is None:
        failures.append(f"env-only ({label}): no trace arrived; the recipe no longer works")
        continue
    tag = f"env-only ({label})"
    check(row["name"] == "env-only-request", f"{tag} trace name = {row['name']!r}")
    check(row["user_id"] == "smoke-user", f"{tag} user_id = {row['user_id']!r}")
    check(row["session_id"] == "smoke-session", f"{tag} session_id = {row['session_id']!r}")
    check(row["observation_count"] == 2, f"{tag} observation_count = {row['observation_count']}")
    generations = [s for s in observations(row["id"]) if s["type"] == "generation"]
    check(len(generations) == 1, f"{tag} generations = {len(generations)}")
    if generations:
        check(
            generations[0]["model"] == "gpt-4o-mini",
            f"{tag} generation model = {generations[0]['model']!r}",
        )
        usage = json.loads(generations[0]["usage"] or "{}")
        check(
            usage.get("input_tokens") == 11 and usage.get("output_tokens") == 3,
            f"{tag} usage = {usage}",
        )

# --- Langfuse SDK, langfuse.* dialect -------------------------------------
langfuse_id = read_id(langfuse_trace_id_file)
row = trace(langfuse_id)
check(row["name"] == "smoke-trace", f"langfuse trace name = {row['name']!r}")
check(row["user_id"] == "smoke-user", f"langfuse user_id = {row['user_id']!r}")
check(row["session_id"] == "smoke-session", f"langfuse session_id = {row['session_id']!r}")
check(row["environment"] == "smoke", f"langfuse environment = {row['environment']!r}")
check(
    sorted(json.loads(row["tags"] or "[]")) == ["smoke", "spec-002"],
    f"langfuse tags = {row['tags']!r}",
)
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
check(
    sorted(json.loads(row["tags"] or "[]")) == ["smoke", "spec-017"],
    f"tracepad tags = {row['tags']!r}",
)
check(row["observation_count"] == 2, f"tracepad observation_count = {row['observation_count']}")
check(
    row["total_cost"] is not None and abs(row["total_cost"] - 0.0003) < 1e-9,
    f"tracepad total_cost = {row['total_cost']}",
)
check(
    (payload(row["metadata_id"]) or {}).get("suite") == "smoke",
    "tracepad trace metadata lost its entry",
)

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
    check(
        generation["completion_start_time"] is not None,
        "tracepad generation has no completion start",
    )
    check(payload(generation["input_id"]) is not None, "tracepad generation has no input")
    check(payload(generation["output_id"]) is not None, "tracepad generation has no output")

# --- the tracepad Go package, the same dialect (spec 033) ------------------
go_id = read_id(tracepad_go_trace_id_file)
row = trace(go_id)
check(row["name"] == "tracepad-go-smoke", f"tracepad-go trace name = {row['name']!r}")
check(row["user_id"] == "smoke-user", f"tracepad-go user_id = {row['user_id']!r}")
check(row["session_id"] == "smoke-session", f"tracepad-go session_id = {row['session_id']!r}")
check(row["environment"] == "smoke", f"tracepad-go environment = {row['environment']!r}")
check(row["release"] == "smoke-1", f"tracepad-go release = {row['release']!r}")
check(
    sorted(json.loads(row["tags"] or "[]")) == ["smoke", "spec-033"],
    f"tracepad-go tags = {row['tags']!r}",
)
check(row["observation_count"] == 2, f"tracepad-go observation_count = {row['observation_count']}")
check(
    row["total_cost"] is not None and abs(row["total_cost"] - 0.0003) < 1e-9,
    f"tracepad-go total_cost = {row['total_cost']}",
)
check(
    (payload(row["metadata_id"]) or {}).get("suite") == "smoke",
    "tracepad-go trace metadata lost its entry",
)

spans = observations(go_id)
generations = [s for s in spans if s["type"] == "generation"]
check(len(generations) == 1, f"tracepad-go generations = {len(generations)}")
if generations:
    generation = generations[0]
    check(
        generation["model"] == "claude-sonnet-5",
        f"tracepad-go generation model = {generation['model']!r}",
    )
    usage = json.loads(generation["usage"] or "{}")
    check(usage.get("input_tokens") == 11, f"tracepad-go usage = {usage}")
    check(usage.get("output_tokens") == 5, f"tracepad-go usage = {usage}")
    params = json.loads(generation["model_parameters"] or "{}")
    check(params.get("max_tokens") == 64, f"tracepad-go model_parameters = {params}")
    check(generation["provided_cost"] == 1, "tracepad-go generation must carry provided_cost")
    check(
        generation["completion_start_time"] is not None,
        "tracepad-go generation has no completion start",
    )
    check(payload(generation["input_id"]) is not None, "tracepad-go generation has no input")
    check(payload(generation["output_id"]) is not None, "tracepad-go generation has no output")

# --- the tracepad Node package, the same dialect (spec 032) ----------------
tracepad_js_id = read_id(tracepad_js_trace_id_file)
row = trace(tracepad_js_id)
check(row["name"] == "tracepad-js-smoke", f"tracepad-js trace name = {row['name']!r}")
check(row["user_id"] == "smoke-user", f"tracepad-js user_id = {row['user_id']!r}")
check(row["session_id"] == "smoke-session", f"tracepad-js session_id = {row['session_id']!r}")
check(row["environment"] == "smoke", f"tracepad-js environment = {row['environment']!r}")
check(row["release"] == "smoke-2", f"tracepad-js release = {row['release']!r}")
check(
    sorted(json.loads(row["tags"] or "[]")) == ["smoke", "spec-032"],
    f"tracepad-js tags = {row['tags']!r}",
)
check(row["observation_count"] == 2, f"tracepad-js observation_count = {row['observation_count']}")
check(
    row["total_cost"] is not None and abs(row["total_cost"] - 0.0004) < 1e-9,
    f"tracepad-js total_cost = {row['total_cost']}",
)
check(
    (payload(row["metadata_id"]) or {}).get("suite") == "smoke",
    "tracepad-js trace metadata lost its entry",
)

spans = observations(tracepad_js_id)
generations = [s for s in spans if s["type"] == "generation"]
check(len(generations) == 1, f"tracepad-js generations = {len(generations)}")
if generations:
    generation = generations[0]
    check(
        generation["model"] == "claude-sonnet-5",
        f"tracepad-js generation model = {generation['model']!r}",
    )
    usage = json.loads(generation["usage"] or "{}")
    check(usage.get("input_tokens") == 12, f"tracepad-js usage = {usage}")
    check(usage.get("output_tokens") == 6, f"tracepad-js usage = {usage}")
    params = json.loads(generation["model_parameters"] or "{}")
    check(params.get("max_tokens") == 96, f"tracepad-js model_parameters = {params}")
    check(generation["provided_cost"] == 1, "tracepad-js generation must carry provided_cost")
    check(
        generation["completion_start_time"] is not None,
        "tracepad-js generation has no completion start",
    )
    check(payload(generation["input_id"]) is not None, "tracepad-js generation has no input")
    check(payload(generation["output_id"]) is not None, "tracepad-js generation has no output")


# --- the Langfuse media channel (spec 041 #9) ------------------------------
# Payloads carrying a reference are past the compression threshold, so they are
# read back through the API rather than decoded here; the rest is the tables.
def api(path):
    key = os.environ.get("SMOKE_READ_KEY", os.environ["SMOKE_SECRET_KEY"])
    request = urllib.request.Request(
        os.environ["SMOKE_HOST"] + path, headers={"Authorization": "Bearer " + key}
    )
    with urllib.request.urlopen(request) as response:
        return response.read()


with open(langfuse_media_file) as f:
    media = json.load(f)
picture = base64.b64decode(media["picture"])
sha = hashlib.sha256(picture).hexdigest()
posts = [a for a in media["answers"] if a["method"] == "POST"]
puts = [a for a in media["answers"] if a["method"] == "PUT"]
patches = [a for a in media["answers"] if a["method"] == "PATCH"]
check(len(posts) == 2, f"media POSTs = {posts}")
if len(posts) == 2:
    check(posts[0]["body"]["uploadUrl"], f"first media POST = {posts[0]}")
    check(
        posts[1]["body"]["uploadUrl"] is None,
        f"second media POST = {posts[1]}, want uploadUrl null",
    )
    check(posts[0]["body"]["mediaId"] == posts[1]["body"]["mediaId"], f"media ids differ: {posts}")
check([p["status"] for p in puts] == [200], f"media PUTs = {puts}")
check([p["status"] for p in patches] == [204], f"media PATCHes = {patches}")
row = db.execute("SELECT mime_type, size FROM media WHERE sha256 = ?", (sha,)).fetchone()
check(
    row is not None and row["size"] == media["size"] and row["mime_type"] == "image/png",
    f"media row = {dict(row) if row else None}",
)
for trace_id in media["traces"]:
    refs = db.execute(
        "SELECT COUNT(*) FROM media_refs WHERE sha256 = ? AND trace_id = ?", (sha, trace_id)
    ).fetchone()[0]
    check(refs == 1, f"media refs of trace {trace_id} = {refs}")
    for obs in observations(trace_id):
        io = json.loads(api(f"/api/v1/observations/{obs['id']}/io?trace_id={trace_id}"))
        text = json.dumps(io.get("input"))
        check(
            sha in text and "@@@langfuseMedia" not in text,
            f"trace {trace_id} input was not rewritten to the reference: {text[:300]}",
        )
check(api(f"/api/v1/media/{sha}") == picture, "the media endpoint does not answer the picture")

# --- raw bodies (spec 002 #9) ---------------------------------------------
raw = db.execute("SELECT dialect, content_encoding, body FROM raw_batches").fetchall()
check(len(raw) >= 5, f"raw_batches = {len(raw)}, want one per accepted export")
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

print(f"smoke OK: 8 exports, {len(raw)} raw batches stored, one picture through the media channel")
