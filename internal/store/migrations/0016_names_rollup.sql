-- Trace names, rolled up beside the traffic (spec 027 #3).
--
-- Two of the three columns a facet answer covers — `environment` and
-- `release` — are already in `stats_hourly`'s dimension tuple. The third is
-- not, and putting it there would multiply that table by the number of names a
-- project uses on every row, on behalf of a column none of the statistics
-- queries reads. A table of its own with the smallest tuple that answers the
-- question is spec 025's move one more time.

-- One row per (project, hour, name). A trace with no name is not in it: it is
-- absent from the name facet, and `?name=` never matches it either (spec 027,
-- edge cases).
--
-- `error_count` rides because it is free — the same `GROUP BY` produces it —
-- and because "which trace names fail" is the next question this table
-- answers.
--
-- Cardinality is the hour × the names a project uses, which is a short, finite
-- set per project; a deployment that puts a request id in the trace name pays
-- for that choice linearly, exactly as spec 013 #1 says of releases.
CREATE TABLE names_hourly (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour        INTEGER NOT NULL,   -- Unix seconds, top of the hour, UTC
    name        TEXT NOT NULL,
    count       INTEGER NOT NULL,
    error_count INTEGER NOT NULL,
    PRIMARY KEY (project_id, hour, name)
) STRICT;

-- The backfill, in one line (spec 027 #3, on spec 023 #15's and spec 025 #5's
-- reasoning).
--
-- On an existing install the watermark is already past the whole history:
-- every hour is rolled, none is dirty, and the table above would stay empty
-- until new traffic arrived — for ever, on a project that has stopped
-- receiving any. Resetting `last_pass` says "everything changed since the last
-- pass", so the first pass after the upgrade finds every hour below the
-- watermark dirty and re-rolls it, writing the name rows and rewriting the
-- identical statistics, per-user and score ones: an hour is recomputed whole
-- and is idempotent by construction (spec 013 #3).
--
-- `rolled_until` is deliberately not touched, for spec 023 #15's reason:
-- moving it back would make the read seam answer from the live scan meanwhile,
-- and for hours whose traces retention has taken the live scan finds nothing.
--
-- An hour past the project's trace-retention window is frozen for this table
-- exactly when this table already holds rows for it (spec 025 #21, spec 026
-- #7), so the backfill fills an hour whose traces are intact and cannot
-- demolish one whose name rows already stand. An hour whose traces retention
-- really did take writes nothing, which is the truth.
UPDATE stats_rollup SET last_pass = 0;
