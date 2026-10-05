# Spec 053 — Agent-first onboarding: one line for the terminal, one line for the agent

**Status:** ✅ SHIPPED
**Sprint:** October 2026

> The quickstart starts with steps a person does by hand: download an archive,
> check it, put it on the path, start the server, copy a key out of a log,
> configure an application, open a browser. A person who already works with a
> coding agent will paste the page into the agent and ask it to do the steps,
> and the agent will then improvise around each one. This spec turns
> onboarding into **two lines**. The first goes into a terminal: it installs
> the binary and the agent skill, verified, with no sudo. The second goes into
> the agent: it points at one page written *for the agent*, which takes the
> project from nothing to a trace the agent has read back itself. The agent
> then tells the person what it did, where the key is, and what is left that
> only a person can do. The documentation is published for agents as well as
> for people: `llms.txt`, and the raw Markdown of every page at a stable
> address.

---

## Overview

Deliverables, one PR (the spec is its first commit; the last commit flips the
status):

- **The install script** (Decisions 2–10): `scripts/install.sh` (POSIX `sh`),
  its offline test `scripts/install-test.sh` against a release built in a
  temporary directory, and `make install-script-test` in the gate, with
  `shellcheck` over both scripts.
- **Where it is served** (Decisions 3, 14): the docs build copies the script,
  `llms.txt`, `llms-full.txt` and every page's Markdown into each version of
  the site. `deploy.sh` mirrors them at the site's root from the version the
  root redirects to.
- **The agent's page** (Decisions 11, 12): `docs/agent-setup.md`, an
  instruction to a coding agent, in the navigation under *Get started*.
- **The skill** (Decision 13): `references/setup.md`, a routing line in
  `SKILL.md` and a setup trigger in its description.
- **The front door** (Decision 15): "With your coding agent" is the first
  block of `docs/quickstart.md` and of the README's *Getting it*, and the
  install script is a section of `docs/install.md`. The manual path stays as
  it is.
- **The audit** (Decision 16): every page of `docs/` read against the
  checklist for agents. Small fixes are made here, and larger findings are
  listed in the PR.

