#!/usr/bin/env bash
# The tag and the package must agree about the version (spec 032 #11).
#
#   scripts/sdk-js-release-check.sh sdk-js/v0.2.0
#
# Its own script rather than a line in the workflow so that it can be run
# without a tag: `release-sdk-js.yml` is exercised by a dry run here, never by
# a publish.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tag="${1:?usage: sdk-js-release-check.sh <tag>}"

case "$tag" in
    sdk-js/v*) tagged="${tag#sdk-js/v}" ;;
    *) echo "tag $tag is not an sdk-js/vX.Y.Z tag" >&2; exit 1 ;;
esac
declared="$(node -p "require('$repo_root/sdk/js/package.json').version")"
if [ "$tagged" != "$declared" ]; then
    echo "tag says $tagged, the package says $declared" >&2
    exit 1
fi
echo "sdk-js $declared"
