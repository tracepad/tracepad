# Spec 037 — The agent skill: how a coding agent works with Tracepad

**Status:** ✅ SHIPPED
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
| 11 | **2026-09-23** — **What the drift test reads, and how** (refines #8). Every name is asked of the binary: a subcommand and a flag of `cli.Run` itself, with the probe `TestUsageAndFlagsAgree` already uses (an already-cancelled context, `--flag=1`, "not defined" is the only refusal that counts); `skills` of its own flag sets; the routes from `GET /api/v1` of a real server; the tools from the MCP registry's `tools/list`. A `tracepad …` command in an inline code span is checked like a fenced line, since #2 promises every command shown is runnable. A backticked word is a tool name when it starts with one of the registry's verbs (`get`, `list`, `search`, `compare`), so `ttft_ms` is not one. `serve` and `mcp` are reported as uncheckable rather than passed: their flags are parsed in package `main`. The ban on `docs/` is on relative links; an absolute `https://github.com/tracepad/tracepad/blob/main/…` link — the form the SDK READMEs already use — is allowed and checked against the file and the heading it names | A relative `docs/` link breaks the moment the skill is installed; an absolute one resolves wherever the skill is read, and the SDK pages are where `instrumenting.md` has to send the agent (#4). Checking the link against the repository keeps it from rotting the way a rename would. Asking `cli.Run` rather than parsing the usage text means the test cannot disagree with the command about what the command takes. |
| 12 | **2026-09-23** — **`keys rm` is not a dry run** except for a project's last key, which the live walk found by revoking one (on a copy). #3 listed "a key" among the acts that preview; the skill instead names the removals that act at once — revoking a key, `runs rm`, `score-configs rm`, `scores rm`, `prompts label --rm` — and tells the agent to ask before running them at all | The rule in #3 is "the agent never confirms on its own initiative"; for a command with no preview, running it *is* confirming, so the ask moves before the command. The command's own behaviour is spec 005's and unchanged here. |
| 13 | **2026-09-23** — **The command's small print.** The version is stamped quoted (`version: "0.4.0"`) so YAML reads `1.10` as a string; `show` prints `SKILL.md` stamped the way `install` would write it and takes a reference as `debugging.md`, `debugging` or `references/debugging.md`; a second install of the same version says *reinstalled*; `--force` over a directory without the marker says it replaced one; `--project` with `--dir`, and `--dir ""`, are usage errors (`2`); an unset `HOME` without `--dir` is `1`, naming `--dir`. The new copy is written beside the target and swapped in by rename, so a failed install leaves the old skill whole | What #6 and #7 leave open, settled the way the rest of the CLI settles it: an empty flag value is a usage error rather than a default (spec 003 #23), and an update is never half of two versions. |
| 14 | **2026-09-23** — **From review of PR #76.** The embed is `//go:embed tracepad`, not `all:tracepad` as #5 wrote: `all:` would compile a `.DS_Store` or an editor's dotfile into the binary, install it, list it in `show` and count it against the budget, and the skill has no dotfile of its own. A target that is a symlink (a copy kept in a dotfiles repository) is followed, so that copy is updated rather than replaced by a directory. On Windows the home directory is `USERPROFILE` when `HOME` is unset — `os.UserHomeDir`'s rule — since the release ships a Windows build. An empty marker is still a marker (*updated an unversioned install*), and one that cannot be read is an error saying so. Staging directories are named `.tracepad-install-*`; one left by a killed install is removed by the next, a failed rollback says where the previous skill is, and a previous copy that cannot be removed after the swap is a warning naming it, not a failed install. The drift test reads prose a paragraph at a time, so a code span or a link that wraps is checked whole; checks `skills show FILE` against the files the binary carries; and slugs headings by the rule `scripts/doc-anchors.sh` states, numbered repeats included | Each is the behaviour #6–#8 meant, made true on a path the first version got wrong: a dotfile shipped by accident, a symlink silently cut, a Windows user told `HOME is not set`, an install that succeeded reported as failed, a wrapped command never checked. |
| 15 | **2026-09-23** — **From the second review of PR #76; supersedes #14 on staging and on Windows.** An install stages in a holding directory one level down, `.tracepad-install/` beside the skill, which it removes when it finishes: a skills directory is read one level deep, so a copy an interrupted install leaves is never taken for a second `tracepad` skill. A staging copy older than an hour is removed by the next install; a previous copy set aside (`*.old`) is **never** deleted — it may be the only copy of a directory `--force` took over — and every install names it on stderr until someone does. The directory is made with `os.Mkdir` under the umask like the files in it, not widened by a chmod. On Windows the home is `USERPROFILE` alone, `os.UserHomeDir`'s rule, because a `HOME` set by Git Bash is not where Claude Code looks. A regular file in the target's place is unmarked, so `--force` replaces it; a relative `--dir` resolves against the same working directory as `--project`; a blank line or a comment inside `metadata:` does not end it. The drift test reads a command behind `$`, `sudo`, a path to the binary or `docker run … tracepad/tracepad`, and numbers repeated heading slugs the way github-slugger does (`a`, `a`, `a-1` → `a`, `a-1`, `a-1-1`) | #14's sweep matched the set-aside copy by its prefix, so the one path meant to rescue a user's files after a failed rollback could delete them; the holding directory separates the two by name and puts both out of an agent's sight. |
| 16 | **2026-09-26** — **Refines #14 and #15: a link is followed only into this command's own skill, and never through the holding directory.** A symlink at the target is followed when the directory it resolves to carries the `.version` marker — the dotfiles copy #14 is about, updated in place. A link that resolves to a directory without the marker (or to a file) is refused **with or without `--force`**, naming the link and where it points and telling the user to remove the link and install again; the refusal does not suggest `--force`. `--force` still replaces an unmarked directory or file that is really at the target (#6, #15). The holding directory `.tracepad-install/` must be a real directory: if the name is taken by a symlink or a file, the install refuses before writing or sweeping anything | `--force` through a link replaced whatever the link named: a link into another directory — planted, or committed to a cloned repository's `.claude/skills/` and reached with `--project --force` — had that directory's contents set aside and deleted. The sweep of the holding directory deletes stale entries, so a link in its place pointed the same deletion somewhere else. Refusing rather than replacing the link itself keeps #14's promise that a dotfiles link is never silently cut. |

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
