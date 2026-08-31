-- Statistics that outlive the raw rows they summarize (spec 013).
--
-- The statistics screen answers from a full scan of `traces` and
-- `observations`, which is a scan of tens of millions of rows for a month's
-- chart at the envelope the README publishes — and which returns nothing at
-- all once a retention window has deleted the rows the numbers lived in.
-- These two tables are the answer to both: thousands of rows a month, read by
-- their primary key, written by a background aggregator, and untouched by the
-- trace sweep.

-- One row per dimension tuple per hour. `model = ''` marks a trace-unit row
-- (the hour, day, environment and release groupings); a row with a model
-- aggregates observations, which is the other unit the API already
-- distinguishes (spec 013 #1, spec 004 #23).
--
-- `latency` is a JSON array of bucket counts (spec 013 #2, #10): a percentile
-- cannot be summed and a histogram can, which is what lets a day be 24 hours
-- merged and lets a re-rolled hour overwrite its own row.
--
-- `total_cost` is nullable for the reason it is nullable everywhere else: an
-- hour in which nothing carried a cost has no cost, and zero would be a
-- different claim (spec 002 #14).
CREATE TABLE stats_hourly (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour        INTEGER NOT NULL,
    environment TEXT NOT NULL,
    release     TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    count       INTEGER NOT NULL,
    error_count INTEGER NOT NULL,
    total_cost  REAL,
    latency     TEXT NOT NULL,
    PRIMARY KEY (project_id, hour, environment, release, model)
) STRICT;

-- The aggregator's bookkeeping. `rolled_until` is the watermark the read path
-- splits on: hours before it are answered from the table above, hours at or
-- past it from the live scan, so the first pass on an existing database is
-- the backfill and no query degrades while it runs (spec 013 #5).
--
-- `last_pass` is the cutoff that finds the hours late spans touched: a trace
-- ingested after it belongs to an hour that has to be rolled again
-- (spec 013 #4).
CREATE TABLE stats_rollup (
    project_id   TEXT NOT NULL PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    rolled_until INTEGER NOT NULL,
    last_pass    INTEGER NOT NULL
) STRICT;

-- When a trace was last written to, which is not when it arrived. The two
-- differ for the ordinary shape of traffic: spans of one trace come in several
-- exports, and `ingested_at` deliberately does not move for them, because a
-- retention lease starts once (spec 005 #1). The rollup asks the opposite
-- question — "what changed since my last pass" — and only this column answers
-- it (spec 013 #15).
--
-- Backfilled from `ingested_at`: before this migration nothing was rolled, so
-- every hour is rolled for the first time anyway.
ALTER TABLE traces ADD COLUMN updated_at INTEGER;
UPDATE traces SET updated_at = ingested_at;

CREATE INDEX idx_traces_updated ON traces(project_id, updated_at);

-- The aggregates are the cheap thing and they outlive the expensive thing
-- (spec 013 #6), so they get a window of their own. NULL — the default, and
-- what every existing project takes — is keep forever, matching the posture
-- `retention_days` already has (spec 005 #2).
ALTER TABLE projects ADD COLUMN stats_retention_days INTEGER;
