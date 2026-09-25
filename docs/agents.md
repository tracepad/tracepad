# Coding agents

A coding agent can reach everything Tracepad has without help: the CLI answers
in JSON when piped, the MCP server is in the binary, and `GET /api/v1`
describes every route. What it lacks is the order of work — which door to use,
what to read before writing, what a healthy answer looks like, and which acts
need a human. The **agent skill** teaches that order, and it ships inside the
binary so that its version is always the server's.

This page is the whole setup: install the skill, connect MCP if the agent
speaks it, and give the CLI its two variables.

## Installing the skill

```sh
tracepad skills install
```

```
installed 0.4.0 to /home/you/.claude/skills/tracepad
```

That is the user-wide skills directory of Claude Code, which every session of
that user reads. Two other places:

```sh
tracepad skills install --project              # ./.claude/skills/tracepad, for one repository
tracepad skills install --dir ~/.agents/skills # DIR/tracepad, for any other agent's skills directory
```

`--project` writes into the working directory's `.claude/skills`, which a
repository can commit so that everyone who clones it has the skill. `--dir`
is for any agent that reads the same layout — a `SKILL.md` with a
`references/` directory beside it — from another place.

The skill is plain files: `SKILL.md`, which the agent loads when a task
matches its description, and four references it reads only when the task
needs one — debugging, instrumenting a service, evals, administration.
`tracepad skills show` prints `SKILL.md` and `tracepad skills show
debugging.md` a reference, for reading it without installing it.

Neither command talks to a server or needs a key.

### Updating

Run the same command again after upgrading the binary:

```
updated 0.3.1 → 0.4.0 in /home/you/.claude/skills/tracepad
```

The directory is replaced whole, so a reference the new version dropped does
not linger. The installed `SKILL.md` carries the binary's version in its
frontmatter (`metadata.version`), and the skill tells the agent to compare it
with `tracepad version` and to reinstall when they differ — a development
build stamps `dev`, which matches anything.

A `tracepad` directory the command did not install — one without the
`.version` file it writes — is refused rather than overwritten, with the
directory named; `--force` replaces it. A `tracepad` that is a symlink — a copy
kept in a dotfiles repository — is updated where it points, but only if what it
points at is a skill this command installed: a link to any other directory is
refused even with `--force`, because replacing it would delete a directory
you never named. Remove the link and install again.

### From the Docker image

The image runs as `nonroot`. Mount the host's skills directory, run as your
own user so the files are yours, and create the directory first — a mount
point Docker creates for you is owned by root:

```sh
mkdir -p ~/.claude/skills
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$HOME/.claude/skills:/skills" \
  ghcr.io/tracepad/tracepad skills install --dir /skills
```

`docker run --rm ghcr.io/tracepad/tracepad skills show` prints the skill
without writing anything.

### By copying

`agent/skills/tracepad/` in the repository is the skill, file for file. A copy
of it works as it is, with `metadata.version: dev`.

## Connecting MCP

The server serves MCP at `/mcp`, and the binary serves the same tools over
stdio. Both configuration blocks, and what each tool does, are in
[mcp.md](mcp.md#connecting). The tools only read: an agent writes and deletes
through the CLI or the API, where the dry-run ceremony applies.

The skill works without MCP — the CLI is the door it uses first when the agent
has a shell — and uses the tools when the client has them.

## The CLI's environment

```sh
export TRACEPAD_URL=http://localhost:4318
export TRACEPAD_API_KEY=tp-sk-…
```

Piped, every command prints the API's JSON; exit `1` is a request that failed
and `2` a command typed wrong. The rest is [cli.md](cli.md). The SDKs read
`TRACEPAD_HOST` rather than `TRACEPAD_URL` — the skill's instrumenting
reference says so, because it is the variable an agent mixes up.

## What the skill holds the agent to

Most of the skill is method: start from the trace rather than the
application's logs, read the observation's input before guessing at the
prompt, list the values a filter takes before guessing one. One part is
policy. Every destructive command in Tracepad is a dry run until it is
confirmed ([admin.md](admin.md#dry-run-by-default)), and that ceremony
protects against a typo, not against an agent that has decided to delete.
The skill closes the gap: the agent runs the dry run, shows the human what it
would remove, and stops; it adds `--yes` only on the human's word about that
preview, never on its own initiative. It also never prints or commits a secret
key, and asks for one rather than hunting for it.
