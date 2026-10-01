#!/usr/bin/env bash
# The tag and the package must agree about the version (spec 032 #11, spec 020
# #28).
#
#   scripts/sdk-js-release-check.sh sdk-js/v0.2.0
#   scripts/sdk-js-release-check.sh --print-tag     # the tag this tree is released by
#
# Its own script rather than a line in the workflow so that it can be run
# without a tag: `release-sdk-js.yml` is exercised by a dry run here, never by
# a publish. The version is held to the spelling every tag has (a pre-release
# is `-rc.N`, not `-rc1`), so that one version has one tag.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/sdk-release-lib.sh
. "$repo_root/scripts/sdk-release-lib.sh"
package_json="${SDK_JS_PACKAGE_JSON:-$repo_root/sdk/js/package.json}"
arg="${1:?usage: sdk-js-release-check.sh <tag> | --print-tag}"

declared="$(node -p 'require(process.argv[1]).version' "$package_json")"
if ! canonical_semver "$declared"; then
    echo "the package says '${declared}', which is not X.Y.Z or X.Y.Z-rc.N (alpha, beta, rc; no leading zeros)" >&2
    exit 1
fi

if [ "$arg" = --print-tag ]; then
    echo "sdk-js/v$declared"
    exit 0
fi
if [ "$arg" != "sdk-js/v$declared" ]; then
    echo "tag $arg is not the tag of the package's version: sdk-js/v$declared" >&2
    exit 1
fi
echo "sdk-js $declared"
