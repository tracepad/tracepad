-- The Langfuse upload channel's lifecycle (spec 041 #29, #31).

-- The traces a deletion or an erasure removed within an upload URL's lifetime
-- (#29): the upload POST and PUT refuse one of them, before the body and in
-- the write. Written in the transaction that removes the traces, and swept once
-- older than any URL issued before the removal could be. Unix nanoseconds.
CREATE TABLE media_voided (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trace_id   TEXT NOT NULL,
    at         INTEGER NOT NULL,
    PRIMARY KEY (project_id, trace_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_media_voided_at ON media_voided(at);

-- A trace's arrival settles its refs from this schema on (#31); before it, a
-- ref whose trace arrived without naming its body stayed pending until the
-- day's sweep. Those are settled here, so that they do not count toward the
-- cap. A walk of the pending refs alone, on their own partial index.
UPDATE media_refs SET pending = 0
 WHERE pending = 1
   AND EXISTS (SELECT 1 FROM traces t WHERE t.project_id = media_refs.project_id AND t.id = media_refs.trace_id);

-- The pending refs of one project (#31): the cap counts them without reading
-- the project's settled refs, and a trace's arrival settles its own with a
-- seek.
CREATE INDEX idx_media_refs_project_pending ON media_refs(project_id, trace_id) WHERE pending = 1;
