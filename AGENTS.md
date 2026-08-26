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
- Next: spec 003 — scores and prompts.

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
