#!/usr/bin/env bash
# scripts/rehearsal-tag.sh, on repositories made for the purpose (spec 020 #34):
# a release above its candidates, a maintenance branch, a HEAD older than the
# newest tag, and a clone with SDK tags alone.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
script="$root/scripts/rehearsal-tag.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
# Every repository made here goes through the shared isolation (spec 020 #35):
# this runs inside the pre-push hook, whose GIT_DIR names the real repository.
# shellcheck source=scripts/lib/isolated-git.sh
. "$root/scripts/lib/isolated-git.sh"
isolated_git_sandbox "$work"

fail() { echo "rehearsal-tag-test: $*" >&2; exit 1; }

repo() { scratch_repo "$work/$1"; } # <name>
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
