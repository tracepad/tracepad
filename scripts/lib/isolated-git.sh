# shellcheck shell=bash
# Sourced, not run. Every script and test that makes a git repository of its
# own does it through this file (spec 020 #35).
#
# Why it exists: `make gate` runs from git's pre-push hook, whose environment
# carries GIT_DIR (in a linked worktree, the worktree's directory inside the
# SHARED repository) and its kin. A bare `git init <path>` or `git config` under
# that environment acts on the real repository: twice it rewrote the shared
# `.git/config` with `core.bare = true` and a test identity, which breaks git in
# every other worktree on the machine. The same is true of the machine's own
# git configuration (a signing key, a hooks path) leaking the other way. So:
#
#   . "$(dirname "$0")/lib/isolated-git.sh"
#   isolated_git_sandbox "$tmp"           # once, with the scratch directory
#   scratch_repo "$tmp/repo"              # git init, with an identity of its own
#
# `isolated_git_sandbox` clears every GIT_* variable, points git's global and
# system configuration at nothing, gives it a home of its own, and sets the
# ceiling so that no repository above the sandbox is ever found by discovery.
# `scratch_repo` refuses a path outside the sandbox and always passes `git init`
# an explicit one. scripts/gate-guard.sh is the other half: it fails a gate
# that left the real repository different.

# isolated_git_sandbox <dir>: dir is the scratch directory everything is made in.
isolated_git_sandbox() {
	local var
	[ -n "${1:-}" ] || { echo "isolated_git_sandbox: no directory" >&2; return 2; }
	ISOLATED_GIT_SANDBOX="$(cd "$1" && pwd -P)"
	while IFS= read -r var; do unset "$var"; done < <(env | sed -n 's/^\(GIT_[A-Za-z_0-9]*\)=.*/\1/p')
	mkdir -p "$ISOLATED_GIT_SANDBOX/home"
	export HOME="$ISOLATED_GIT_SANDBOX/home"
	export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
	export GIT_CEILING_DIRECTORIES="$ISOLATED_GIT_SANDBOX"
	export GIT_TERMINAL_PROMPT=0
}

# scratch_repo <path>: an empty repository at path, which must lie inside the
# sandbox, with an identity and no signing or hooks of its own.
scratch_repo() {
	local path="$1" parent
	[ -n "${ISOLATED_GIT_SANDBOX:-}" ] || { echo "scratch_repo: isolated_git_sandbox was not called" >&2; return 2; }
	mkdir -p "$(dirname "$path")"
	parent="$(cd "$(dirname "$path")" && pwd -P)"
	case "$parent/$(basename "$path")" in
	"$ISOLATED_GIT_SANDBOX"/*) ;;
	*) echo "scratch_repo: $path is outside the sandbox $ISOLATED_GIT_SANDBOX" >&2; return 2 ;;
	esac
	git init -q "$path"
	git -C "$path" config user.email scratch@example.invalid
	git -C "$path" config user.name scratch
	git -C "$path" config commit.gpgsign false
	git -C "$path" config tag.gpgsign false
	git -C "$path" config core.hooksPath /dev/null
}
