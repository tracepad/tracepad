#!/usr/bin/env bash
# The tag and the package must agree about the version (spec 033 #1, spec 020
# #28).
#
#   scripts/sdk-go-release-check.sh sdk/go/v0.2.0
#   scripts/sdk-go-release-check.sh sdk/go/v0.2.0-rc.1
#
# The Go module is released by its tag alone, and the proxy serves whatever the
# tag holds for good: `Version` in `sdk/go/http.go`, which is sent as the
# User-Agent, is the one place a wrong number can ship. Run it before the tag is
# pushed (`release-sdk-go.yml` runs it after, to be told at once).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tag="${1:?usage: sdk-go-release-check.sh <tag>}"
source_file="${SDK_GO_VERSION_FILE:-$repo_root/sdk/go/http.go}"

case "$tag" in
    sdk/go/v*) tagged="${tag#sdk/go/v}" ;;
    *) echo "tag $tag is not an sdk/go/vX.Y.Z tag" >&2; exit 1 ;;
esac
if ! printf '%s\n' "$tagged" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)\.[0-9]+)?$'; then
    echo "tag $tag is not sdk/go/vX.Y.Z or sdk/go/vX.Y.Z-rc.N (alpha, beta, rc)" >&2
    exit 1
fi

declared="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$source_file")"
if [ "$tagged" != "$declared" ]; then
    echo "tag says $tagged, the package says ${declared:-nothing}" >&2
    exit 1
fi
echo "sdk-go $declared"
