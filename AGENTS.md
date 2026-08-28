# Tracepad — agent guide

Lightweight, self-hosted, OTLP-native store and viewer for LLM/agent traces.
Single Go binary, embedded SQLite, UI/CLI/MCP as thin clients over one read
API. This file routes; it does not duplicate what specs and docs say.

## Current status

- ✅ Spec 001 (server skeleton) shipped: config, self-migrating SQLite store,
  project/key bootstrap, `/health`, CI gate, GoReleaser config. Go 1.27,
  `modernc.org/sqlite`.
- ✅ Spec 002 (trace model + OTLP ingest) shipped: `POST /v1/traces` and the
  Langfuse alias, schema 0002, attribute mapping with two dialects,
  group-commit writer, raw body storage.
- ✅ Spec 003 (scores & prompts) shipped: `/api/v1/scores` and
  `/api/v1/prompts`, schema 0003, versioned prompts with movable labels, the
  group-commit writer generalized to carry every durable write.
- ✅ Spec 004 (read API, CLI, MCP) shipped: the read API with response
  budgets and truncation markers, self-description and OpenAPI, schema 0004,
  a CLI in the same binary, and an MCP server on protocol 2026-07-28. Both
  clients are HTTP clients of the read API and contain no logic of their own.
- ✅ Spec 005 (retention & admin) shipped: schema 0005, the hourly sweeper
  writing every chunk through the group-commit writer, the admin API under
  `/api/v1/projects` with a dry-run/confirm contract on every destructive
  endpoint, and the `projects`/`keys`/`retention`/`users` CLI. MCP unchanged,
  by design.

## Where things are

| Working on | Read first |
|---|---|
| Any feature | Its spec in `specs/` (spec-first — see Process below) |
| Storage, schema, migrations | `internal/store/`, spec 001 |
| Write pipeline (group commit) | `internal/store/writer.go`, spec 002 #15, spec 003 #9 — every durable write is a `WriteJob` |
| HTTP surface | `internal/server/` |
| OTLP ingest | `internal/server/otlp.go`, `docs/ingest.md`, spec 002 |
| Scores & prompts | `internal/server/scores.go`, `prompts.go`, `docs/scores.md`, `docs/prompts.md`, spec 003 |
| JSON API plumbing (auth, strict decode, pagination) | `internal/server/api.go`, spec 003 |
| Read API (traces, sessions, stats, system) | `internal/server/traces.go` and neighbours, `docs/api.md`, spec 004 — the route table in `routes.go` is the surface, and `openapi.json` must agree with it |
| Response budgets and truncation | `internal/server/budget.go`, spec 004 #2 |
| CLI | `internal/cli/`, `internal/client/`, `docs/cli.md`, spec 004 |
| MCP | `internal/mcpserver/`, `docs/mcp.md`, spec 004 — tools call the read API over HTTP, never the store, and only ever with a GET (spec 005 #13) |
| Retention and the sweeper | `internal/store/sweep.go`, `docs/retention.md`, spec 005 — every chunk is a `WriteJob`, never a second write connection |
| Admin API (projects, keys, retention, erasure) | `internal/server/admin.go`, `internal/store/admin.go`, `docs/admin.md`, spec 005 — destructive endpoints are a dry run until `?confirm=` echoes the name, checked inside the write transaction |
| Attribute mapping | `internal/mapping/rules.go` is the table; `mapping.go` applies it |
| Configuration | `internal/config/`, spec 001 + spec 002 Configuration tables |

Attribute semantics for the `langfuse.*` dialect are derived from Langfuse
(MIT) — see `NOTICE`. Keep new rules in the table in `rules.go`, with the
reason in a comment; adding a dialect should be a table edit.

## Commands

- `make precommit` — full gate (format-check + vet + tests). Budget: under 30
  seconds. The gate self-installs as the git pre-commit hook on first run (and
  on Claude Code session start); `make install-hooks` force-reinstalls it.
- `make dev` — run the server, output mirrored to `.dev.log` (read that file
  first when debugging a running server).
- `make build` — binary into `./bin`.
- `make smoke` — export from pinned real SDKs into a real binary and assert
  the rows. Needs network on first run (installs the SDKs).
- `make fixtures` — regenerate `testdata/otlp/*.pb` and their goldens after a
  deliberate mapping change. Review the golden diff; it *is* the change.

## Process

- **Spec-first.** A feature is implemented against a spec in `specs/`
  (`NNN-name.md`). Deviating from the spec during implementation means adding
  a dated entry to that spec's Decisions log — not silently diverging, and
  not rewriting history.
- **The spec outranks code and tests.** A red test is a question, not a
  command: do not "green" code against a stale test. Intentional behavior
  changes update the test *and* the Decisions log.
- **Docs ship in the same PR** as the behavior they describe.
- **Trunk-based PR flow**: short-lived branch → PR → squash-merge. PR titles
  follow Conventional Commits (they become the commit history).
- Stage git changes with explicit paths (never `git add -A`); review
  `git diff --cached --name-only` before committing.
