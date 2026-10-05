#!/usr/bin/env bash
# The server tag the release rehearsal builds under (spec 020 #34): the nearest
# `v<version>` tag reachable from HEAD, printed.
#
# GoReleaser told nothing picks a tag by itself, and on a commit that carries a
# release candidate's server tag and its SDK's (`v0.1.0-rc.1` and
# `sdk-js/v0.1.0-rc.1`) it may pick the SDK's: a version with a slash in it,
# archives in a directory of their own, and a formula `formula-check.sh` cannot
# match to its checksums. So the rehearsal names the tag, as the release does
# with the one that triggered it. `git describe` rather than a sort of the tags:
# a sort ranks a candidate above its own release (`v0.1.0-rc.2` over `v0.1.0`),
# and says nothing of whether HEAD contains the tag at all; the nearest tag is
# the version a snapshot of this commit comes after.
#
# Fails, saying so, when there is none — an empty GORELEASER_CURRENT_TAG counts
# as unset, which is the pick this script exists to prevent.
set -euo pipefail

if ! tag="$(git describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null)"; then
    echo "rehearsal-tag: no tag named v<version> is reachable from HEAD, so there is no server tag to rehearse under; fetch the tags (fetch-depth: 0)" >&2
    exit 1
fi
echo "$tag"
