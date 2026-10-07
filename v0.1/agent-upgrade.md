# Agent upgrade

This page is an instruction to a coding agent that a person has asked to
update Tracepad on this machine: "Update Tracepad to the latest release", or to
a version they name. A person reading it learns what the agent will do and
what it leaves to them. The work is done by one command of the binary,
[`tracepad upgrade`](cli.md#upgrade), which a person can run as well;
[install.md](install.md#upgrading) and
[docker.md](docker.md#upgrading-and-backing-up-first) cover what it leaves to
them.

The line a person gives you:

```text
Update Tracepad to the latest release: follow https://tracepad.github.io/tracepad/agent-upgrade.md
```

## What to do

The steps, with their commands and what each exit status means, are in the
agent skill the binary carries. Print it and follow it:

```sh
tracepad skills show upgrade.md
```

If your agent loaded the `tracepad` skill, this is the same file as its
`references/upgrade.md`. *The skill has no file "upgrade.md"*: the binary is
older than the command, and the file is on `main`:

```sh
curl -fsSL https://raw.githubusercontent.com/tracepad/tracepad/main/agent/skills/tracepad/references/upgrade.md
```

*command not found*: Tracepad is not installed here, and
[agent-setup.md](agent-setup.md) is the page to follow.

In outline: `tracepad upgrade --plan` changes nothing and says what the upgrade
will do and what is the person's; you show it to them. `tracepad upgrade` then
backs the data up into `~/tracepad-backups/<run>/` before it stops anything,
puts the new binary in place, starts the server again with the same
arguments and environment, and checks it — for a container, it archives its
volume, renames it aside and runs the release under its name with the
`docker run` it was created with. A new version that exits or answers
as another version is rolled back at once; one that
runs but stays silent through the wait (a long migration runs before the
server listens), or answers with a trace count that looks wrong, is left
running, and the person decides (exit `4`). Nothing is ever deleted:
what a way back replaces is set aside, renamed. When it replaced the binary,
you plan once more with the new one, whose advice for what is the person's is
the new release's. Then the project's `tracepad` package moves to the same
version, and you report.

## What needs the person

- A server the command does not start or stop itself: a service (systemd,
  launchd), a server reachable from other machines, one that runs another
  binary (Homebrew's: `brew upgrade tracepad`); and a container it cannot
  recreate exactly — Compose's, one published beyond this machine, one with a
  setting its `docker run` would not carry. The plan names each, with its
  reason and the commands for it — for a container: stop it, back its volume
  up, pull the release, and the `docker run` it was created with, read from
  `docker inspect`, its variables passed in a file and never printed; for
  Compose's, the same through its project and its file, the volume under the
  name Compose gave it. A development build where the binary is installed is
  the person's too: the plan gives the install script's line that replaces it.
- A trace count that is lower after the upgrade, or cannot be read: keep the
  new version, or go back to the old one, which drops what arrived since.
- Free space for the backup, when there is not enough.
- An application firewall's question (LuLu, Little Snitch): a step that reaches
  the network and seems to hang — the plan's look at the releases, a
  download — is often a new binary's first connection, held until someone
  answers the firewall's window. The firewall asks once per path: a binary
  older than `tracepad upgrade` is upgraded through a release the skill
  fetches into a new directory of that upgrade's own,
  `~/.cache/tracepad/tmp.XXXXXX/`, so it asks once for each such upgrade.
- Deleting the backups, and whatever a way back set aside.

## The backup, said plainly

A run directory in `~/tracepad-backups/` is a full copy of the database —
every prompt and completion — and of the server's environment, secrets
included. It stays until someone deletes it, and erasing traces or a user's
data never reaches it, nor what a way back set aside. The report names each
and gives the commands to remove them; deleting is the person's, once the new
version has run for a while.
