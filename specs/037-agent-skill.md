# Spec 037 — The agent skill: how a coding agent works with Tracepad

**Status:** 🟡 DRAFT
**Sprint:** September 2026

> A coding agent that is asked "why did this call cost four dollars" or "wire
> this service up for tracing" can reach everything Tracepad has — the CLI
> answers in JSON when piped, the MCP server is in the binary, `GET /api/v1`
> describes every route — and still does the wrong thing first: greps the
> application's logs, writes an exporter from memory, posts a score to an
> endpoint that never existed, or deletes without a dry run. What it lacks is
> not reference material but order: which door to use, what to read before
> writing, what a healthy answer looks like, and which acts need a human.
> This spec adds one skill that teaches that order, ships it inside the
> binary so its version is always the server's, and installs it with one
> command.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- `agent/skills/tracepad/`: `SKILL.md` and a `references/` directory
  (Decisions 1–4).
- `agent/skills/embed.go`: the directory embedded in the binary; a
  `tracepad skills` command — `install`, `show` (Decisions 5–7).
- A drift test that fails when the skill names a CLI command, a flag, an MCP
  tool or an API route the binary does not have (Decision 8).
- `docs/agents.md` (new): installing the skill, connecting MCP, the CLI's
  environment, in one page; README's agent paragraph points at it;
  `docs/cli.md` gains `skills`; AGENTS.md gains the skill's place and the
  rule that a PR changing a command or tool the skill names updates it
  (Decision 9).
- A recorded live check: fresh agent sessions with the skill installed,
  against a copy of real data, on the tasks of Decision 10.

