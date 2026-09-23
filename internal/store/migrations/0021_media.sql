-- Media: images and files taken out of the JSON at ingest (spec 041).
--
-- One row per distinct content across the whole store, addressed by the
-- SHA-256 of the decoded bytes (#2): a photograph sent to five generations is
-- one row, not five. The body is the decoded bytes, three quarters of the
-- base64 it arrived as, and it lives in this file so that one file stays the
-- backup story.
CREATE TABLE media (
    sha256     TEXT PRIMARY KEY,       -- hex of the decoded bytes
    mime_type  TEXT NOT NULL,
    size       INTEGER NOT NULL,       -- decoded bytes
    body       BLOB NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

-- Who points at a body (#3). A body lives while a row here or in
-- `media_raw_refs` names it, and every path that deletes traces deletes their
-- rows in the same transaction and collects the bodies left with none.
--
-- `created_at` (Unix nanoseconds) is not in the spec's schema (Decision 13):
-- the Langfuse channel records a ref for a trace whose spans have not arrived
-- yet, and the sweep needs an age to tell "not yet" from "never".
CREATE TABLE media_refs (
    sha256     TEXT NOT NULL REFERENCES media(sha256),
    project_id TEXT NOT NULL,
    trace_id   TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (sha256, project_id, trace_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_media_refs_trace ON media_refs(project_id, trace_id);

-- The raw archive's own refs (Decision 12). A raw batch outlives the traces
-- it fed — erasure and trace deletion leave it, and its window is its own
-- (spec 005 #6, #7; spec 035 #3) — so a ref keyed by trace id could not keep
-- a body alive for the batch that needs it at replay. Keyed by the batch, and
-- dropped with it.
CREATE TABLE media_raw_refs (
    sha256       TEXT NOT NULL REFERENCES media(sha256),
    raw_batch_id INTEGER NOT NULL REFERENCES raw_batches(id) ON DELETE CASCADE,
    PRIMARY KEY (sha256, raw_batch_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_media_raw_refs_batch ON media_raw_refs(raw_batch_id);

-- The project setting of #6: `placeholder` writes no body and leaves a
-- reference that says so.
ALTER TABLE projects ADD COLUMN media TEXT NOT NULL DEFAULT 'store'
    CHECK (media IN ('store', 'placeholder'));
