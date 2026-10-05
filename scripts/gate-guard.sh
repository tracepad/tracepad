#!/usr/bin/env bash
# Runs a command and fails if it left the repository different (spec 020 #35).
#
#   scripts/gate-guard.sh make gate-checks
#
# The gate makes scratch repositories (scripts/lib/isolated-git.sh) and runs from
# a git hook, whose GIT_DIR names the real repository; a test that forgets the
# isolation rewrites the real `.git/config` instead of its own, and git breaks in
# every worktree of the machine. The isolation prevents it; this notices when it
# did not, and names the key. The command's own failure is returned as it is.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$#" -gt 0 ] || { echo "usage: $0 <command> [args...]" >&2; exit 2; }

before="$("$here/repo-fingerprint.sh")" || { echo "gate-guard: cannot read the repository's state" >&2; exit 2; }
"$@"
status=$?
after="$("$here/repo-fingerprint.sh")" || { echo "gate-guard: cannot read the repository's state after the run" >&2; exit 2; }

if [ "$before" != "$after" ]; then
	echo "gate-guard: the run changed the repository it ran in — a script made a git repository without scripts/lib/isolated-git.sh:" >&2
	diff <(echo "$before") <(echo "$after") | grep '^[<>]' | sed 's/^</  was:/; s/^>/  now:/' >&2
	echo "gate-guard: repair it with 'git config --local --unset <key>' (and 'git config --local core.bare false'), then fix the script" >&2
	exit 1
fi
exit "$status"
