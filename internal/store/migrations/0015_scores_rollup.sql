-- Quality, rolled up beside the traffic (spec 025).
--
-- A score lives on one trace, and "did hallucination drop after 2.5.0" was
-- answerable only by the scan the design forbids at scale. This table is spec
-- 013's answer one dimension further along: the same hourly grain, the same
-- aggregator pass, the same watermark, with the score's name, type and
-- category in the tuple.

-- Spec 013's dimension tuple plus `name`, `data_type` and `category`
-- (spec 025 #1). A score is counted in **the hour of the trace it names** —
-- not in the hour it was graded — so the curve lines up with the traffic and
-- the cost curves on the Stats screen, and so every row has an environment, a
-- release and a model without inventing any.
--
-- `model` is spec 013 #1's discriminator read the same way: '' for a score
-- that names no observation (or one whose observation carries no model), and
-- otherwise the observation's model. Unlike `stats_hourly`, the two are not
-- two units here — every score is in exactly one row — so a grouping that is
-- not by model sums the lot and counts each score once.
--
-- Two kinds of score are absent by design: one that names no trace (a
-- session-only score, which spec 003 #4 allows) has no trace to borrow the
-- tuple from, and a `text` score has nothing to add up. `docs/quality.md`
-- says both.
--
-- Cardinality is the spec 013 tuple × names × (categories for a categorical
-- name), and a name with thousands of distinct categories pays for that
-- linearly, as spec 013 #1 says of releases.
CREATE TABLE scores_hourly (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour        INTEGER NOT NULL,          -- Unix seconds, top of the hour, UTC: the trace's hour
    environment TEXT NOT NULL,
    release     TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',  -- '' = the score names no observation with a model
    name        TEXT NOT NULL,
    data_type   TEXT NOT NULL CHECK (data_type IN ('numeric', 'boolean', 'categorical')),
    category    TEXT NOT NULL DEFAULT '',  -- categorical: the string_value; '' otherwise
    -- Four numbers that merge exactly across hours (spec 025 #2): the mean of
    -- a day is Σsum / Σcount over its 24 rows, the rate likewise, and the
    -- extremes are extremes. A distribution does not follow from these and is
    -- deliberately left out; this row can grow a JSON histogram column later
    -- exactly as `stats_hourly` carries its `latency`.
    count       INTEGER NOT NULL,
    sum         REAL NOT NULL,             -- numeric: Σ value; boolean: the number of 1s; categorical: 0
    min         REAL,                      -- numeric only
    max         REAL,                      -- numeric only
    PRIMARY KEY (project_id, hour, environment, release, model, name, data_type, category)
) STRICT;

-- The primary key answers "this hour, every name"; this index answers "this
-- name, these hours", which is what the detail view of one score asks.
CREATE INDEX idx_scores_hourly_name ON scores_hourly(project_id, name, hour);

-- The score-side dirty set (spec 025 #3).
--
-- A score's arrival touches no trace row: a judge grading yesterday's traffic
-- today writes only `scores`, and `traces.updated_at` — spec 013 #15's whole
-- mechanism — never hears of it. So a pass also asks which scores were
-- written since the last one, and dirties the hour of each one's trace. An
-- upsert moves `created_at` too (spec 003 #3), so a corrected score is found
-- by the same question.
CREATE INDEX idx_scores_created ON scores(project_id, created_at);

-- The backfill, in one line (spec 025 #5, spec 023 #15's).
--
-- On an existing install the watermark is already past the whole history:
-- every hour is rolled, none is dirty, and the table above would stay empty
-- until new traffic arrived. Resetting `last_pass` says "everything changed
-- since the last pass", so the first pass after the upgrade finds every hour
-- below the watermark dirty and re-rolls it — writing the score rows and
-- rewriting the identical statistics and per-user ones, which costs a pass and
-- changes nothing: an hour is recomputed whole and is idempotent by
-- construction (spec 013 #3).
--
-- `rolled_until` is deliberately not touched, for spec 023 #15's reason:
-- moving it back would make the read seam answer from the live scan meanwhile,
-- and for hours whose traces retention has taken the live scan finds nothing.
--
-- Hours past the project's trace-retention window are frozen (spec 013 #11)
-- and get no score rows, ever. `docs/quality.md` says so.
UPDATE stats_rollup SET last_pass = 0;