Builds on spec 037 (the skill, `tracepad skills install`), spec 050 (the docs
site, `deploy.sh`, the hook), spec 045 #28 (the first-start key is
`ingest`-only, and declared keys keep all three scopes) and spec 001 #24 (every
`TRACEPAD_*` variable is in `config.Env`).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-10-05** — **Two lines, always in this order.** The terminal line is `curl -fsSL https://tracepad.github.io/tracepad/install.sh \| sh`. The agent line is `Set up Tracepad for this project: follow https://tracepad.github.io/tracepad/agent-setup.md`. Each works alone: the script ends by printing the agent line, and the agent page starts with "if `tracepad version` fails, run the script". | The person may be in a terminal, in an agent, or in both. Two lines that each lead to the other are one path with two entrances, and neither entrance asks the person to read further. |
| 2 | **2026-10-05** — **A script, not a package manager, is the first line.** Weighed: **Homebrew** (`tracepad/tap`) exists, but only for stable releases (spec 020 #27), is only verified on macOS, and is not on most Linux machines or in containers. After 0.1.0 it stays the recommended path in `install.md` for people who use it. **Docker** needs Docker, and puts no CLI on the host for the agent to call. **`npx skills add` and other skill registries** install a skill but not the binary, and a skill that does not match the binary's version is the drift spec 037 #5 exists to prevent. **`go install`** builds without the interface (`install.md`). A POSIX script needs `curl`, `tar` and a SHA-256 tool, which are on every macOS and on any Linux where the person can paste a `curl` line. | The first line must work on a bare machine, for a person and for an agent alike. It installs the two things the agent needs, the binary and the skill matched to it, with the same verification `install.md` describes by hand. |
| 3 | **2026-10-05** — **The script lives at `scripts/install.sh`, and the site serves it.** The docs build copies it into each version (`<site>/<version>/install.sh`). `deploy.sh` copies it from the version the root redirects to (`latest`, or `dev` until the first stable release, spec 050 #6) to `<site>/install.sh`. The same rule mirrors `llms.txt`, `llms-full.txt` and the pages' Markdown at the root (#14). The file is also at `raw.githubusercontent.com/tracepad/tracepad/<ref>/scripts/install.sh` for any ref. | The site root is the one address that is ours and stable across releases, and the brief names it. Copying from the root's own version keeps one rule for what the root shows: once 0.1.0 exists, the script people run is the newest stable release's, not whatever `main` holds today. mike's redirect aliases (spec 050 #2) only cover HTML: under `latest/`, every file but a page is a 404 (`mike/commands.py`, `_add_redirect_to_commit`). So the mirror is written by `deploy.sh` and not left to mike. |
| 4 | **2026-10-05** — **What the script does, in order.** (1) Reads the OS and the architecture (`uname -s`, `uname -m`): `linux` or `darwin`, `amd64` or `arm64`. Anything else stops with exit `1` and points at `install.md`, which covers the Windows archives. (2) Resolves the version (#5). (3) Downloads the archive and `checksums.txt` into a temporary directory that is removed on exit. (4) Verifies the archive (#6). (5) Extracts `tracepad` and installs it (#7). (6) Installs the skill (#8). (7) Prints a summary: the version, where the binary is, whether the archive was verified by checksum only or by attestation as well, and where the skill went. Last comes one line, the agent line of #1. Exit `0` only when every step that ran succeeded. | Every step either finishes or says why it stopped. Because the script ends by naming the next action, the person never has to find the next page themselves. |
| 5 | **2026-10-05** — **Version: the newest stable release by default; a candidate only when it is pinned.** By default the script reads `<releases>/latest/download/checksums.txt`. GitHub redirects that address to the newest non-pre-release release. The version is read from the archive names in that file, so the script needs no API call and has no rate limit. `TRACEPAD_VERSION` pins a version, `0.1.0-rc.1` or `v0.1.0-rc.1`, candidates included. A missing stable release is read from the HTTP status (`404`), not from curl's exit code: `curl -f` reports GitHub's 404 as `22` over HTTP/1.1 and as `56` over HTTP/2, which the first live run showed. When there is no stable release, the script does **not** fall back to a candidate. It stops with exit `1`, says no stable release exists yet, and prints the pinned line to run. When the default source is GitHub, it names the newest pre-release from `api.github.com`; otherwise it shows a placeholder. | A silent fall-back to a candidate would install software the person did not choose. Until 0.1.0 a stable release does not exist, and saying so with the exact command to run is honest and costs one more paste. The API is consulted only on that error path, for a better hint, and never decides what gets installed. |
| 6 | **2026-10-05** — **Verification: the checksum always, and the attestation whenever it can be checked.** The archive's line in `checksums.txt` must exist and match (`sha256sum`, or `shasum -a 256`). If neither tool exists, the script refuses to install. If `gh` is on the PATH and logged in, `gh attestation verify <archive> --repo tracepad/tracepad` runs as well, and a failure stops the install. If `gh` is absent or not logged in, the summary says the attestation was skipped and how to run it. | The checksum catches a damaged download and costs nothing (`install.md`, *Verify what you downloaded*). Provenance needs `gh` and a login, which a bare machine lacks, so requiring it would make the one line fail where it matters most. Running it when possible, and saying which checks were made, is the most honest the script can be. A failed check is never only a warning. |
| 7 | **2026-10-05** — **Install directory: `~/.local/bin`, never `sudo`.** `TRACEPAD_INSTALL_DIR` overrides it. The directory is created if missing. The binary is copied next to its destination and renamed over it, so a running server keeps its file and a failure leaves the old binary in place. When the directory is not on `PATH`, the summary says so and prints the line to add to the shell's profile. The script does not edit any profile. | A script piped from the network should not ask for root, and `~/.local/bin` is the XDG location most shells already put on `PATH`. Editing a shell profile is a change the person did not ask for. |
| 8 | **2026-10-05** — **The skill goes where the person's agents read skills.** The script runs `tracepad skills install` (Claude Code, `~/.claude/skills`) when `~/.claude` exists, and `tracepad skills install --dir ~/.agents/skills` when `~/.agents` or `~/.codex` exists (the user-level location Codex and the shared convention read). When none exists, it installs nothing and prints both commands. `TRACEPAD_NO_SKILL=1` skips this step. The installed skill is the binary's own (spec 037 #5), so a re-run after an upgrade updates both. | Creating `~/.claude` on a machine without Claude Code is clutter. Detecting the directories an agent already made is the narrowest rule that works for the agents people actually have. |
| 9 | **2026-10-05** — **Re-running the script is an upgrade, or nothing.** When the installed binary (`tracepad version` in the install directory) already reports the target version, the script downloads nothing, says `tracepad X is already installed`, and still installs the skill. Otherwise it says `installed X`, or `updated A → B`. After an upgrade, if `tracepad health` answers with another version, the summary says the running server keeps that version until it is restarted. | The person and the agent can run the line again without having to ask whether that is safe. A server still running the old binary is the one surprise an upgrade leaves behind, so the script names it. |
| 10 | **2026-10-05** — **The installer's variables are in `config.Env`, as their own kind, and the script is tested offline.** (a) `TRACEPAD_VERSION`, `TRACEPAD_INSTALL_DIR`, `TRACEPAD_NO_SKILL` and `TRACEPAD_DOWNLOAD_URL` are `EnvInstaller`. `TRACEPAD_DOWNLOAD_URL` defaults to `https://github.com/tracepad/tracepad/releases`, and points the script at a mirror that copies that layout. The server does not warn about these variables when one is left exported. `docs/configuration.md` gets a table for them, and a test holds `scripts/install.sh` to exactly that list. (b) `scripts/install-test.sh` builds releases in a temporary directory (a fake `tracepad` in real archives, with a real `checksums.txt`) and runs the script through `file://` addresses, with a fake `uname` and a fake `gh` on the `PATH`. The cases: latest stable, a pinned candidate, no stable release, a wrong checksum, an archive missing from the checksums, a failing attestation, a re-run, an upgrade, a directory not on the `PATH`, an unsupported system, and the skill detection. It runs no network and no server. (c) `make install-script-test` runs `shellcheck` (`shellcheck-py` 0.11.0.1 via `uvx`, which the gate already needs) and then the test, and is in the gate. | spec 001 #24: a variable missing from `config.Env` is one nobody finds, and the start-up warning would call a person's exported `TRACEPAD_VERSION` a typo. The download address is the only test seam, and it is also a real feature for a mirror or an air-gapped machine. `curl` reads `file://`, so the whole script runs under test without a server. On CI's Ubuntu, `sh` is `dash`, which checks the "POSIX" claim on every push. |
| 11 | **2026-10-05** — **`docs/agent-setup.md` is written to the agent, and the procedure itself is the skill's `references/setup.md`.** The page is short. It installs or checks the binary, then has the agent print `tracepad skills show setup.md` and follow that. It adds an outline of the steps, the table for choosing a path, the Docker variant, what needs the person, and what the report must say. A binary older than the reference (`0.1.0-rc.1`) answers "no such file", and the page then sends the agent to the file on `main`. The procedure is numbered steps, and each step has a command, what a healthy answer looks like, and what to do otherwise. The steps: install or check the binary; find or start a server; get a key; detect the project's language and existing tracing, then follow `references/setup.md` and `instrumenting.md` of the skill to connect it; send a test span; run the application's own path; read both traces back with the CLI; report to the person. Next to the steps is a list of **what needs the person**: a provider's API key for the model calls, opening the port beyond this machine, a key on a server the agent did not start, creating the browser account, anything that deletes, and a service that outlives the session. The SDK's own pages hold the shapes. | The page is the second line's target, read by a model that will act on it. An instruction it can follow and check is worth more than prose it has to interpret. Three things put the commands in the skill rather than on the page. Agents' web-fetch tools often hand the model a summary of a page instead of its text, while `tracepad skills show` prints the file verbatim. The file matches the installed binary's version. And the drift test checks its commands against the binary (spec 037 #8, #17 here). Writing the procedure on the page as well would make two copies that drift apart. |
| 12 | **2026-10-05** — **A key the agent declares itself, under three conditions; otherwise keys come from the person** (owner decision 2026-10-05). The agent creates the project's key only when all three hold. (a) It started the server itself, in this setup. (b) The server listens on loopback only (`127.0.0.1` / `::1`, the binary's default). (c) Its data directory was fresh: no `tracepad.db` in it before this start. Then the agent generates `tp-pk-…` and `tp-sk-…` (`openssl rand -hex`), starts the server with `TRACEPAD_PROJECTS=<project>:<pk>:<sk>`, and names the project after the repository. The secret goes into the project's `.env`, piped in from the generator and never printed. Before writing `.env`, the agent makes sure git ignores it (`git check-ignore .env`). If it does not, the agent adds `.env` to `.gitignore` and tells the person it did. Declared keys hold all three scopes (spec 045 #28) and are never written to the log, so the same key sends the test span and reads it back. The report tells the person to mint an `ingest`-only key for each application before production. **On any other server** — one already answering, one on a data directory that has a database, one listening beyond loopback — the skill's rule stands: ask the person for a key, and never search for one or create it. **The fallback**, when the conditions do not hold or the person declines: the application gets the `ingest` key the first start prints (read from the server's log by a pipe into `.env`, never echoed), and the read-back waits for a `read` key the person mints in Settings → Project → API keys and hands over. Until then the 200 from the test span is the only check, and the report says so. | The skill says "never mint a key", and that rule protects servers that belong to someone else. A loopback server on a fresh directory, which the agent starts on the person's machine because the person asked, has no other owner and no data yet, and nobody else can reach it. Without the carve-out, the agent cannot read back the trace it was asked to confirm, and verification falls to the person, which defeats the second line. A declared key is the path the server already supports for a deployment that never prints a secret (spec 001 #12). Each of the three conditions closes one way the exception could reach someone else's data: a server already running, an existing database, or a port open to the network. The `.gitignore` check closes the fourth way, a committed secret. |
| 13 | **2026-10-05** — **The skill learns setup.** `references/setup.md` covers the case "Tracepad is not here yet". It explains how to tell whether a server exists, the decision behind #12, starting the server in the background with its PID and log in the data directory, what to hand back to the person, and how to restart. `instrumenting.md` stays the reference for connecting an application. `SKILL.md` gains one routing line, and its description gains "set up or install Tracepad for a project". `SKILL.md` was at its budget of 200 lines and the skill at 750 of 900 (spec 037 #1). The reference takes 150 lines, so the skill is at 900, its budget. `SKILL.md`'s three new lines are paid for by rewording three paragraphs (the reference list's intro, *Connecting*'s first paragraph, the `GET /api/v1` bullet), with nothing dropped. *Connecting*'s rule "nor mint one yourself" names the setup exception and points at `setup.md`. The drift test checks the new reference like the others. | An agent that installed the skill through the first line will be asked to "set up Tracepad" and must find the method in the skill, not only on a web page it may never fetch. |
| 14 | **2026-10-05** — **Docs for agents: `llms.txt`, `llms-full.txt`, and every page as Markdown.** The docs hook writes three outputs into each version. (a) `<page>.md` beside the HTML, as the source after the same link rewriting the HTML build gets. Relative links between pages therefore still resolve, as Markdown. (b) `llms.txt` per llmstxt.org: an H1, a blockquote summary, and one H2 per navigation section listing every page as `[title](absolute URL of its .md): description`, the description being the page's first paragraph cut to a sentence. (c) `llms-full.txt`: every page in navigation order, each under a comment naming its URL. The URLs in `llms.txt` point at the site's root mirror (#3). `site.yml` deploys `dev` on changes to `scripts/install.sh` too. | llmstxt.org is the convention agents look for. The `.md`-beside-the-page form is what llmstxt.org recommends and what the root mirror can copy without any further layout. Generating at build time, from the navigation in `mkdocs.yml`, means a new page cannot be missing from either file. |
| 15 | **2026-10-05** — **The front door.** `docs/quickstart.md` and the README's *Getting it* open with "With your coding agent": the two lines and two sentences on what happens. Everything after that stays as it was, so existing anchors (`#1-run-the-server`, `#getting-it` and the others `doc-anchors` checks) keep their targets. `install.md` gains *With the install script*, ahead of the Homebrew section. Until 0.1.0 is out, every place that shows the terminal line also shows the pinned form for the candidate. | The person who wants the manual path keeps it unchanged. The person with an agent finds the short path first. |
| 16 | **2026-10-05** — **An audit of `docs/` for agents.** Every page is read against five questions. Can each command be copied and run, with no `…` left to fill in except one that is named? Does each command show what a healthy answer looks like? Is any step only a screenshot or only a click, with no command or API equivalent? Are environment variables named explicitly? Are the CLI's and the API's answers shown? Fixes that are a line or a block are made in this PR. Findings that need a design change are listed in the PR description for the coordinator. **Done 2026-10-05**: about thirty one-line or one-block fixes across `quickstart`, `install`, `docker`, `ingest`, `configuration`, `langfuse-sdk`, the three SDK pages, `cli`, `api`, `admin`, `retention`, `annotation`, `datasets`, `accounts`, `prompts`, `export` and `ui`, each checked against the source. Examples: `tracepad traces` with no subcommand is a usage error; `api.md`'s table lacked five routes; `admin.md` said the printed key holds all three scopes; three files and two variables were used but never defined. | The agent line sends a model through these pages. A step it cannot check is where it guesses. |
| 17 | **2026-10-05** — **The drift test checks `tracepad serve`.** Amends spec 037 #8, whose test reported every `serve` and `mcp` invocation as one it could not check ("if the skill grows one, this test grows the check first"). The setup reference starts a server, so `serve`'s flags are now checked against the flags `config.Env` pairs with a variable (`--listen`, `--data-dir`), the table `tracepad help` prints them from. `nohup` joins `$`, `sudo` and `VAR=value` as words that stand in front of the binary. The fixture's `serve --listen` defect becomes `serve --port`, an unknown flag, and `tracepad mcp` takes over as the invocation that cannot be checked. | The alternative was to leave the command out of the skill, which would have put the most error-prone step of the setup where no test reads it. |
| 18 | **2026-10-05** — **What the live run found, and what changed for it.** (a) **The script against the real `0.1.0-rc.1`**, in a clean `HOME`. Unpinned, it stops with the pinned line naming `0.1.0-rc.1`; this is where #5's HTTP-status rule came from. Pinned, it installs, the checksum matches and the skill goes in. A re-run says "already installed". With `~/.codex` the skill goes to `~/.agents/skills`. With a logged-in `gh`, the attestation is verified. (b) **`agent-setup.md` followed by a fresh agent session** on a small Python project with no tracing, on a spare port and a fresh data directory: the key declared, `.gitignore` created, the test span `200`, the `tracepad` package wired in, and both traces read back with model and usage. A person then opened the setup link and saw both traces in the interface. (c) **Fixed from what the agent hit**: the start command now has a `port=` that every address uses, and the health check names its URL. Before, on another port, `tracepad health` asked `localhost:4318`, and the other program there answered `ok`. The read-back re-exports the two variables, because an agent's shells do not keep them. The packages' candidate pins are named. A commit hash from `tracepad version` counts as another build. `.gitignore` is created if missing. The log does show the public key; the docs had said it showed no key. | A procedure for agents is tested by an agent. Two of these defects (the health check that passed against the wrong server, the variables lost between shells) did not appear when the steps were run by hand in one shell. |
| 19 | **2026-10-05** — **Review fixes.** Amends #7, #9, #12, #14 and #17. (a) **#7**: the new binary's `version` is run as the temporary file next to the target, *before* the rename. A binary that does not run here (a noexec mount, another architecture) or says another version is removed, and the old one is left as it was. (b) **#9**: an unpinned run never steps back. When the installed version is later than the newest stable release (by semver: a release is later than its candidates, alpha < beta < rc), the script says so, changes nothing, and prints the pinned line for going back. A pinned older version installs, as "downgraded", with a warning that an older binary does not open a database a newer one migrated. (c) **#4 (7)**: the post-install hint asks `localhost:4318` explicitly, not `TRACEPAD_URL`, and a watchdog stops the probe after three seconds. (d) **#12**: every write to `.env` replaces the variable's line in place (a `put` function), so a re-run, or a variable `.env` already had, leaves exactly one line. The start command is one block for both branches: `declare=yes` declares the key, and `declare=no` moves the printed `ingest` key. Either way it writes `TRACEPAD_URL` and `TRACEPAD_API_KEY`, and a second block builds the path's lines (OpenTelemetry or Langfuse) from what `.env` holds. A key the person put there themselves takes the same second block. The block puts `~/.local/bin` on `PATH`, checks that the process is alive and answers `health` on its port, and only then writes `.env`. A busy port therefore ends with the log's last lines and an untouched `.env`. The Docker variant is a whole block with the same `port` and `declare`, and `-p 127.0.0.1:$port:4318`. (e) A candidate (`0.1.0-rc.1`) counts as installed; only a missing binary or a development build sends the agent to the script. (f) **#17**: the drift test parses a `serve` line with `config.ParseFlags`, the function `serve` reads its arguments with (spec 001 #25's single `flagTargets` table). It therefore refuses a flag the server lacks and a positional word, exactly as a start would. The fixture has one of each. (g) **#14**: the agents' copy of a page is the text `on_page_markdown` returned to the build, kept by page in one pass (`BUILT`, emptied by `on_pre_build`), not a second read of the file. | The review's ten findings, each a way the first version could leave a person worse off than before: an overwritten binary, a downgraded one, a `.env` with two keys, a server that failed silently, an installer that waited on someone else's host. |
| 20 | **2026-10-05** — **Second review.** Amends #11, #13, #14, #17, #19 and spec 037 #1. (a) **The Docker variant is part of the procedure.** It lives in `references/setup.md` as `how=docker` in the one start command, where the drift test reads it. `agent-setup.md` describes it and links to it, and copies no command. The image is `ghcr.io/tracepad/tracepad:<the binary's version>` and only a release or candidate version: an empty one, or `dev`, stops before `docker run` with that reason. Docker's own error (a busy port, a taken name, a missing tag) is what the agent reports. The container and its volume are named after the project (`tracepad-<project>`). (b) **The server knows its address.** The start passes `TRACEPAD_URL=http://localhost:$port` to the binary and the container, so the setup link and the connection lines it prints carry the real port (verified on 4319). (c) **The public key is this start's.** It is generated with the declared key, or read from this server's own log (the file, or `docker logs`). The start command writes it to `.env` when the path is Langfuse, and the path block reads it from `.env`. Either one stops, writing nothing, when the key is missing, never writing it empty. (d) **No orphans.** The start waits up to ten seconds for `health`, and counts it only when its own process is alive and has logged `listening addr` since this start. Otherwise it stops that process, removes the new PID file, prints the log's last lines and leaves `.env` as it was. (e) **`dev`** is what a development build's `tracepad version` prints (never a hash): it serves, and has no image. (f) **Tests that test.** The install test's sandbox has `sleep`, so the hanging-server case times the real watchdog; it turns red when the watchdog kills the wrong process. The drift test's tokenizer drops a redirection in every spelling (`>> file`, `>file`, `2>&1`, `<in`) for every command, and keeps a `<placeholder>`. `serve` needs no special case. (g) **The docs hook** reads a page from `docs/` and rewrites it when a dirty build (`mkdocs serve --dirty`) did not hand it over. (h) **`deploy.sh`** builds the root's index with one `git update-index --index-info`. (i) **The skill's total budget is 910 lines** (spec 037 #1 said 900). Moving the Docker variant in cost 17 lines; rewording paid back 9; the rest is this raise. `SKILL.md` stays at 200. | The second review found the first version's Docker block to be the two-copies drift that #11 gave as the reason for the skill, already diverged (no `PATH`, the wrong port in the links). The rest are the cases where an agent would have reported the wrong cause or left something running. The budget is raised rather than squeezed further: the lines are a whole procedure an agent runs as written, and squeezing it into longer lines would make it harder to read and no shorter to load. |

## Application contract

- `scripts/install.sh`: POSIX `sh`. Reads `TRACEPAD_VERSION`, `TRACEPAD_INSTALL_DIR`,
  `TRACEPAD_NO_SKILL` and `TRACEPAD_DOWNLOAD_URL`, and nothing else from the
  `TRACEPAD_*` space. It needs `curl`, `tar`, `uname`, `mktemp` and `sha256sum`
  or `shasum`; `gh` is optional. Exit `0` on success, `1` on any failure, with
  the reason on stderr. The summary goes to stdout, and its last line is the
  agent line.
- `internal/config/env.go`: the `EnvInstaller` kind and its four variables.
  `cmd/tracepad/env_test.go`: the script reads exactly those.
- `scripts/docs-site/hooks.py`: the per-page Markdown, `llms.txt`,
  `llms-full.txt`, and the copy of `install.sh` into the build, with tests in
  `hooks_test.py`.
- `scripts/docs-site/deploy.sh`: after `set-default`, a commit that copies
  `install.sh`, `llms.txt`, `llms-full.txt` and `*.md` from the default
  version's directory to the branch's root, and deletes root files of those
  kinds that the version no longer has.
- `docs/agent-setup.md` (new), in `mkdocs.yml` and `docs/index.md`.
- `agent/skills/tracepad/references/setup.md` (new); `SKILL.md`.
- `Makefile`: `install-script-test`, in the gate.
- `.github/workflows/site.yml`: `scripts/install.sh` in the paths of a `dev`
  deploy.

## Testing

- `make install-script-test`: shellcheck, then the offline cases of #10(b).
- `make docs-site-test`: the hook's new outputs. `llms.txt` lists every page
  in the navigation and nothing else, each `.md` exists beside its page, and
  links in the copies are rewritten as in the HTML.
- `make docs-build` (`--strict`) and `make doc-anchors`.
- `make docs-site` (the rehearsal): the root holds `install.sh`, `llms.txt`
  and the pages' Markdown, copied from `latest`. A redeploy of `dev` alone
  leaves them alone, and the copies match the files under `v0.1/`.
- **Live, by hand, recorded in the PR**: a clean temporary `HOME`, the
  terminal line from `raw.githubusercontent.com` (the site's copy exists only
  after the merge) with `TRACEPAD_VERSION=0.1.0-rc.1` against the real
  release: checksum verified, binary and skill installed, a re-run says
  "already installed". Then `docs/agent-setup.md` followed by a fresh agent
  session on a small demo application: the server started, the key declared,
  the application connected, the test span and the application's trace read
  back with the CLI, and the trace seen in the interface.

## Edge cases

- **No stable release yet** (true until 0.1.0): exit `1`, with the pinned line
  (#5).
- **A pinned version that does not exist**: the download of its
  `checksums.txt` fails. The message names the version and the releases page.
- **A server already at `localhost:4318`, not started by the agent**: the
  agent asks for a key (#12) and does not start a second server.
- **No server running, but the default data directory already holds a
  `tracepad.db`** (an earlier install): the directory is not fresh, so the
  agent does not declare a key. It asks the person whether to start that
  server, which falls under the fallback of #12, or to use a new data
  directory for this project.
- **The port is taken by something that is not Tracepad**: `tracepad health`
  fails while the port is open. The agent picks another port with
  `--listen`, and every address it writes uses that port.
- **`.env` is not ignored by git**: the agent adds it to `.gitignore` before
  writing the key, and says so in the report (#12). If `.env` is already
  tracked, ignoring it does not untrack it, so the agent writes no key there
  and asks the person where the key should go.
- **macOS quarantine**: `curl` sets no quarantine mark, so Gatekeeper does not
  apply (`install.md`).
- **An install directory that holds a `tracepad` the script did not put
  there** (from Homebrew, say): `~/.local/bin` is not Homebrew's directory, so
  nothing is overwritten. The summary says which `tracepad` comes first on
  `PATH` when that is not the one just installed.

## Docs to touch

- `docs/agent-setup.md` (new), `docs/index.md`, `mkdocs.yml`.
- `docs/quickstart.md`, `README.md`: the first block (#15).
- `docs/install.md`: *With the install script*.
- `docs/agents.md`: the setup reference, and the script's skill install.
- `docs/configuration.md`: the installer's table.
- `CONTRIBUTING.md` / `AGENTS.md`: the new make target, the script's place,
  and the status line.
- `CHANGELOG.md`: *Added*.
- Whatever the audit fixes (#16).

## Out of scope

- A Windows installer (PowerShell). The Windows archives are in
  `install.md`.
- Installing a service (systemd, launchd) from the script: that outlives the
  session and is the person's decision (`install.md` has both).
- Editing shell profiles or agent configuration files (MCP blocks) for the
  person.
- A custom domain for the site, which would change both lines' addresses.
- Listing the script on third-party skill or installer registries.
