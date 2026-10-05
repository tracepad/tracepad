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
arguments and environment (or recreates the container with the same mounts,
ports, restart policy and variables), and checks it. A new version that does
not answer is rolled back at once; one that answers but whose trace count
looks wrong is left running, and the person decides. Nothing is ever deleted:
what a way back replaces is set aside, renamed. Then the project's `tracepad`
package moves to the same version, and you report.

## What needs the person

- A server the command does not start or stop itself: a service (systemd,
  launchd), a container Compose runs, a server reachable from other machines,
  one that runs another binary (Homebrew's: `brew upgrade tracepad`). The plan
  names each, with its reason and the commands for it.
- A trace count that is lower after the upgrade, or cannot be read: keep the
  new version, or go back to the old one, which drops what arrived since.
- Free space for the backup, when there is not enough.
- Deleting the backups, and whatever a way back set aside.

## The backup, said plainly

A run directory in `~/tracepad-backups/` is a full copy of the database —
every prompt and completion — and of the server's environment, secrets
included. It stays until someone deletes it, and erasing traces or a user's
data never reaches it, nor what a way back set aside. The report names each
and gives the commands to remove them; deleting is the person's, once the new
version has run for a while.
