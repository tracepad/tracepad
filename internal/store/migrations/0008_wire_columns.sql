-- What the wire already carries (spec 012). STRICT per spec 001 #7.
--
-- Everything here changes the stored shape of data, which is why it lands
-- now: the public release is deferred until after iteration 2, so no database
-- exists outside the developers' machines and no column has to be backfilled
-- from raw bodies (spec 012 #1). After the release the same three columns
-- would cost a migration on data we do not own.
--
-- The rebuild below runs with foreign keys disabled (migrate.go) and is
-- followed by `PRAGMA foreign_key_check` there, which is what stands between
-- a mistyped SELECT list and a database whose payload references dangle.

-- traces: three nullable columns and the index behind the release filter.
--
-- `release` and `version` are per-field trace columns like every other one
-- (spec 002 #6); `ttft_ms` is an aggregate the ingest transaction recomputes
-- beside latency and cost (spec 002 #22). All three are NULL on every row
-- written before this migration and stay NULL until the trace is delivered
-- again: `tracepad remap` is out of scope, and a backfill from raw bodies is
-- the expensive kind this spec exists to avoid (spec 012 #1).
ALTER TABLE traces ADD COLUMN release TEXT;
ALTER TABLE traces ADD COLUMN version TEXT;
ALTER TABLE traces ADD COLUMN ttft_ms INTEGER;

-- "Did the deploy break it" is a filter over one project's traces, and
-- without an index it is a scan of the project (spec 012 #4; the
-- EXPLAIN QUERY PLAN method of spec 003 #25).
CREATE INDEX idx_traces_release ON traces(project_id, release);

-- observations: rebuilt rather than altered, because SQLite cannot alter a
-- CHECK constraint and the type vocabulary is what widens (spec 012 #2, #10).
-- The rebuild follows the pattern schema 0005 set for `traces_new`.
--
-- The ten values are the Langfuse observation vocabulary. Until now the
-- mapper collapsed them onto three and kept the original spelling in metadata
-- so that exactly this widening would be possible later (spec 002 #21); rows
-- written before this migration keep their collapsed type, since nothing
-- rereads the raw bodies.
--
-- `completion_start_time` is Unix nanoseconds like the other two instants,
-- stored as sent — no clamping and no clock correction (spec 002 #4), so a
-- completion that claims to precede its own span survives as the number the
-- client sent.
--
-- `prompt_name` and `prompt_version` carry no foreign key to `prompts`: they
-- record what the client said, whether or not this store manages that prompt
-- (spec 012 #5).
CREATE TABLE observations_new (
    project_id            TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trace_id              TEXT NOT NULL,
    id                    TEXT NOT NULL,
    parent_observation_id TEXT,
    type                  TEXT NOT NULL CHECK (type IN (
                              'span', 'generation', 'event', 'agent', 'tool', 'chain',
                              'retriever', 'guardrail', 'evaluator', 'embedding')),
    name                  TEXT,
    start_time            INTEGER,
    end_time              INTEGER,
    completion_start_time INTEGER,
    model                 TEXT,
    model_parameters      TEXT,
    level                 TEXT NOT NULL DEFAULT 'DEFAULT'
                          CHECK (level IN ('DEBUG', 'DEFAULT', 'WARNING', 'ERROR')),
    status_message        TEXT,
    usage                 TEXT,
    cost_details          TEXT,
    provided_cost         INTEGER NOT NULL DEFAULT 0,
    prompt_name           TEXT,
    prompt_version        INTEGER,
    input_id              INTEGER REFERENCES payloads(id),
    output_id             INTEGER REFERENCES payloads(id),
    metadata_id           INTEGER REFERENCES payloads(id),
    PRIMARY KEY (project_id, trace_id, id)
) STRICT;

INSERT INTO observations_new (project_id, trace_id, id, parent_observation_id, type, name,
                              start_time, end_time, completion_start_time, model,
                              model_parameters, level, status_message, usage, cost_details,
                              provided_cost, prompt_name, prompt_version,
                              input_id, output_id, metadata_id)
     SELECT project_id, trace_id, id, parent_observation_id, type, name,
            start_time, end_time, NULL, model,
            model_parameters, level, status_message, usage, cost_details,
            provided_cost, NULL, NULL,
            input_id, output_id, metadata_id
       FROM observations;

DROP TABLE observations;
ALTER TABLE observations_new RENAME TO observations;

-- The indexes of 0002 and 0004, recreated as the rebuild dropped them: the
-- trace's own spans (the tree read and every aggregate refresh) and the span
-- id alone (`/observations/{id}/io` and its ambiguity check).
CREATE INDEX idx_observations_trace ON observations(project_id, trace_id);
CREATE INDEX idx_observations_span ON observations(project_id, id);

-- The two filters spec 012 #8 adds to the trace listing are EXISTS subqueries
-- over this table, and a subquery that scans a trace's spans per listed row is
-- a disaster on the capped count (spec 009 #4). `trace_id` is the last column
-- of each because that is what the subquery returns after seeking the value.
CREATE INDEX idx_observations_type ON observations(project_id, type, trace_id);

-- Partial, because most observations carry no prompt: the index costs writes
-- only for the ones that do (spec 012 #8).
CREATE INDEX idx_observations_prompt ON observations(project_id, prompt_name, prompt_version, trace_id)
    WHERE prompt_name IS NOT NULL;