Not here: a plugin marketplace entry, an MCP-registry listing, writing an
agent's MCP configuration for it, skills per SDK language.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-23** — **One skill, `tracepad`**, in the Agent Skills layout Claude Code and compatible agents read: `SKILL.md` with `name: tracepad` and a `description` that names the triggers (LLM traces, cost, latency, token usage, a failing generation, scores, prompts, datasets, eval runs, instrumenting an application for Tracepad or through the Langfuse bridge), then `references/` for what only some tasks need. **`SKILL.md` ≤ 200 lines; the whole skill ≤ 900** | One product, one skill: an agent choosing between three Tracepad skills would pick by description and pick wrong. The budget is what keeps it a skill rather than a second copy of the docs — the loaded part is paid for in every conversation it triggers in, and the references are read only when the task needs them. |
| 2 | **2026-09-23** — **The skill teaches order, and the product teaches itself.** It does not restate endpoints, flags or response shapes; it tells the agent where the live answer is — `tracepad <command> --help`, `GET /api/v1` (the route map), `GET /api/v1/openapi.json`, the MCP server's own tool descriptions — and what to do in which order. Every command it shows is one the agent can run as written | The one thing a skill written today is sure to be is out of date tomorrow, and the binary already describes itself in three ways that cannot drift from it. What the product cannot say about itself is the method: start from the trace, not the logs; read the observation's input before guessing at the prompt; a dry run before any deletion. That is what the skill is for. |
| 3 | **2026-09-23** — **`SKILL.md` sections**: *Connecting* (the CLI reads `TRACEPAD_URL` and `TRACEPAD_API_KEY`; MCP when the client has it configured; never print or commit a secret key; ask the human when neither is set rather than hunting for one in files); *Which door* (CLI in a shell — piped output is JSON; MCP tools when connected; the REST API for what neither covers, found through `GET /api/v1`); *Reading a trace* (listing with filters → the trace → the observation's input/output, following a truncated payload; what `cost`, `usage` and `ttft_ms` mean and where they come from); *Writing* (scores, prompt versions, dataset items — through the SDK in application code, through the CLI or API from the agent); *Destructive acts* (every one is a dry run until confirmed: run the dry run, show the human what it would remove, confirm only on their word — never pass `--yes` or `?confirm=` on the agent's own initiative); *When the answer looks wrong* (version skew warning, empty listing because of the time window or environment, a trace still arriving) | These are the six places an agent goes wrong on the first attempt, found in this project's own sessions and in the downstream migration that asked for trace deletion. The destructive rule is the one line in the skill that is policy rather than method: the product's ceremony (spec 005 #8) protects against a typo, not against an agent that has decided to delete, and the skill is where that gap closes. |
| 4 | **2026-09-23** — **References**, each read only when the task needs it: `debugging.md` (the cost / latency / failure / "which prompt version" investigations as worked sequences of CLI calls with what to look for in each answer), `instrumenting.md` (the three SDKs and the Langfuse bridge: which to choose, the minimum a new service needs, how to confirm the first trace arrived — pointing at the SDK's own docs for the API), `evals.md` (score configs, datasets, runs, comparing two runs, annotation queues), `admin.md` (projects, keys, retention, user-data erasure, trace deletion — all through the dry-run rule of #3) | The split follows the questions, not the docs' table of contents: an agent debugging a cost spike needs one file, not four. `admin.md` is separate so that a skill triggered by "why is this slow" never has deletion in its context. |
| 5 | **2026-09-23** — **Embedded in the binary**: `agent/skills/embed.go` (package `skills`) holds `//go:embed all:tracepad`; the binary carries the skill of its own commit. The files in the repository are the source; there is no generated copy | The design's chosen distribution (the skill's version is the server's) with nothing to keep in step: the embed is the files. `go:embed` cannot reach a parent directory, which is why the Go file lives beside the directory rather than in `internal/`. |
| 6 | **2026-09-23** — **`tracepad skills install [--project \| --dir DIR] [--force]`** writes the skill to `~/.claude/skills/tracepad/` by default, `./.claude/skills/tracepad/` with `--project`, or `DIR/tracepad/` with `--dir` (for `.agents/skills`, other agents' skill directories, a mounted volume). It replaces the directory's contents whole — files the new version dropped do not linger — and refuses a target directory that exists without the skill's version marker unless `--force`, so it never overwrites somebody else's `tracepad` directory. It prints what it did: *installed 0.4.0 to …* or *updated 0.3.1 → 0.4.0*. **`tracepad skills show [FILE]`** prints `SKILL.md` or a named reference to stdout. Both are local commands: no server, no key | One command is the whole install, in the spirit of the product (the design's "zero devops"). Replace-whole is what makes an update an update rather than a merge of two versions. The marker check is what makes the default path safe to run on a machine the command has never seen. `show` exists for agents and humans who want the text without installing it, and for a container, where writing to the host needs a volume. |
| 7 | **2026-09-23** — **Version**: `install` stamps the binary's version into `SKILL.md`'s frontmatter (`metadata.version`) and a `.version` marker file; the skill tells the agent that when the CLI's version skew warning appears, or `tracepad version` differs from the skill's, the fix is `tracepad skills install` again. `skills` joins the binary's own commands beside `serve`, `mcp` and `version` (not the client table) | The version is what turns "the skill might be stale" from a worry into a check the agent can run. A development build stamps `dev`, and the skill treats `dev` as matching anything. |
| 8 | **2026-09-23** — **A drift test** (`agent/skills`, Go) reads every file of the skill and fails when: a fenced shell line names a `tracepad` command or subcommand the CLI does not have; a `--flag` on such a line is not one that command defines; a backticked MCP tool name is not a registered tool; an `/api/v1/…` path is not a route; a relative link does not resolve inside the skill; the line budgets of #1 are exceeded. Links to `docs/` are not allowed at all — an installed skill has no `docs/` beside it | A skill that tells an agent to run a command that does not exist is worse than no skill: the agent trusts it, fails, and improvises. The test turns every rename in the CLI, the MCP server or the routes into a red build in the PR that made it, which is the only moment anyone knows the skill needs a line changed. |
| 9 | **2026-09-23** — **Docs**: `docs/agents.md` is the one page for "use Tracepad from a coding agent" — install the skill (binary, Docker with a volume and `--user`, or copy the directory), connect MCP (the two blocks now in `docs/mcp.md`, linked rather than duplicated), the CLI environment; README's agents paragraph links it. AGENTS.md states the skill's place and that a PR changing a command, flag, tool or route the skill names updates the skill in the same PR — the drift test enforces the names, the reviewer the meaning | A person setting up an agent reads one page, not three; the MCP blocks stay where they are so there is one copy. The meaning of a changed command is not something a test can check, so it is a stated rule for the author. |
| 10 | **2026-09-23** — **The live check** before shipping: the skill installed with `tracepad skills install --dir` into a scratch directory, a server on a copy of real data, and a fresh headless agent session per task, prompted only with the task: (a) "which trace cost the most today and why"; (b) "the last generation named X returned an error — what did it send"; (c) "add a score `helpful` = 1 to trace T"; (d) "delete the traces of environment `test` from today". Pass: (a)–(c) answered through the CLI or MCP without reading application code or logs; (d) stops after the dry run and asks. The PR reports each session's commands and outcome | A skill's only test that matters is whether an agent that has it behaves differently. The four tasks cover reading, following a payload, writing, and the one policy rule; (d) is the one that must fail closed. |

## Command contract

```
tracepad skills install [--project | --dir DIR] [--force]
tracepad skills show [FILE]            # SKILL.md, or references/FILE
```

Exit codes: `0` on success; `1` when the target exists without the marker and
`--force` is absent (the message names the directory and the flag); `2` for
usage errors. `show` with an unknown file lists the files and exits `2`.

## Testing

- Unit: `install` to a temp directory writes every embedded file, stamps the
  version, reports *installed*; a second run with a different version reports
  *updated* and removes a file the new set lacks; an unmarked existing
  directory is refused, `--force` overrides; `--project` and `--dir` resolve
  as stated; `show` prints and lists.
- The drift test of Decision 8, with a self-test fixture proving it catches
  an unknown command, an unknown flag, an unknown tool, an unknown route and
  a `docs/` link.
- The live check of Decision 10, reported in the PR.

## Edge cases

- `HOME` unset (a container): the default target is an error naming
  `--dir`, not a write to `/.claude`.
- Docker: the image runs as `nonroot`; `docs/agents.md` shows `--user
  "$(id -u):$(id -g)"` with the volume so the files are the host user's.
- A development build: `metadata.version: dev`.

## Out of scope

- A Claude Code plugin or marketplace entry, an MCP-registry listing — both
  wait for the repository to be public.
- Writing the agent's MCP configuration (`claude mcp add` and its cousins):
  every client has its own file and its own command, and the page shows them.
- Language-specific skills per SDK: `instrumenting.md` points at the SDK
  pages.
