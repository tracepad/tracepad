-- Datasets, runs and score configs (spec 014). STRICT per spec 001 #7.
--
-- Three nouns an eval needs and the trace store did not have: a dataset of
-- versioned test cases, a run that groups the traces one pass over it
-- produced, and a config that pins what a score's name means. The store never
-- executes anything (spec 014 #1); the harness stays the client's, and these
-- tables are where its results finally meet the traces they came from.

-- A dataset is addressed by name, unique per project, with the prompt-name
-- grammar (spec 014 #9). `version` is one clock for the whole item set: it
-- advances by exactly one on every write that changes the set, and a batch is
-- one tick (spec 014 #5). `next_seq` numbers items in order of first
-- appearance, which is the order the listing keeps (spec 014 #21).
CREATE TABLE datasets (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT,
    metadata    TEXT,
    version     INTEGER NOT NULL DEFAULT 0,
    next_seq    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (project_id, name)
) STRICT;

-- Items are append-only: an edit is a new row at a new dataset version, a
-- delete is a row with `archived = 1`, and "the dataset at version V" is, per
-- item, the row with the greatest `dataset_version <= V` minus the archived
-- ones (spec 014 #5). The bodies are opaque JSON the store never reads inside
-- (spec 014 #4), compacted before they are compared and written (spec 014 #6).
--
-- `source_trace_id` and `source_observation_id` are strings, not foreign keys:
-- the trace a case was cut from lives under retention and may be gone, and a
-- case does not stop being a case when its origin does (spec 014 #4).
CREATE TABLE dataset_items (
    project_id            TEXT NOT NULL,
    dataset               TEXT NOT NULL,
    item_id               TEXT NOT NULL,
    dataset_version       INTEGER NOT NULL,
    seq                   INTEGER NOT NULL,
    archived              INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1)),
    input                 TEXT,
    expected_output       TEXT,
    metadata              TEXT,
    source_trace_id       TEXT,
    source_observation_id TEXT,
    created_at            INTEGER NOT NULL,
    PRIMARY KEY (project_id, dataset, item_id, dataset_version),
    FOREIGN KEY (project_id, dataset) REFERENCES datasets(project_id, name) ON DELETE CASCADE
) STRICT;

-- The listing walks `seq`, and the per-item "greatest version at or below V"
-- resolves through the primary key.
CREATE INDEX idx_dataset_items_seq ON dataset_items(project_id, dataset, seq, dataset_version);

-- A run is a container the harness opens and closes (spec 014 #1, #8). It
-- pins the dataset version it ran (spec 014 #7); its status is what the
-- harness said and never inferred, so a run left `running` is reported as
-- such with its age. `metadata` is the harness's free-form description of
-- what was tried (spec 014 #12).
CREATE TABLE dataset_runs (
    project_id      TEXT NOT NULL,
    id              TEXT NOT NULL,
    dataset         TEXT NOT NULL,
    dataset_version INTEGER NOT NULL,
    name            TEXT,
    metadata        TEXT,
    status          TEXT NOT NULL CHECK (status IN ('running', 'finished', 'failed')),
    error           TEXT,
    created_at      INTEGER NOT NULL,
    finished_at     INTEGER,
    PRIMARY KEY (project_id, id),
    FOREIGN KEY (project_id, dataset) REFERENCES datasets(project_id, name) ON DELETE CASCADE
) STRICT;

-- A dataset's runs, newest first, which is the listing's order.
CREATE INDEX idx_dataset_runs_dataset ON dataset_runs(project_id, dataset, created_at DESC, id DESC);

-- A score config binds a name to a type, a direction and a range, and a
-- score whose name has one must satisfy it (spec 014 #15). Declarative: a PUT
-- replaces the whole row, there are no versions (spec 014 #17). `direction`
-- is required for numeric and boolean names and forbidden for the rest
-- (spec 014 #16); that rule lives in the handler, since a CHECK cannot see
-- which data type a direction is being attached to without repeating it.
CREATE TABLE score_configs (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    data_type   TEXT NOT NULL CHECK (data_type IN ('numeric', 'boolean', 'categorical', 'text')),
    direction   TEXT CHECK (direction IN ('higher', 'lower', 'none')),
    min_value   REAL,
    max_value   REAL,
    categories  TEXT,
    description TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (project_id, name)
) STRICT;

-- The link between a trace and its run is two columns on the trace, claimed by
-- the mapper from `tracepad.run_id` / `tracepad.item_id` and upserted per field
-- like `session_id` (spec 014 #2). No foreign key: the columns record what the
-- client said, and a trace naming a run that does not exist is stored, counted
-- and reported rather than refused (spec 014 #3).
ALTER TABLE traces ADD COLUMN run_id  TEXT;
ALTER TABLE traces ADD COLUMN item_id TEXT;

-- Every read of a run is a walk over its traces by item; the sweep's pin check
-- and the pinned count are seeks on `run_id` (spec 014 #13).
CREATE INDEX idx_traces_run ON traces(project_id, run_id, item_id);
