#!/usr/bin/env bash
# scripts/rehearsal-tag.sh, on repositories made for the purpose (spec 020 #34):
# a release above its candidates, a maintenance branch, a HEAD older than the
# newest tag, and a clone with SDK tags alone.
set -euo pipefail

# This runs inside the pre-push hook, whose environment points git at the
# repository being pushed (GIT_DIR and the like), and on machines whose own
# configuration installs hooks everywhere (core.hooksPath): neither may reach
# the repositories made here, or their commits run the checks of this one.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX GIT_OBJECT_DIRECTORY GIT_COMMON_DIR
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1

script="$(cd "$(dirname "$0")" && pwd)/rehearsal-tag.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "rehearsal-tag-test: $*" >&2; exit 1; }

repo() { # <name>: an empty repository with a commit identity of its own
    git init -q "$work/$1"
    git -C "$work/$1" config user.email test@example.invalid
    git -C "$work/$1" config user.name test
    git -C "$work/$1" config commit.gpgsign false
    git -C "$work/$1" config tag.gpgsign false
}
commit() { git -C "$1" commit -q --allow-empty -m "$2"; }
expect() { # <repo> <want>
    got="$(cd "$work/$1" && "$script")" || fail "$1: the script failed"
    [ "$got" = "$2" ] || fail "$1: got '$got', want '$2'"
}

repo line
commit "$work/line" one
git -C "$work/line" tag v0.1.0-rc.1
git -C "$work/line" tag sdk-js/v0.1.0-rc.1     # the tie that took GoReleaser to a slash
git -C "$work/line" tag sdk-py/v0.1.0-rc.1
expect line v0.1.0-rc.1
git -C "$work/line" branch maintenance
commit "$work/line" two
git -C "$work/line" tag v0.1.0-rc.2
commit "$work/line" three
git -C "$work/line" tag v0.1.0
commit "$work/line" four
expect line v0.1.0                              # the release, not rc.2 above it in a sort
git -C "$work/line" checkout -q v0.1.0-rc.2
expect line v0.1.0-rc.2                         # HEAD older than the newest tag
git -C "$work/line" checkout -q maintenance
commit "$work/line" fix
expect line v0.1.0-rc.1                         # a branch that does not contain the later tags

repo sdkonly
commit "$work/sdkonly" one
git -C "$work/sdkonly" tag sdk-js/v0.1.0-rc.1
git -C "$work/sdkonly" tag sdk/go/v0.1.0-rc.1
if out="$(cd "$work/sdkonly" && "$script" 2>&1)"; then
    fail "sdkonly: printed '$out' instead of failing"
fi
case "$out" in *"no tag named v<version>"*) ;; *) fail "sdkonly: the refusal does not say why: $out" ;; esac

echo "rehearsal-tag-test: the nearest server tag, from HEAD, and a refusal when there is none"
