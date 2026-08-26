# Spec 001 — Server Skeleton

**Status:** ✅ SHIPPED
**Sprint:** August 2026

> The minimal runnable Tracepad: a single binary that starts an HTTP server,
> owns a self-migrating SQLite store, bootstraps projects with API keys, and
> answers `/health`. Everything later (ingest, read API, UI) builds on this
> foundation, so this spec fixes the binary layout, configuration surface,
> storage bootstrap, and release plumbing.

---

## Overview

Deliverables:

- `tracepad` binary: `tracepad` / `tracepad serve` runs the server,
  `tracepad version` prints the version.
- Configuration via `TRACEPAD_*` environment variables with flag mirrors.
- SQLite store (pure-Go driver) with embedded forward-only migrations that
  apply automatically on start, taking a file backup first.
- Project + API key model with first-run bootstrap and declarative
  `TRACEPAD_PROJECTS` provisioning.
- `GET /health` returning `{"status":"ok","version":...}`.
- Make targets (`build`, `test`, `precommit`, …), GitHub Actions CI,
  GoReleaser config for the six-platform release matrix.

## Decisions log

| # | Decision | Why |
|---|---|---|
| 1 | Bare `tracepad` (no args) runs the server | The target experience is "download, run, works". Requiring `serve` is a paper cut for the primary flow; subcommands still exist for everything else. |
| 2 | Default listen address is `:4318` | 4318 is the OTLP/HTTP convention port, and OTel SDK exporters default to `http://localhost:4318`. A locally running Tracepad therefore receives traces from an unconfigured OTel app with zero endpoint setup. UI and API share the same port (single-binary, single-port). |
| 3 | CLI is stdlib `flag` + manual subcommand dispatch, no framework | Stage 0 has two subcommands; a CLI framework is not paying for itself yet. Revisit in the stage-2 spec (read-side CLI with many subcommands and global `--json`) — this decision is expected to be renegotiated there, and the dispatch code is small enough to throw away. |
| 4 | API secrets are hashed with SHA-256, not bcrypt | Keys are generated 256-bit random strings, so offline brute force is infeasible and slow hashing adds nothing. bcrypt would cost ~100ms *per request* on the hot auth path and pull in `x/crypto`. bcrypt/argon2 are for low-entropy human passwords, which we do not have (no user accounts by design). |
| 5 | SQLite driver is `modernc.org/sqlite` (pure Go) | CGO-free static cross-compilation for the whole release matrix without C toolchains. ~2× slower writes than the CGO driver is irrelevant at our load profile (design gives ~100× headroom). The store interface stays narrow so the driver can be swapped locally if the CI benchmark gate (later stage) ever trips. |
| 6 | Migrations: embedded files `NNNN_name.sql`, forward-only, tracked by filename in `schema_migrations`, lexicographic order, file backup before applying | Zero-devops constraint: the binary must self-heal its schema on start with no operator action. Forward-only avoids down-migration testing surface. A copy of the DB file (`<db>.pre-<first-pending>.bak`) before applying pending migrations makes every upgrade trivially reversible by file swap — appropriate for an embedded single-file DB. |
| 7 | All tables are SQLite `STRICT` | Catches type mistakes at insert time instead of storing garbage; costs nothing. New tables must follow suit. |
| 8 | One key pair (`tp-pk-…`, `tp-sk-…`) per project serves both native and compat auth | Native auth is `Bearer <tp-sk>`; the future Langfuse-SDK path uses `Basic base64(pk:sk)` with the same pair. Langfuse SDKs do not validate key prefixes, so one pair covers both wire formats. Secret lookup is by `sha256(secret)` (unique index) so Bearer auth needs no key id in the token. |
| 9 | `TRACEPAD_PROJECTS="name:pk:sk,…"` declarative bootstrap, idempotent | docker-compose reproducibility: a restarted container must not mint new keys. Existing project names are left untouched (keys are not rotated by env), missing ones are created with the given pair. First run without the variable creates project `default` with generated keys and prints ready-to-paste OTel and Langfuse env lines. |
| 10 | Logging is `log/slog`, text handler to stderr | stdlib, structured, zero deps. File logging with rotation is a later concern (self-diagnosis stage); stderr is correct for both `docker logs` and dev. |
| 11 | (2026-08-26, from PR #1 review) Pre-migration backups use `VACUUM INTO`, not a file copy | A plain copy of the main DB file silently loses committed rows still sitting in the WAL (reproduced: after an unclean shutdown the copied file restored an empty database), betraying exactly the scenario backups exist for. `VACUUM INTO` produces a complete checkpointed snapshot and streams it without loading the DB into memory. The snapshot is defragmented, not byte-identical — irrelevant for restore-by-file-swap. |

## Configuration

| Env | Flag | Default | Meaning |
|---|---|---|---|
| `TRACEPAD_LISTEN` | `--listen` | `:4318` | HTTP listen address |
| `TRACEPAD_DATA_DIR` | `--data-dir` | `$XDG_DATA_HOME/tracepad` or `~/.local/share/tracepad` | DB + future payload storage |
| `TRACEPAD_PROJECTS` | — | empty | Declarative project bootstrap (Decision 9) |

Flags override env. Unknown `TRACEPAD_*` variables are a startup warning (typo
detector), not an error.

## Data contract (schema 0001)

```sql
projects(id TEXT PK, name TEXT UNIQUE, retention_days INT DEFAULT 30, created_at)
api_keys(public_key TEXT PK, secret_hash BLOB UNIQUE, project_id → projects, created_at)
```

`schema_migrations(filename TEXT PK, applied_at)` is created by the migration
runner itself, outside numbered migrations.

## Edge cases

- Data dir missing → created with 0700.
- DB from a newer binary (unknown migration recorded) → refuse to start with a
  clear error naming both versions; never run "unknown future" schemas.
- Corrupt WAL → SQLite recovers on open; if open fails, the error names the DB
  path and the latest `.bak` file.
- Duplicate names in `TRACEPAD_PROJECTS` → startup error (ambiguous intent).
- Backup file already exists (re-run after crashed migration) → overwrite; the
  pre-migration state of *this* run is what matters.

## Out of scope (later specs)

Trace/observation schema and OTLP ingest (002), scores/prompts, read API,
retention sweeper, UI, admin token auth, Docker/brew packaging polish.
