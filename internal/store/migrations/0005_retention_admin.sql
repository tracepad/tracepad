-- Retention and administration (spec 005). STRICT per spec 001 #7.
--
-- Two tables are rebuilt rather than altered. STRICT tables cannot have a
-- column constraint altered, which `projects.retention_days` needs
-- (NOT NULL DEFAULT 30 becomes nullable, spec 005 #2), and `ALTER TABLE ADD
-- COLUMN` cannot add a NOT NULL column without leaving a default behind,
-- which `traces.ingested_at` must not have (spec 005 Decision 16): a default
-- there would silently date a forgotten insert to 1970 and hand it to the
-- next sweep. Rebuilt, the column has no default, so an insert that cannot
-- say when it arrived fails loudly instead.
--
-- The rebuilds run with foreign keys disabled (migrate.go), which the
-- `projects` one requires: with them on, DROP TABLE performs an implicit
-- DELETE FROM, and every ON DELETE CASCADE hanging off projects would take
-- the whole database with it.

-- The retention clock is arrival, not the client's clock (spec 005 #1).
-- `timestamp` is client bytes; only the server can say how long we have held
-- what we received. Backfilled as min(timestamp, migration time): a client
-- that sent the future must not buy itself immortality.
CREATE TABLE traces_new (
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    id                TEXT NOT NULL,
    name              TEXT,
    user_id           TEXT,
    session_id        TEXT,
    environment       TEXT NOT NULL DEFAULT 'default',
    tags              TEXT,
    metadata_id       INTEGER REFERENCES payloads(id),
    timestamp         INTEGER,
    ingested_at       INTEGER NOT NULL,
    total_cost        REAL,
    latency_ms        INTEGER,
    error_count       INTEGER NOT NULL DEFAULT 0,
    observation_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, id)
) STRICT;

INSERT INTO traces_new (project_id, id, name, user_id, session_id, environment,
                        tags, metadata_id, timestamp, ingested_at, total_cost,
                        latency_ms, error_count, observation_count)
     SELECT project_id, id, name, user_id, session_id, environment,
            tags, metadata_id, timestamp,
            MIN(COALESCE(timestamp, CAST(strftime('%s', 'now') AS INTEGER) * 1000000000),
                CAST(strftime('%s', 'now') AS INTEGER) * 1000000000),
            total_cost, latency_ms, error_count, observation_count
       FROM traces;

DROP TABLE traces;
ALTER TABLE traces_new RENAME TO traces;

-- The read-path indexes of 0002 and 0004, recreated as the rebuild dropped
-- them. The tie-break in idx_traces_timestamp is what keeps keyset pagination
-- a seek rather than a sort (spec 003 #25, spec 004 #4).
CREATE INDEX idx_traces_timestamp ON traces(project_id, timestamp DESC, id DESC);
CREATE INDEX idx_traces_user ON traces(project_id, user_id);
CREATE INDEX idx_traces_session ON traces(project_id, session_id);
CREATE INDEX idx_traces_environment ON traces(project_id, environment);

-- The sweep window: "this project's traces older than N days", oldest first.
-- Without it every hourly pass is a full scan of the project (spec 005, Data
-- contract; the EXPLAIN QUERY PLAN method of spec 003 #25).
CREATE INDEX idx_traces_ingested ON traces(project_id, ingested_at);

-- `retention_days` becomes nullable and every existing row becomes NULL —
-- keep forever (spec 005 #2). The 0001 default of 30 never acted, because no
-- sweeper existed; setting it to NULL is what preserves every running
-- deployment's observed behavior now that one does.
--
-- `raw_retention_days` NULL means "follow retention_days" (#6); `deleted_at`
-- is the soft-delete stamp whose grace window the sweeper purges after (#9).
CREATE TABLE projects_new (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL UNIQUE,
    retention_days     INTEGER,
    raw_retention_days INTEGER,
    deleted_at         INTEGER,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

INSERT INTO projects_new (id, name, retention_days, raw_retention_days, deleted_at, created_at)
     SELECT id, name, NULL, NULL, NULL, created_at FROM projects;

DROP TABLE projects;
ALTER TABLE projects_new RENAME TO projects;
