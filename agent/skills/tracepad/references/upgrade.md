# Upgrading Tracepad

`tracepad upgrade` does the upgrade: it backs the data up before it stops
anything, swaps, checks the new version, and goes back by itself when that exits
or answers as another, deleting nothing. Your part: run the plan, show it to the human,
run the upgrade, act on its exit status, and report. Never improvise around a
refusal: it says what is the human's.

## 1. The plan

With the key in the environment (never on a command line), so the trace counts
before and after can be compared:

```sh
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env 2>/dev/null)"
tracepad upgrade --plan
```

`--to 0.2.0` for a version the human named. *unknown command "upgrade"*: the
binary predates the command; plan and upgrade with a release's, fetched into a
directory of its own (it upgrades the installed one), `v` the version named:

```sh
v=; dir="$(mktemp -d)"; curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$v" TRACEPAD_INSTALL_DIR="$dir" TRACEPAD_NO_SKILL=1 sh >/dev/null && "$dir/tracepad" upgrade --plan ${v:+--to "$v"}
```

The exit status: `10` an upgrade is pending; `0` nothing to do; `4` only what
is the human's is behind (a server behind an installed candidate too); `1`
refused or interrupted — say why (*no stable release*, from the plan or the install script: name the candidate it gives, and ask). Show the human what it will do, and what it says
is theirs: a service, a Compose project, a server or container open beyond
this machine — show them the commands the plan gives for each. More than one
server or container that is the command's: ask which, and pass its
`--data-dir` or `--container` to the upgrade.

## 2. The upgrade

The same command and flags without `--plan`. Its exit status:

- `0`: upgraded, healthy (or nothing at all was behind). After an upgrade, run
  `setup.md`'s step 4 span and step 6 read-back.
- `3`: not upgraded; the old version runs again. Say what failed (the first
  line of the report) and stop.
- `4` with a run in the report: the new version runs, but its check needs the
  human (a lower or unreadable trace count, or a server still starting). Show
  the evidence, and ask: keep it (`tracepad upgrade --check <run>` later), or
  `tracepad upgrade --back <run>`, which drops what arrived since. Never
  decide for them. `4` with no run: nothing of the command's was behind, only
  what is the human's; show them that list.
- `10`: nothing to do for the one named, and another server of the command's
  is behind. Run the upgrade the report's `next` gives, and act
  on its status.
- `5`: stuck. Stop, and give the human the report as it is.
- `1`: nothing changed; say why.

## 3. The package

The command installed the skill again wherever a copy was. A pinned
`tracepad` package goes to the same version in the project's own tool:
`tracepad==0.1.0rc2` (PyPI's spelling), `tracepad@0.1.0-rc.2` (npm), Go's
`…/sdk/go@v0.1.0-rc.2`. Not published: keep the pin, and say so. Run the
project's tests.

## 4. Report to the human

- **From and to**, and the check: the first log line, `health`, the counts.
- **The backup, in the report's words**: the run directory is a full copy of
  the database, every prompt and completion, and of the server's environment,
  kept until someone deletes it; erasing traces or a user never reaches it,
  nor what a way back set aside. Give the report's commands to remove them
  once the new version has run a while; deleting is theirs.
- **What changed**: the skill's copies, the package's pin, the tests' result.
- **What is theirs**: the plan's list, with its commands.
