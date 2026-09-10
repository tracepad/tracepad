-- Accounts: the people who sign in (spec 028, Decisions 1-5).
--
-- Until now a caller was a project key or the deployment's admin token, and
-- both are secrets a program holds. A person is neither: they have one
-- identity across projects, a role in each, and a session that a browser can
-- carry without a script being able to read it. That is the four tables here.
--
-- The word *user* is taken (spec 023: the traced application's end users), so
-- the person who signs in is an **account** — in the tables, the routes, the
-- CLI and the docs.

-- One person. The email is the sign-in name and nothing else: it is never
-- verified and never written to, so no `email_verified`, no `pending_email`.
--
-- `password_hash` is NULL from the moment an owner invites somebody until the
-- moment they accept the invitation (Decision 10); an account in that state
-- reads as `pending` and cannot sign in — the login answers the one 401 every
-- other failure answers.
--
-- `owner` is a flag rather than a membership row (Decision 2): an owner has
-- every project, so a row per project would be a fan-out that has to be kept
-- in step with the projects table for no gain.
CREATE TABLE accounts (
    id            TEXT PRIMARY KEY,                 -- 16 random bytes, hex
    email         TEXT NOT NULL COLLATE NOCASE UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    password_hash BLOB,                             -- NULL until an invite is accepted
    owner         INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,                 -- unix nanoseconds, like every timestamp here
    last_login_at INTEGER
) STRICT;

-- A role in one project. Two roles and no third (Decision 2): `viewer` looks
-- and annotates, `editor` changes what the project is made of.
--
-- The cascade on `projects` is what makes a project's purge take its
-- memberships with it (spec 005 #9). A *soft*-deleted project keeps them,
-- because its row is still there — so a restore brings the members back.
CREATE TABLE memberships (
    account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'editor')),
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (account_id, project_id)
) STRICT;

-- The project side of the same question: "who can see this project" is the
-- Settings screen's read, and without this index it is a scan of every
-- membership there is.
CREATE INDEX idx_memberships_project ON memberships(project_id);

-- A browser session (Decision 4). The row id is `sha256(cookie value)`, for
-- the reason a key is stored as a hash (spec 001 #8): a read of the database
-- must not hand out live sessions.
--
-- `user_agent` and `ip` are here so that "sign out everywhere" can be a
-- decision rather than a leap of faith — a person looking at the list needs
-- to recognise which row is the laptop they still have.
CREATE TABLE account_sessions (
    id           TEXT PRIMARY KEY,                  -- sha256(cookie value), hex
    account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT ''
) STRICT;

-- Listing one account's sessions, and ending them all when a password changes
-- or the account is disabled.
CREATE INDEX idx_account_sessions_account ON account_sessions(account_id);

-- The sweeper's seek (spec 005): expired rows go on the usual pass, and a
-- lookup ignores an expired row before the sweeper reaches it.
CREATE INDEX idx_account_sessions_expires ON account_sessions(expires_at);

-- Invitations and password resets are one table because they are one thing
-- (Decision 10): a single-use token that sets a password. The id is
-- `sha256(token)` for the same reason the session's is.
CREATE TABLE account_tokens (
    id          TEXT PRIMARY KEY,                   -- sha256(token), hex
    account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_account_tokens_account ON account_tokens(account_id);
CREATE INDEX idx_account_tokens_expires ON account_tokens(expires_at);
