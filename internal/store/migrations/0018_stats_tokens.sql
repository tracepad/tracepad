-- Tokens in the statistics (spec 031 #3).
--
-- The statistics knew cost and latency but not a single token — the one
-- number every provider reports, and the only one a user of a provider
-- without a price has. Three sums per cell, read off each observation's
-- `usage` under one closed list of spellings per class (spec 031 #1) and
-- summed into both units the row already distinguishes (#2): the model cell
-- the observation is in, and the trace-unit cell its trace is in.
--
-- Nullable, for the reason `total_cost` is (spec 002 #14): NULL is "nothing
-- in this cell carried that count", which is not the claim that it carried
-- zero — a cell whose calls reported no usage must not chart as zero tokens
-- beside a cell that reported few.
--
-- Reasoning and cache-creation counts are deliberately absent: reasoning is
-- inside the output for most providers and beside it for some, so a sum
-- would double-count or under-count depending on who sent it, and cache
-- creation is a fact about one provider's billing that the observation panel
-- already shows.
ALTER TABLE stats_hourly ADD COLUMN input_tokens INTEGER;
ALTER TABLE stats_hourly ADD COLUMN output_tokens INTEGER;
ALTER TABLE stats_hourly ADD COLUMN cache_read_tokens INTEGER;

-- The backfill, in one line (spec 031 #3, on spec 023 #15's reasoning, as
-- 0013, 0015 and 0016 did).
--
-- On an existing install every hour is rolled and none is dirty, so the
-- three columns above would stay NULL until new traffic arrived — for ever,
-- on a project that has stopped receiving any. Resetting `last_pass` says
-- "everything changed since the last pass", so the first pass after the
-- upgrade finds every hour below the watermark dirty and re-rolls it: an hour
-- is recomputed whole and is idempotent by construction (spec 013 #3), so the
-- pass rewrites identical counts and fills the tokens in.
--
-- `rolled_until` is deliberately not touched: moving it back would make the
-- read seam answer from the live scan meanwhile, and for hours whose traces
-- retention has taken the live scan finds nothing.
--
-- Known limit: an hour past the project's `retention_days` is frozen
-- (spec 013 #14) and keeps NULL tokens for ever — its observations are gone,
-- and there is nothing to sum. `docs/api.md` says so.
UPDATE stats_rollup SET last_pass = 0;
