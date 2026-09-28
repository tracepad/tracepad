-- An erasure is a task (spec 047 #6): the confirmed request records one and a
-- worker runs it, writing its progress in the transactions that make it
-- (#12), so that a restart resumes it from its phase. The row forgets the
-- user id when the erasure ends (#9), and goes 30 days after (#15).
CREATE TABLE erasures (
    id              TEXT NOT NULL PRIMARY KEY,           -- 32 hex, random (#8)
    project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id         TEXT,                                -- NULL once finished (#9)
    state           TEXT NOT NULL,                       -- queued|running|done|failed
    phase           TEXT,                                -- raw|parsed|tail while running
    created_at      INTEGER NOT NULL,                    -- Unix ns
    started_at      INTEGER,
    finished_at     INTEGER,
    since           INTEGER,                             -- step 1's moment (spec 044 #20 c)
    now             INTEGER NOT NULL,                    -- the freeze clock, read once (spec 013 #11)
    attempts        INTEGER NOT NULL DEFAULT 0,          -- starts, a restart's included (#12)
    traces_at_start INTEGER,                             -- step 1's count
    counts          TEXT NOT NULL DEFAULT '{}',          -- the `deleted` keys, as committed
    compaction      INTEGER NOT NULL DEFAULT 0,          -- the latest compaction it asked for
    error           TEXT
) STRICT;
CREATE INDEX idx_erasures_project ON erasures(project_id, created_at DESC);
-- One erasure of a user at a time (#11): the id is kept only while it runs.
CREATE UNIQUE INDEX idx_erasures_running ON erasures(project_id, user_id)
    WHERE user_id IS NOT NULL;
CREATE INDEX idx_erasures_state ON erasures(state, created_at)
    WHERE state IN ('queued', 'running');

-- What the tail of an erasure scrubs (spec 044 #4, #20 c): each trace a chunk
-- deleted that received a batch step 2 did not read, with the arrival window
-- to read. Written by the chunk that deleted it (spec 047 #12), so a resumed
-- erasure finishes the tail a repeat could not; the trace id is what picks
-- its spans out of a batch. Gone when the erasure ends.
CREATE TABLE erasure_tail (
    erasure_id   TEXT NOT NULL REFERENCES erasures(id) ON DELETE CASCADE,
    trace_id     TEXT NOT NULL,
    arrived_from INTEGER NOT NULL,                       -- Unix ns
    arrived_to   INTEGER NOT NULL,
    PRIMARY KEY (erasure_id, trace_id)
) STRICT, WITHOUT ROWID;
