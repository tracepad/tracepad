# Agent upgrade

This page is an instruction to a coding agent that a person has asked to
update Tracepad on this machine: "Update Tracepad to the latest release", or to
a version they name. A person reading it learns what the agent will do and
what it leaves to them. [install.md](install.md#upgrading) and
[docker.md](docker.md#upgrading-and-backing-up-first) cover the same ground by
hand.

**The goal**: the binary at the new version, the server you started on it,
with a backup taken first, the data checked against that backup, the agent
skill and the project's `tracepad` package at the same version, and a report
to the person. Work in order. Anything that does not answer as the procedure
says is a reason to stop and tell the person, not to improvise.

The line a person gives you:

```text
Update Tracepad to the latest release: follow https://tracepad.github.io/tracepad/agent-upgrade.md
```

## 1. The procedure

The steps, with their commands and their healthy answers, are in the agent
skill the binary carries. Print it and follow it:

```sh
tracepad skills show upgrade.md
```

If your agent loaded the `tracepad` skill, this is the same file as its
`references/upgrade.md`. A binary older than this page answers *the skill has
no file "upgrade.md"*: read the file from the repository instead.

```sh
curl -fsSL https://raw.githubusercontent.com/tracepad/tracepad/main/agent/skills/tracepad/references/upgrade.md
```

*command not found*: Tracepad is not installed here, and
[agent-setup.md](agent-setup.md) is the page to follow.

In outline:

1. **What is installed, and what runs it**: the binary, the servers running
   (the one you started has its PID in its data directory's `server.pid`) and
   the containers.
2. **The version to go to**: the newest stable release from GitHub, or the one
   the person named. An older version is refused: migrations run forward
   only, and an older binary does not open a database a newer one migrated.
3. **Your own server**: the install script with `TRACEPAD_VERSION`, the server
   stopped, its data directory archived into `~/tracepad-backups/`, the new
   binary started on the same port and directory.
4. **Your own container**: the image pulled, the volume archived from the
   stopped container, a new container with the same volumes, ports and
   variables, the old one kept, renamed, for the way back.
5. **The check**: the log's first line names the new version, `health` says
   it, the trace count is what it was, and a test span arrives. Otherwise the
   way back: the archive restored and the old binary or container started.
6. **The skill and the package**: the skill installed again wherever it was,
   and the project's pinned `tracepad` package moved to the same version, with
   the project's tests run.
7. **The report** (below).

## What needs the person

- A server you did not start: a service (systemd, launchd), a container
  Compose runs, one someone else started. You give them the commands, with
  its unit or container name, and leave it running.
- A server reachable from other machines: restarting it is an outage for
  whoever uses it.
- Free space for the backup, when there is not enough.
- A binary that a package manager installed (Homebrew: `brew upgrade
  tracepad`).
- Deleting the backup and the old container once the upgrade has proved
  itself.

## The report

- **From and to**: the versions of the binary and of each server.
- **The backup**: its directory, which holds the whole database and is as
  private as the data directory.
- **The checks**: the log's first line, `health`, the trace counts before and
  after, the test span.
- **What changed**: the skill's copies, the package's pin, and the result of
  the project's tests.
- **What is left for them**: the items above that apply, and anything that
  did not pass, with the answer you got.
