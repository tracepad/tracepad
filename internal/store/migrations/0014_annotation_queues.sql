-- What to review, who reviewed it (spec 024).
--
-- Spec 022 lets a person score the trace they happen to be reading. A review
-- programme is the other way round: somebody decides *which* traces deserve a
-- human verdict, several people work through them, and the team can see what
-- is done and what is left. These two tables are that list and its
-- bookkeeping; the verdicts themselves stay in `scores`, where every other
-- judgement about a trace already lives (#3).

-- A queue is a question ("rate these on accuracy and tone") and the score
-- configs are the question's shape (#1): the names a reviewer must set on
-- every item, in the order the desk asks for them. Referenced by name, the
-- binding spec 003 chose for scores, so the queue never disagrees with what a
-- score of that name means — and a config deleted later leaves the name here
-- untouched, exactly as it leaves the scores it admitted (spec 014 #17).
--
-- Declarative like score configs: a `PUT` replaces the row whole, so a team
-- keeps its queues in a file beside them.
CREATE TABLE annotation_queues (
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    score_configs TEXT NOT NULL,              -- JSON array of config names, in order
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (project_id, name)
) STRICT;

-- An item is one trace, or one observation of a trace (#2) — the unit spec 022
-- scores and spec 016 adds to datasets, for the same reason: in an agent trace
-- the thing to judge is often one generation.
--
-- `seq` is MAX(seq) + 1 per queue, assigned inside the write transaction, the
-- rule spec 003 uses for prompt versions: the writer is the only goroutine
-- committing, which is what makes the clock gapless without a lock of its own.
--
-- The UNIQUE is what makes adding idempotent: a filter re-run and a script
-- retried do not double the queue, and the second add answers with the item
-- already there. `observation_id` is NULL for a trace item, and SQLite's
-- UNIQUE treats NULLs as distinct — which is fine here because the pair is
-- either both-columns-set or trace-only, and a trace-only target is caught by
-- the lookup the add does first, in the same transaction.
--
-- No foreign key to `traces`: the target need not exist yet (spec 003's rule
-- for scores), because a queue may be filled from a script that has not
-- finished exporting. Items follow their trace all the same (#3) — the sweep,
-- the erasure and the purge all delete them — which is what
-- `idx_annotation_items_trace` is for.
CREATE TABLE annotation_items (
    project_id     TEXT NOT NULL,
    queue          TEXT NOT NULL,
    id             TEXT NOT NULL,             -- 32 hex, server-generated
    trace_id       TEXT NOT NULL,
    observation_id TEXT,                      -- NULL = the trace itself
    status         TEXT NOT NULL,             -- pending | completed | skipped
    seq            INTEGER NOT NULL,
    added_at       INTEGER NOT NULL,
    claimed_by     TEXT,
    claimed_until  INTEGER,
    completed_by   TEXT,
    completed_at   INTEGER,
    skip_reason    TEXT,
    PRIMARY KEY (project_id, id),
    FOREIGN KEY (project_id, queue) REFERENCES annotation_queues(project_id, name) ON DELETE CASCADE,
    UNIQUE (project_id, queue, trace_id, observation_id)
) STRICT;

-- `next` hands out the oldest pending item of one queue (#5), and the items
-- table is the one listing that reads in `seq` order: this index answers both
-- as a seek, and the status counts of the queue listing as a scan of one
-- prefix rather than of the table.
CREATE INDEX idx_annotation_items_next  ON annotation_items(project_id, queue, status, seq);
-- "Which items point at this trace" — the join the retention sweep's delete
-- makes, and the erasure path's. Without it a sweep chunk would scan every
-- item in the project per trace it takes.
CREATE INDEX idx_annotation_items_trace ON annotation_items(project_id, trace_id);
