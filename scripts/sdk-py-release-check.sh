#!/usr/bin/env bash
# The tag and the package must agree about the version (spec 017 #12, spec 020
# #28).
#
#   scripts/sdk-py-release-check.sh sdk-py/v0.2.0
#   scripts/sdk-py-release-check.sh sdk-py/v0.2.0-rc.1     # releases 0.2.0rc1
#   scripts/sdk-py-release-check.sh --print-tag            # the tag this tree is released by
#
# One spelling of a version goes in a tag, semver's, whichever package it names
# (`v0.2.0-rc.1` for the server, the Node package and the Go module as well),
# and PyPI's own is what `VERSION` says: `rc1` is `-rc.1`, `b2` is `-beta.2`,
# `a3` is `-alpha.3`. The tag is derived from the package, in that one
# direction, and compared as a string, so that there is exactly one tag that
# releases a given version.
#
# Its own script rather than a line in the workflow so that the gate can run
# it without a tag (scripts/sdk-release-check-test.sh).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/sdk-release-lib.sh
. "$repo_root/scripts/sdk-release-lib.sh"
config="${SDK_PY_CONFIG:-$repo_root/sdk/python/src/tracepad/_config.py}"
arg="${1:?usage: sdk-py-release-check.sh <tag> | --print-tag}"

declared="$(sed -n 's/^VERSION = "\(.*\)"$/\1/p' "$config")"
n='(0|[1-9][0-9]*)'
if ! printf '%s\n' "$declared" | grep -Eq "^$n\\.$n\\.$n((a|b|rc)$n)?$"; then
    echo "the package says '${declared}', which is not X.Y.Z, X.Y.ZaN, X.Y.ZbN or X.Y.ZrcN" >&2
    exit 1
fi
expected="$(printf '%s\n' "$declared" | sed -E 's/a([0-9]+)$/-alpha.\1/; s/b([0-9]+)$/-beta.\1/; s/rc([0-9]+)$/-rc.\1/')"
canonical_semver "$expected" || { echo "$declared does not derive a semver tag (got $expected)" >&2; exit 1; }

if [ "$arg" = --print-tag ]; then
    echo "sdk-py/v$expected"
    exit 0
fi
if [ "$arg" != "sdk-py/v$expected" ]; then
    echo "tag $arg is not the tag of the package's version: $declared on PyPI is sdk-py/v$expected" >&2
    exit 1
fi
echo "sdk-py $declared"
