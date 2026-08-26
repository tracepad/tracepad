-- Projects and API keys (spec 001). STRICT per spec 001 #7.

CREATE TABLE projects (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    retention_days INTEGER NOT NULL DEFAULT 30,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE TABLE api_keys (
    public_key  TEXT PRIMARY KEY,
    secret_hash BLOB NOT NULL UNIQUE,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX idx_api_keys_project ON api_keys(project_id);
