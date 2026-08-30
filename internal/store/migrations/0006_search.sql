-- Full-text search over what the observations carried (spec 011). STRICT per
-- spec 001 #7, except the virtual table, which is FTS5's own shape.

-- The side table is what scopes the index to a project and maps a hit back to
-- its trace: a contentless FTS table can return nothing but its rowid
-- (spec 011 #3). One row per (observation, field), plus one per trace name,
-- where `observation_id` is NULL.
--
-- `id` is an explicit INTEGER PRIMARY KEY and the FTS rowid. Not
-- `observations.rowid`: that table has no explicit integer key, and VACUUM may
-- renumber such rowids — and this store vacuums (spec 005 #5, migration
-- backups). A renumbering would silently re-point every entry at another span.
--
-- No foreign key on `project_id` either: the purge deletes a project's entries
-- explicitly, and a cascade here would fire an implicit DELETE FROM on a table
-- whose FTS half it cannot reach.
CREATE TABLE search_entries (
    id             INTEGER PRIMARY KEY,
    project_id     TEXT NOT NULL,
    trace_id       TEXT NOT NULL,
    observation_id TEXT,               -- NULL for the trace name
    field          TEXT NOT NULL
) STRICT;

-- Every deletion path addresses entries by the trace they belong to, and the
-- listing's subquery reads `trace_id` for a project: both ride the first two
-- columns. `observation_id` is the third because the ingest path replaces one
-- span's entries on every delivery, and without it that delete scans the whole
-- trace's entries — which inside a batch of twenty spans is quadratic in the
-- batch (measured: it was a quarter of what the index costs ingest).
CREATE INDEX idx_search_entries_trace
    ON search_entries(project_id, trace_id, observation_id);

-- Contentless: a content-bearing FTS table would store every indexed payload a
-- second time, uncompressed — the one thing the payload table exists to avoid
-- (design §5.2). `contentless_delete` makes a delete a rowid delete rather than
-- a re-supply of the original text, which for a compressed payload would mean
-- decompressing it to forget it (spec 011 #3).
--
-- `unicode61 remove_diacritics 2` folds case and diacritics; positions are kept
-- (the default `detail=full`), which is what makes a phrase query answerable.
CREATE VIRTUAL TABLE search_fts USING fts5(
    body,
    content='', contentless_delete=1,
    tokenize='unicode61 remove_diacritics 2'
);

-- Whether the one-off backfill of the rows written before this migration has
-- finished (spec 011 #8). Written only when it has, so a crash halfway resumes;
-- a fresh database finds nothing to do and marks itself done in milliseconds.
CREATE TABLE search_backfill (
    id      INTEGER PRIMARY KEY CHECK (id = 1),
    done_at INTEGER
) STRICT;

INSERT INTO search_backfill (id, done_at) VALUES (1, NULL);
