-- Trace data model (spec 002). STRICT per spec 001 #7.
--
-- Ids are lower-case hex TEXT, 32 chars for traces and 16 for spans, with
-- project_id part of every primary key (spec 002 #3). Timestamps are Unix
-- nanoseconds as delivered by OTLP (spec 002 #4).

-- Large values live here so that traces/observations rows stay narrow and
-- list scans never touch a blob (spec 002 #8). One row per value; rows are
-- owned by exactly one column of one owner row, so orphan cleanup after an
-- overwriting upsert is the retention stage's concern.
CREATE TABLE payloads (
    id          INTEGER PRIMARY KEY,
    compression TEXT NOT NULL CHECK (compression IN ('none', 'zstd')),
    size_raw    INTEGER NOT NULL,
    body        BLOB NOT NULL
) STRICT;

-- Derived from spans: OTLP has no trace message, so every column here is
-- merged field-wise from whichever span carried it (spec 002 #6). The
-- aggregates are maintained inside the ingest transaction so the trace list
-- never has to join observations (spec 002 #7).
CREATE TABLE traces (
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    id                TEXT NOT NULL,
    name              TEXT,
    user_id           TEXT,
    session_id        TEXT,
    environment       TEXT NOT NULL DEFAULT 'default',
    tags              TEXT,
    metadata_id       INTEGER REFERENCES payloads(id),
    timestamp         INTEGER,
    total_cost        REAL,
    latency_ms        INTEGER,
    error_count       INTEGER NOT NULL DEFAULT 0,
    observation_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, id)
) STRICT;

CREATE INDEX idx_traces_timestamp ON traces(project_id, timestamp DESC);
CREATE INDEX idx_traces_user ON traces(project_id, user_id);
CREATE INDEX idx_traces_session ON traces(project_id, session_id);
CREATE INDEX idx_traces_environment ON traces(project_id, environment);

-- One row per span, upserted by natural key: exporters retry, so duplicate
-- delivery is normal and last delivery wins (spec 002 #5).
CREATE TABLE observations (
    project_id            TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trace_id              TEXT NOT NULL,
    id                    TEXT NOT NULL,
    parent_observation_id TEXT,
    type                  TEXT NOT NULL CHECK (type IN ('span', 'generation', 'event')),
    name                  TEXT,
    start_time            INTEGER,
    end_time              INTEGER,
    model                 TEXT,
    model_parameters      TEXT,
    level                 TEXT NOT NULL DEFAULT 'DEFAULT'
                          CHECK (level IN ('DEBUG', 'DEFAULT', 'WARNING', 'ERROR')),
    status_message        TEXT,
    usage                 TEXT,
    cost_details          TEXT,
    provided_cost         INTEGER NOT NULL DEFAULT 0,
    input_id              INTEGER REFERENCES payloads(id),
    output_id             INTEGER REFERENCES payloads(id),
    metadata_id           INTEGER REFERENCES payloads(id),
    PRIMARY KEY (project_id, trace_id, id)
) STRICT;

CREATE INDEX idx_observations_trace ON observations(project_id, trace_id);

-- The insurance policy (spec 002 #9): the zstd-compressed protobuf of every
-- accepted export, so a mapping bug or an SDK convention change is a replay
-- away from being fixed instead of being data loss.
CREATE TABLE raw_batches (
    id               INTEGER PRIMARY KEY,
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    received_at      INTEGER NOT NULL,
    dialect          TEXT,
    content_encoding TEXT,
    body             BLOB NOT NULL
) STRICT;

CREATE INDEX idx_raw_batches_received ON raw_batches(project_id, received_at);
