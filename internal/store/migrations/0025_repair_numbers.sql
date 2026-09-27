-- Repair the numbers already stored (spec 043 #9). The counting rule of
-- Decision 4 keeps every new sum finite; what an earlier version stored stays
-- as it was until something recomputes it, and this is that something.
--
-- The cost expression below is Decision 4's, spelled out as the store builds
-- it (`costExpr`; a test holds the two together): a `total` that is a number
-- between -1e12 and 1e12 — a JSON number, or a string whose text is one, which
-- SQLite's `SUM` always counted and still counts (spec 043 #24) — and NULL
-- otherwise. The clock is Unix nanoseconds, the unit `traces.updated_at` is
-- stamped in.

-- One pass over the observations finds every trace the repair touches:
-- `cost` marks a trace an observation of which carries a `total` the rule does
-- not count — not a number, or outside the domain — and `tokens` one that
-- carries a token count the rule does not count — negative, or past 10^9 —
-- under any spelling of the three classes (re-rolling an hour that did not
-- need it costs nothing). One scan, and one parse of `usage` rather than one
-- per key: about three seconds for a million observations that all carry both
-- (spec 043 #24).
CREATE TEMP TABLE repair_numbers (
    project_id TEXT NOT NULL,
    trace_id   TEXT NOT NULL,
    cost       INTEGER NOT NULL
);
INSERT INTO repair_numbers (project_id, trace_id, cost)
SELECT project_id, trace_id, cost
  FROM (SELECT o.project_id, o.trace_id,
               o.provided_cost = 1
               AND json_extract(o.cost_details, '$.total') IS NOT NULL
               AND (CASE WHEN (CASE WHEN json_type(o.cost_details, '$.total') IN ('integer', 'real')
                     THEN json_extract(o.cost_details, '$.total')
                     WHEN json_type(o.cost_details, '$.total') = 'text'
                          AND json_valid(json_extract(o.cost_details, '$.total'))
                          AND json_type(json_extract(o.cost_details, '$.total')) IN ('integer', 'real')
                     THEN json_extract(json_extract(o.cost_details, '$.total'), '$') END) BETWEEN -1e12 AND 1e12
          THEN CAST((CASE WHEN json_type(o.cost_details, '$.total') IN ('integer', 'real')
                     THEN json_extract(o.cost_details, '$.total')
                     WHEN json_type(o.cost_details, '$.total') = 'text'
                          AND json_valid(json_extract(o.cost_details, '$.total'))
                          AND json_type(json_extract(o.cost_details, '$.total')) IN ('integer', 'real')
                     THEN json_extract(json_extract(o.cost_details, '$.total'), '$') END) AS REAL) END) IS NULL AS cost,
               o.usage IS NOT NULL
               AND EXISTS (SELECT 1 FROM json_each(o.usage) u
                            WHERE u.key IN ('input_tokens', 'prompt_tokens', 'input',
                                            'output_tokens', 'completion_tokens', 'output',
                                            'cache_read_input_tokens', 'cache_read_tokens', 'input_cached_tokens')
                              AND u.type IN ('integer', 'real')
                              AND u.value NOT BETWEEN 0 AND 1000000000) AS tokens
          FROM observations o)
 WHERE cost OR tokens;

-- Traces whose total is infinite, or whose observations carry a cost the rule
-- does not count: recomputed from their own rows, exactly as a late span would
-- have made them, and stamped so the next pass re-rolls their hours (spec 013
-- #15) and the users those hours hold (spec 023 #3).
UPDATE traces
   SET total_cost = (SELECT CAST(SUM(CASE WHEN (CASE WHEN json_type(o.cost_details, '$.total') IN ('integer', 'real')
                     THEN json_extract(o.cost_details, '$.total')
                     WHEN json_type(o.cost_details, '$.total') = 'text'
                          AND json_valid(json_extract(o.cost_details, '$.total'))
                          AND json_type(json_extract(o.cost_details, '$.total')) IN ('integer', 'real')
                     THEN json_extract(json_extract(o.cost_details, '$.total'), '$') END) BETWEEN -1e12 AND 1e12
          THEN CAST((CASE WHEN json_type(o.cost_details, '$.total') IN ('integer', 'real')
                     THEN json_extract(o.cost_details, '$.total')
                     WHEN json_type(o.cost_details, '$.total') = 'text'
                          AND json_valid(json_extract(o.cost_details, '$.total'))
                          AND json_type(json_extract(o.cost_details, '$.total')) IN ('integer', 'real')
                     THEN json_extract(json_extract(o.cost_details, '$.total'), '$') END) AS REAL) END) AS REAL)
                       FROM observations o
                      WHERE o.project_id = traces.project_id AND o.trace_id = traces.id
                        AND o.provided_cost = 1),
       updated_at = CAST(unixepoch('subsec') * 1000000000 AS INTEGER)
 WHERE abs(total_cost) > 1.7976931348623157e308
    OR (project_id, id) IN (SELECT project_id, trace_id FROM repair_numbers WHERE cost);

-- The token counts: nothing on the trace changes, only its hours move.
UPDATE traces
   SET updated_at = CAST(unixepoch('subsec') * 1000000000 AS INTEGER)
 WHERE (project_id, id) IN (SELECT project_id, trace_id FROM repair_numbers WHERE NOT cost);

DROP TABLE repair_numbers;

-- Sums no row can produce. The pass rewrites every hour it can recompute; an
-- hour frozen past the retention window (spec 013 #11) has lost the rows it
-- would be recomputed from, so its honest value is NULL, "no data".
--
-- A token sum is impossible when it is negative — a sum that wrapped — or,
-- on a model cell, larger than 10^9 for each observation it counts. A cell
-- with no model counts traces, and its tokens are every observation of them:
-- an agent's trace of three thousand calls honestly sums past 10^9, so no
-- bound per row holds there, and the one value nulled on it is the largest
-- int64 itself — what `CAST(1e300 AS INTEGER)` stored and no honest sum lands
-- on (spec 043 #24). The traces behind every impossible sum are stamped above,
-- so the hours the pass can recompute are recomputed whatever this leaves.
UPDATE stats_hourly SET total_cost = NULL WHERE abs(total_cost) > 1.7976931348623157e308;
UPDATE stats_hourly SET input_tokens = NULL
 WHERE input_tokens < 0 OR input_tokens = 9223372036854775807
    OR (model != '' AND input_tokens > 1000000000 * count);
UPDATE stats_hourly SET output_tokens = NULL
 WHERE output_tokens < 0 OR output_tokens = 9223372036854775807
    OR (model != '' AND output_tokens > 1000000000 * count);
UPDATE stats_hourly SET cache_read_tokens = NULL
 WHERE cache_read_tokens < 0 OR cache_read_tokens = 9223372036854775807
    OR (model != '' AND cache_read_tokens > 1000000000 * count);
UPDATE users_hourly SET total_cost = NULL WHERE abs(total_cost) > 1.7976931348623157e308;
UPDATE users        SET total_cost = NULL WHERE abs(total_cost) > 1.7976931348623157e308;
