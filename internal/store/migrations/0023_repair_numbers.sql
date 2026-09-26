-- Repair the numbers already stored (spec 043 #9). The counting rule of
-- Decision 4 keeps every new sum finite; what an earlier version stored stays
-- as it was until something recomputes it, and this is that something.
--
-- `COUNTED` below is Decision 4's cost expression, spelled out as the store
-- builds it (`costExpr`; a test holds the two together): a `total` that is a
-- JSON number between -1e12 and 1e12, NULL otherwise. The clock is Unix
-- nanoseconds, the unit `traces.updated_at` is stamped in.

-- Traces whose total is infinite, or whose observations carry a cost the rule
-- no longer counts: recomputed from their own rows, exactly as a late span
-- would have made them, and stamped so the next pass re-rolls their hours
-- (spec 013 #15) and the users those hours hold (spec 023 #3).
UPDATE traces
   SET total_cost = (SELECT CAST(SUM(
                              CASE WHEN json_type(o.cost_details, '$.total') IN ('integer', 'real')
                                    AND json_extract(o.cost_details, '$.total') BETWEEN -1e12 AND 1e12
                                   THEN CAST(json_extract(o.cost_details, '$.total') AS REAL) END) AS REAL)
                       FROM observations o
                      WHERE o.project_id = traces.project_id AND o.trace_id = traces.id
                        AND o.provided_cost = 1),
       updated_at = CAST(unixepoch('subsec') * 1000000000 AS INTEGER)
 WHERE abs(total_cost) > 1.7976931348623157e308
    OR (project_id, id) IN (SELECT o.project_id, o.trace_id FROM observations o
                             WHERE o.provided_cost = 1
                               AND json_extract(o.cost_details, '$.total') IS NOT NULL
                               AND NOT (json_type(o.cost_details, '$.total') IN ('integer', 'real')
                                        AND json_extract(o.cost_details, '$.total') BETWEEN -1e12 AND 1e12));

-- Traces an observation of which carries a token count the rule no longer
-- counts — negative, or past 10^9: nothing on the trace changes, only its
-- hours move. A spelling that is not the first present in its class is
-- stamped too; re-rolling an hour that did not need it costs nothing.
UPDATE traces
   SET updated_at = CAST(unixepoch('subsec') * 1000000000 AS INTEGER)
 WHERE (project_id, id) IN (SELECT o.project_id, o.trace_id FROM observations o
                             WHERE o.usage IS NOT NULL
                               AND ((json_type(o.usage, '$.input_tokens') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.input_tokens') NOT BETWEEN 0 AND 1000000000)
                             OR (json_type(o.usage, '$.prompt_tokens') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.prompt_tokens') NOT BETWEEN 0 AND 1000000000)
                             OR (json_type(o.usage, '$.input') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.input') NOT BETWEEN 0 AND 1000000000)
                             OR (json_type(o.usage, '$.output_tokens') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.output_tokens') NOT BETWEEN 0 AND 1000000000)
                             OR (json_type(o.usage, '$.completion_tokens') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.completion_tokens') NOT BETWEEN 0 AND 1000000000)
                             OR (json_type(o.usage, '$.output') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.output') NOT BETWEEN 0 AND 1000000000)
                             OR (json_type(o.usage, '$.cache_read_input_tokens') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.cache_read_input_tokens') NOT BETWEEN 0 AND 1000000000)
                             OR (json_type(o.usage, '$.cache_read_tokens') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.cache_read_tokens') NOT BETWEEN 0 AND 1000000000)
                             OR (json_type(o.usage, '$.input_cached_tokens') IN ('integer', 'real')
                                 AND json_extract(o.usage, '$.input_cached_tokens') NOT BETWEEN 0 AND 1000000000)));

-- Sums no row can produce. The pass rewrites every hour it can recompute; an
-- hour frozen past the retention window (spec 013 #11) has lost the rows it
-- would be recomputed from, so its honest value is NULL, "no data".
--
-- A token sum is impossible when it is negative — a sum that wrapped — or
-- larger than 10^9 per row the cell counts, which is where `1e300` cast to
-- the largest int64 landed. On a trace-unit cell the count is traces and the
-- tokens are their observations', so the bound assumes 10^9 tokens per trace:
-- three orders past the largest context any model offers.
UPDATE stats_hourly SET total_cost = NULL WHERE abs(total_cost) > 1.7976931348623157e308;
UPDATE stats_hourly SET input_tokens = NULL
 WHERE input_tokens < 0 OR input_tokens > 1000000000 * count;
UPDATE stats_hourly SET output_tokens = NULL
 WHERE output_tokens < 0 OR output_tokens > 1000000000 * count;
UPDATE stats_hourly SET cache_read_tokens = NULL
 WHERE cache_read_tokens < 0 OR cache_read_tokens > 1000000000 * count;
UPDATE users_hourly SET total_cost = NULL WHERE abs(total_cost) > 1.7976931348623157e308;
UPDATE users        SET total_cost = NULL WHERE abs(total_cost) > 1.7976931348623157e308;
