#!/usr/bin/env bash
# scripts/gate-guard.sh and scripts/lib/isolated-git.sh, held to what they claim
# (spec 020 #35), under the conditions that broke the shared repository twice: a
# pre-push hook, in a linked worktree, running a script that makes a git
# repository of its own.
#
# In a scratch repository with a linked worktree and a pre-push hook that runs
# the guard around a command, `git push` from the worktree is made to run
#   - a `git init` and `git config` with the hook's environment untouched
#     (the mistake): the push must be refused, naming what changed;
#   - the same through isolated_git_sandbox and scratch_repo: the push goes
#     through and the repository is as it was;
#   - a command that fails and changes nothing: its status comes back.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Run from the gate, which runs from a pre-push hook: nothing of that hook's
# environment may reach the repositories made here.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# shellcheck source=scripts/lib/isolated-git.sh
. "$root/scripts/lib/isolated-git.sh"
isolated_git_sandbox "$tmp"

fail() { echo "gate-guard-test: $*" >&2; exit 1; }

scratch_repo "$tmp/main"
git -C "$tmp/main" commit -q --allow-empty -m one
git -C "$tmp/main" config --unset core.hooksPath     # this repository's hooks are the point
git init -q --bare "$tmp/remote.git"
git -C "$tmp/main" remote add origin "$tmp/remote.git"
git -C "$tmp/main" worktree add -q -b work "$tmp/linked"
git -C "$tmp/linked" commit -q --allow-empty -m two

# The hook is what the real one is: it runs the guard around a command.
hooks="$(cd "$tmp/main" && cd "$(git rev-parse --git-common-dir)" && pwd -P)/hooks"
mkdir -p "$hooks"
cat >"$hooks/pre-push" <<'HOOK'
#!/bin/sh
exec "$GUARD" bash -c "$HOOK_COMMAND"
HOOK
chmod +x "$hooks/pre-push"
export GUARD="$root/scripts/gate-guard.sh" ISOLATED="$root/scripts/lib/isolated-git.sh" VICTIM="$tmp/victim" INNER="$tmp/inner"

push() { # <command the hook runs>: git push from the linked worktree, output in $tmp/out
	HOOK_COMMAND="$1" git -C "$tmp/linked" push -q origin work:work >"$tmp/out" 2>&1
}
state() { git -C "$tmp/main" config --local --list | LC_ALL=C sort; }

before="$(state)"
# shellcheck disable=SC2016 # the hook's shell expands these, not this one
if push 'git init -q "$VICTIM" && git -C "$VICTIM" config user.email x@example.invalid'; then
	fail "a script that made a repository under the hook's environment passed the guard"
fi
grep -q "gate-guard: the run changed the repository" "$tmp/out" || fail "the refusal does not say what happened: $(cat "$tmp/out")"
grep -Eq "core\.bare|user\.email" "$tmp/out" || fail "the refusal does not name the key: $(cat "$tmp/out")"
if [ "$(state)" = "$before" ]; then
	fail "the unisolated script changed nothing, so this test proved nothing about the guard"
fi

# Back to what it was, for the cases that follow.
git -C "$tmp/main" config --local user.email scratch@example.invalid
git -C "$tmp/main" config --local core.bare false
[ "$(state)" = "$before" ] || fail "could not restore the scratch repository: $(diff <(echo "$before") <(state))"

# shellcheck disable=SC2016 # likewise
push '. "$ISOLATED"; d="$(mktemp -d)"; isolated_git_sandbox "$d"; scratch_repo "$d/repo"; git -C "$d/repo" commit -q --allow-empty -m x; rm -rf "$d"' ||
	fail "an isolated script was refused: $(cat "$tmp/out")"
[ "$(state)" = "$before" ] || fail "an isolated script changed the repository: $(diff <(echo "$before") <(state))"

# scratch_repo refuses a path outside its sandbox.
if (isolated_git_sandbox "$tmp" && scratch_repo "$tmp/../outside") 2>/dev/null; then
	fail "scratch_repo made a repository outside the sandbox"
fi

# The command's own failure is the guard's, and a failure is not a change.
status=0
(cd "$tmp/main" && "$root/scripts/gate-guard.sh" bash -c 'exit 7') || status=$?
[ "$status" -eq 7 ] || fail "a failing command's status came back as $status, want 7"

echo "gate-guard-test: a script that made a repository under a pre-push hook is refused, naming the key; an isolated one is not"
