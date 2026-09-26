-- Key provenance (spec 045, Data contract). `api_keys` is rebuilt rather than
-- altered so that `scopes` and `created_via` have no default: an insert that
-- does not say what a key may do, or who made it, fails loudly instead of
-- inheriting whatever this backfill needed. The table is a handful of rows,
-- and the rebuild runs with foreign keys disabled (migrate.go), as 0005's did.

CREATE TABLE api_keys_new (
    public_key       TEXT PRIMARY KEY,
    secret_hash      BLOB NOT NULL UNIQUE,
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    name             TEXT NOT NULL DEFAULT '',
    -- The canonical spelling of a non-empty subset, in this order (#1);
    -- until a mint can choose (#6), every key is 'ingest read write'.
    scopes           TEXT NOT NULL CHECK (scopes IN (
                         'ingest', 'read', 'write', 'ingest read', 'ingest write',
                         'read write', 'ingest read write')),
    created_via      TEXT NOT NULL CHECK (created_via IN (
                         'account', 'admin_token', 'startup', 'unknown')),
    created_by       TEXT REFERENCES accounts(id) ON DELETE SET NULL,
    created_by_email TEXT NOT NULL DEFAULT '',   -- as it was at minting (#8)
    last_used_at     INTEGER                     -- Unix ns; NULL = not since 0023 (#9)
) STRICT;

-- Every key that exists now can do what every key could: all three scopes
-- (#5). Who minted it was never recorded, and `unknown` says so rather than
-- guessing (#8). The hashes are copied, so every secret that authenticated
-- before this migration authenticates after it.
INSERT INTO api_keys_new (public_key, secret_hash, project_id, created_at, scopes, created_via)
SELECT public_key, secret_hash, project_id, created_at, 'ingest read write', 'unknown'
  FROM api_keys;

DROP TABLE api_keys;
ALTER TABLE api_keys_new RENAME TO api_keys;
CREATE INDEX idx_api_keys_project ON api_keys(project_id);
