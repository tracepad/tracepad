#!/usr/bin/env bash
# The tag and the package must agree about the version (spec 033 #1, spec 020
# #28).
#
#   scripts/sdk-go-release-check.sh sdk/go/v0.2.0
#   scripts/sdk-go-release-check.sh sdk/go/v0.2.0-rc.1
#   scripts/sdk-go-release-check.sh --print-tag     # the tag this tree is released by
#
# The Go module is released by its tag alone, and the proxy serves whatever the
# tag holds for good: `Version` in `sdk/go/http.go`, which is sent as the
# User-Agent, is the one place a wrong number can ship. Run it before the tag is
# pushed (`release-sdk-go.yml` runs it after, to be told at once).
#
# The major version is the module path's too: a `v2.0.0` tag on a module whose
# path does not end in `/v2` is a version `go get` refuses to resolve, so v0
# and v1 are what a path without a suffix may carry.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/sdk-release-lib.sh
. "$repo_root/scripts/sdk-release-lib.sh"
source_file="${SDK_GO_VERSION_FILE:-$repo_root/sdk/go/http.go}"
mod_file="${SDK_GO_MOD:-$repo_root/sdk/go/go.mod}"
arg="${1:?usage: sdk-go-release-check.sh <tag> | --print-tag}"

declared="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$source_file")"
if ! canonical_semver "$declared"; then
    echo "the package says '${declared}', which is not X.Y.Z or X.Y.Z-rc.N (alpha, beta, rc; no leading zeros)" >&2
    exit 1
fi

module="$(sed -n 's/^module //p' "$mod_file")"
major="${declared%%.*}"
case "$module" in
    */v[0-9]*) wanted="${module##*/v}" ;;
    *) wanted="" ;;
esac
if [ -n "$wanted" ]; then
    [ "$major" = "$wanted" ] || { echo "module $module wants major $wanted, the package says $declared" >&2; exit 1; }
elif [ "$major" -ge 2 ]; then
    echo "module $module has no /v$major suffix, so $declared cannot be resolved; v0 and v1 only" >&2
    exit 1
fi

if [ "$arg" = --print-tag ]; then
    echo "sdk/go/v$declared"
    exit 0
fi
if [ "$arg" != "sdk/go/v$declared" ]; then
    echo "tag $arg is not the tag of the package's version: sdk/go/v$declared" >&2
    exit 1
fi
echo "sdk-go $declared"
