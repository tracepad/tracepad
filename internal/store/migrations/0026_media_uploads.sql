-- The Langfuse upload channel's lifecycle (spec 041 #29, #31).

-- The traces a deletion or an erasure removed within an upload URL's lifetime
-- (#29): the upload PUT refuses a URL for one of them, before the body and in
-- the write. Written in the transaction that removes the traces, and swept once
-- older than any URL issued before the removal could be. Unix nanoseconds.
CREATE TABLE media_voided (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trace_id   TEXT NOT NULL,
    at         INTEGER NOT NULL,
    PRIMARY KEY (project_id, trace_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_media_voided_at ON media_voided(at);

-- The pending refs of one project (#31): the cap counts them without reading
-- the project's settled refs.
CREATE INDEX idx_media_refs_project_pending ON media_refs(project_id) WHERE pending = 1;
