-- An erasure reaches every copy the store holds (spec 044).

-- When an erasure rewrote a raw batch without the erased spans (#2). NULL is a
-- batch as it was received; a rewritten one is no longer what the client sent,
-- and the listing, the body and the export say so. Nothing records whose spans
-- went (#15).
ALTER TABLE raw_batches ADD COLUMN scrubbed_at INTEGER;  -- Unix ns, NULL = as received

-- The retention pass over session-only scores (#8): a score that names no
-- trace goes once it is older than the trace window and no trace carries its
-- session.
CREATE INDEX idx_scores_session_only ON scores(project_id, created_at)
    WHERE trace_id IS NULL;

-- The erasure's lookup of the dataset items cut from a trace (#9).
CREATE INDEX idx_dataset_items_source ON dataset_items(project_id, source_trace_id)
    WHERE source_trace_id IS NOT NULL;
