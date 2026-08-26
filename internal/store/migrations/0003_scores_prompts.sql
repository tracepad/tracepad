-- Scores and versioned prompts (spec 003). STRICT per spec 001 #7.
--
-- The first tables filled by something other than telemetry: an eval loop
-- grading traces, and an application publishing the prompt it runs on. Both
-- are written through the same group-commit writer as ingest (spec 003 #9),
-- so a 201 here means what a 200 on an export means — the row is on disk.

-- One judgement about a trace, an observation or a session. Targets are not
-- foreign keys: a score may grade a trace whose spans are still in flight,
-- and refusing it would force clients to poll (spec 003 #4).
--
-- data_type is NOT NULL although spec 003 #5 lets the client omit it: the
-- field is always resolved before the row is built (present `value` means
-- numeric, present `string_value` means text), so a NULL here would be an
-- unreachable state, not a meaningful one.
CREATE TABLE scores (
    project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    id             TEXT NOT NULL,
    trace_id       TEXT,
    observation_id TEXT,
    session_id     TEXT,
    name           TEXT NOT NULL,
    data_type      TEXT NOT NULL
                   CHECK (data_type IN ('numeric', 'boolean', 'categorical', 'text')),
    value          REAL,
    string_value   TEXT,
    comment        TEXT,
    -- JSON, inline rather than a payloads row: judge config and eval context
    -- are small by nature, and inlining keeps score reads join-free
    -- (spec 003 #6).
    metadata       TEXT,
    timestamp      INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (project_id, id)
) STRICT;

CREATE INDEX idx_scores_timestamp ON scores(project_id, timestamp DESC);
CREATE INDEX idx_scores_trace ON scores(project_id, trace_id);
CREATE INDEX idx_scores_session ON scores(project_id, session_id);
-- Trend queries — one score name over time (design §5.2).
CREATE INDEX idx_scores_name ON scores(project_id, name, timestamp);

-- Append-only per (project, name): a version is never edited, and a rollback
-- is a label move rather than a rewrite (spec 003 #10). The version number is
-- assigned inside the write transaction, which is what makes it gapless.
CREATE TABLE prompts (
    project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    version        INTEGER NOT NULL,
    type           TEXT NOT NULL CHECK (type IN ('text', 'chat')),
    prompt         TEXT NOT NULL,
    config         TEXT,
    commit_message TEXT,
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (project_id, name, version)
) STRICT;

-- One row per label, so "which version is production" is a single-row answer
-- and a promotion is a single-row write (spec 003 #12). `latest` is never
-- stored: it is computed at read time (spec 003 #11).
--
-- No foreign key to prompts(project_id, name, version): the write path checks
-- the version inside the same transaction anyway (that check is where the 404
-- text comes from), and a second cascade path through this table would make
-- the order of project-level deletes matter.
CREATE TABLE prompt_labels (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    label      TEXT NOT NULL,
    version    INTEGER NOT NULL,
    PRIMARY KEY (project_id, name, label)
) STRICT;
