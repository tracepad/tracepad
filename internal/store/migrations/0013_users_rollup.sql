-- Who the traffic belongs to, rolled up beside the statistics (spec 023).
--
-- A user id rides on most production traces and the store could filter by it,
-- erase by it, and nothing else: "who are my heaviest users", "when did this
-- one last show up", "what does this account cost me" were answerable only by
-- the scan spec 013 exists to remove. These two tables are the same answer one
-- dimension over — the same hourly grain, the same histogram, the same
-- aggregator pass, with `user_id` in the tuple.

-- Spec 013's dimension tuple plus `user_id` (spec 023 #1). `model = ''` marks
-- a trace-unit row exactly as it does in `stats_hourly` (spec 013 #1); a row
-- with a model aggregates observations.
--
-- Traces that carry no user id are not here at all: the listing is of users,
-- and a "no user" pseudo-row is the trace listing with no filter (Out of
-- scope). Cardinality is therefore active-user-hours × environments ×
-- (models + 1), which `docs/users.md` states in numbers.
--
-- `sessions_started` counts, on trace-unit rows only, the sessions of that
-- user whose earliest trace of theirs falls in this hour. Counted where they
-- *start* because a distinct count does not merge across hours and a start
-- does: the sum over any range is exact, which is what lets a day be its 24
-- hours added up.
CREATE TABLE users_hourly (
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour             INTEGER NOT NULL,
    user_id          TEXT NOT NULL,
    environment      TEXT NOT NULL,
    release          TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT '',
    count            INTEGER NOT NULL,
    error_count      INTEGER NOT NULL,
    total_cost       REAL,
    latency          TEXT NOT NULL,
    sessions_started INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, hour, user_id, environment, release, model)
) STRICT;

-- The primary key answers "this hour, every user"; this index answers "this
-- user, these hours", which is what the user page and `/stats?user_id=` ask.
CREATE INDEX idx_users_hourly_user ON users_hourly(project_id, user_id, hour);

-- The summary the listing sorts and pages over (spec 023 #3). A keyset cursor
-- needs a row per user with the sort key on it: forming these groups on every
-- page view would be the scan again, one table further along.
--
-- Recomputed — never delta-maintained — from `users_hourly` for the users a
-- pass touched, for spec 013 #3's reason: re-delivery is routine, and a delta
-- double-counts every retried span.
CREATE TABLE users (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL,
    traces      INTEGER NOT NULL,
    error_count INTEGER NOT NULL,
    total_cost  REAL,
    sessions    INTEGER NOT NULL,
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    PRIMARY KEY (project_id, user_id)
) STRICT;

-- One index per sort the listing offers (spec 023 #5), each with `user_id`
-- after the key so that the tie-break rides the index too and a keyset page is
-- a seek rather than a sort.
CREATE INDEX idx_users_last_seen ON users(project_id, last_seen DESC, user_id);
CREATE INDEX idx_users_traces    ON users(project_id, traces DESC, user_id);
CREATE INDEX idx_users_cost      ON users(project_id, total_cost DESC, user_id);
CREATE INDEX idx_users_errors    ON users(project_id, error_count DESC, user_id);

-- The backfill, in one line.
--
-- Spec 013 #5 could say "the aggregator's first passes are the backfill"
-- because a fresh watermark makes every hour a forward roll. Here the
-- watermark is already past the whole history: every hour is rolled, none is
-- dirty, and the two tables above would stay empty until new traffic arrived —
-- for ever, for a project that has stopped receiving any.
--
-- Resetting `last_pass` says "everything changed since the last pass", so the
-- next pass finds every hour below the watermark dirty and re-rolls it. That
-- writes the per-user rows and rewrites the identical `stats_hourly` ones,
-- which costs a pass and changes nothing: an hour is recomputed whole and is
-- idempotent by construction (spec 013 #3).
--
-- `rolled_until` is deliberately *not* touched. Moving it back would make the
-- read seam answer `/api/v1/stats` from the live scan meanwhile — and for
-- hours whose traces retention has taken, the live scan finds nothing, so a
-- month's chart would read as empty for the length of one pass (spec 013 #12).
-- The dirty set reaches the same hours without moving the claim.
--
-- Two consequences, stated rather than discovered. The first pass after this
-- migration walks the whole rolled history at once, because the dirty set is
-- deliberately uncapped (spec 013 #16); it is background work behind the group
-- commit, and it happens once. And an hour past the project's trace-retention
-- window is **frozen** (spec 013 #11): its raw rows are gone, so it gets no
-- per-user rows, ever. `docs/users.md` says so.
UPDATE stats_rollup SET last_pass = 0;
