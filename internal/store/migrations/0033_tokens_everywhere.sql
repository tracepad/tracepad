-- Tokens where people look (spec 049).
--
-- Tokens are the one measure of usage almost every generation reports, and
-- until now they lived only in `stats_hourly`. This migration gives them to
-- the trace, to both per-user tables, and adds the two classes spec 031 left
-- on the observation (#1): reasoning and cache write. No class is ever added
-- to another, so each is a column of its own, nullable for `total_cost`'s
-- reason (spec 002 #14): NULL is "nothing carried that count", not zero.

-- The trace's five, maintained on write by `refreshAggregates` (#2).
ALTER TABLE traces ADD COLUMN input_tokens       INTEGER;
ALTER TABLE traces ADD COLUMN output_tokens      INTEGER;
ALTER TABLE traces ADD COLUMN cache_read_tokens  INTEGER;
ALTER TABLE traces ADD COLUMN reasoning_tokens   INTEGER;
ALTER TABLE traces ADD COLUMN cache_write_tokens INTEGER;

-- The per-user rollup and its summary roll the same five (#5).
ALTER TABLE users_hourly ADD COLUMN input_tokens       INTEGER;
ALTER TABLE users_hourly ADD COLUMN output_tokens      INTEGER;
ALTER TABLE users_hourly ADD COLUMN cache_read_tokens  INTEGER;
ALTER TABLE users_hourly ADD COLUMN reasoning_tokens   INTEGER;
ALTER TABLE users_hourly ADD COLUMN cache_write_tokens INTEGER;
ALTER TABLE users ADD COLUMN input_tokens       INTEGER;
ALTER TABLE users ADD COLUMN output_tokens      INTEGER;
ALTER TABLE users ADD COLUMN cache_read_tokens  INTEGER;
ALTER TABLE users ADD COLUMN reasoning_tokens   INTEGER;
ALTER TABLE users ADD COLUMN cache_write_tokens INTEGER;

-- The statistics already carry the first three (0018); the two new classes
-- join them in every cell (#7).
ALTER TABLE stats_hourly ADD COLUMN reasoning_tokens   INTEGER;
ALTER TABLE stats_hourly ADD COLUMN cache_write_tokens INTEGER;

-- `sort=tokens` on the user listing (#6): the headline number of #3, a user
-- with none read as 0 so the keyset needs no NULL shape. The expression is
-- `userTokensKey` in the store, character for character in meaning: the
-- planner reads the order off this index only for the same expression.
CREATE INDEX idx_users_tokens ON users(project_id,
    (coalesce(input_tokens, 0) + coalesce(output_tokens, 0)) DESC, user_id);

-- The backfill of the trace columns (#4), by the rule `refreshAggregates`
-- sums with. `tracepad_token` is that rule, registered by the binary
-- (`UsageTokens` in tokens.go):
-- per class, over the observations that name a model (spec 031 #12), each
-- count read under the first key of its class that is present and counted
-- only within 0..10^9 (spec 043 #4). One grouped pass over the observations
-- in index order rather than a subquery per trace, and only the traces some
-- class is present on are written: the rest stay NULL, which is their value.
WITH sums(project_id, trace_id, input_tokens, output_tokens, cache_read_tokens,
          reasoning_tokens, cache_write_tokens) AS (
    SELECT o.project_id, o.trace_id,
           SUM(tracepad_token(o.usage, 0)),
           SUM(tracepad_token(o.usage, 1)),
           SUM(tracepad_token(o.usage, 2)),
           SUM(tracepad_token(o.usage, 3)),
           SUM(tracepad_token(o.usage, 4))
      FROM observations o
     WHERE o.model IS NOT NULL AND o.model != ''
     GROUP BY o.project_id, o.trace_id
)
UPDATE traces
   SET input_tokens       = sums.input_tokens,
       output_tokens      = sums.output_tokens,
       cache_read_tokens  = sums.cache_read_tokens,
       reasoning_tokens   = sums.reasoning_tokens,
       cache_write_tokens = sums.cache_write_tokens
  FROM sums
 WHERE traces.project_id = sums.project_id AND traces.id = sums.trace_id
   AND (sums.input_tokens IS NOT NULL OR sums.output_tokens IS NOT NULL
        OR sums.cache_read_tokens IS NOT NULL OR sums.reasoning_tokens IS NOT NULL
        OR sums.cache_write_tokens IS NOT NULL);

-- The rollups' backfill, in one line (spec 023 #15, as 0013, 0015, 0016,
-- 0018 and 0025 did): the first pass after the upgrade finds every hour below
-- the watermark dirty and re-rolls it whole into `stats_hourly`,
-- `users_hourly` and `users`. `rolled_until` is not touched, so the read seam
-- keeps answering from the rollup meanwhile.
--
-- Known limit, as 0018 recorded: an hour past the project's `retention_days`
-- is frozen (spec 013 #14) and keeps NULL for the new classes and the new
-- columns for ever — its observations are gone.
UPDATE stats_rollup SET last_pass = 0;
