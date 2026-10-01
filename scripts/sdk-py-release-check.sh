#!/usr/bin/env bash
# The tag and the package must agree about the version (spec 017 #12, spec 020
# #28).
#
#   scripts/sdk-py-release-check.sh sdk-py/v0.2.0
#   scripts/sdk-py-release-check.sh sdk-py/v0.2.0-rc.1     # releases 0.2.0rc1
#
# One spelling of a version goes in a tag, semver's, whichever package it names
# (`v0.2.0-rc.1` for the server, the Node package and the Go module as well),
# and PyPI's own is derived from it: `-rc.1` is `rc1`, `-beta.2` is `b2`,
# `-alpha.3` is `a3`. A tag that is spelled any other way fails, so that there
# is exactly one tag that releases a given version.
#
# Its own script rather than a line in the workflow so that the gate can run
# it without a tag (scripts/sdk-release-check-test.sh).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tag="${1:?usage: sdk-py-release-check.sh <tag>}"
config="${SDK_PY_CONFIG:-$repo_root/sdk/python/src/tracepad/_config.py}"

case "$tag" in
    sdk-py/v*) tagged="${tag#sdk-py/v}" ;;
    *) echo "tag $tag is not an sdk-py/vX.Y.Z tag" >&2; exit 1 ;;
esac

core='[0-9]+\.[0-9]+\.[0-9]+'
if printf '%s\n' "$tagged" | grep -Eq "^$core$"; then
    expected="$tagged"
elif printf '%s\n' "$tagged" | grep -Eq "^$core-(alpha|beta|rc)\.[0-9]+$"; then
    expected="$(printf '%s\n' "$tagged" | sed -E 's/-alpha\./a/; s/-beta\./b/; s/-rc\./rc/')"
else
    echo "tag $tag is not sdk-py/vX.Y.Z or sdk-py/vX.Y.Z-rc.N (alpha, beta, rc)" >&2
    exit 1
fi

declared="$(sed -n 's/^VERSION = "\(.*\)"$/\1/p' "$config")"
if [ "$expected" != "$declared" ]; then
    echo "tag says $tagged, which is $expected on PyPI; the package says ${declared:-nothing}" >&2
    exit 1
fi
echo "sdk-py $declared"
