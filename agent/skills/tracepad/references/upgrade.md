# Upgrading Tracepad

`tracepad upgrade` does the upgrade: it backs the data up before it stops
anything, swaps, checks the new version, and goes back by itself when that exits
or answers as another, deleting nothing. Your part: run the plan, show it to the human,
run the upgrade, act on its exit status, and report. Never improvise around a
refusal: it says what is the human's.

## 1. The plan

First `tracepad version` against the skill's `metadata.version`. Older, or a
development build (`dev`, a commit), and the binary goes first: plan with
`--to` the skill's version, which says how — the upgrade replaces a release's,
a package manager's names that manager's command, a development build gets the
install script's line. A skill that says `dev` names no release: ask which.

With the key in the environment (never on a command line), so the trace counts
before and after are compared: `setup.md` keeps it in `.env`; a project that
keeps it elsewhere (`config/.env.local`, say) has it read from there. Empty, they are not:

```sh
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env 2>/dev/null)"
tracepad upgrade --plan
```

`--to 0.2.0` for a version the human named. *unknown command "upgrade"*: the
binary predates the command; plan and upgrade with a release's, fetched into a
new directory of its own, `BRIDGE:`; `v` the version named. `rm -r` it once no
command you give the human names it (after a refusal, give them with the
installed binary's path when there is one). It plans for the installed binary,
and leaves one that is not a release's with the line that replaces it:

```sh
v=; if dir="$(mkdir -p "$HOME/.cache/tracepad" && mktemp -d "$HOME/.cache/tracepad/tmp.XXXXXX")"; then echo "BRIDGE: $dir"; else echo "STOP: no directory for the bridge (mkdir or mktemp said why above)"; false; fi && { curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$v" TRACEPAD_INSTALL_DIR="$dir" TRACEPAD_NO_SKILL=1 sh >/dev/null && [ -x "$dir/tracepad" ] || { rm -rf "$dir"; echo "STOP: the release did not download (curl said why above)"; false; }; } && if [ -n "$v" ]; then "$dir/tracepad" upgrade --plan --to "$v"; else "$dir/tracepad" upgrade --plan; fi
```

The exit status: `10` an upgrade is pending; `0` nothing to do; `4` only what
is the human's is behind (a server behind an installed candidate, or the
binary step 1 replaces, too); `1`
refused or interrupted — say why (*no stable release*, from the plan or the install script: name the candidate it gives, and ask). Show the human what it will do, and what it says
is theirs: a service, a Compose project, a server or container open beyond
this machine — show them the commands the plan gives for each. More than one
server or container that is the command's: ask which, and pass its
`--data-dir` or `--container` to the upgrade.

## 2. The upgrade

The same command and flags without `--plan`. Its exit status:

- `0`: upgraded, healthy (or nothing at all was behind). After an upgrade, run
  `setup.md`'s step 4 span and step 6 read-back; if it replaced the binary, plan
  again by the report's `binary` path and give the human what it says is theirs.
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
`tracepad==<version>` (PyPI; a candidate `X.Y.ZrcN`), `tracepad@<version>`
(npm; `X.Y.Z-rc.N`), Go's `…/sdk/go@v<version>`. Not published: keep the pin,
and say so. Run the project's tests.

## 4. Report to the human

- **From and to**, and the check: the first log line, `health`, the counts.
- **The backup, in the report's words**: the run directory is a full copy of
  the database, every prompt and completion, and of the server's environment,
  kept until someone deletes it; erasing traces or a user never reaches it,
  nor what a way back set aside. Give the report's commands to remove them
  once the new version has run a while; deleting is theirs.
- **What changed**: the skill's copies, the package's pin, the tests' result.
- **What is theirs**: the last plan's list, with its commands.
