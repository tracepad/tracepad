#!/usr/bin/env bash
# Which kind of release a server tag names (spec 020 #27): `stable`, `prerelease`,
# or a refusal. `release-server.yml`'s `check` job runs it first, and what it
# prints decides which image tags and which Homebrew formula the tag may move.
#
# Two shapes are accepted and nothing else:
#
#   vX.Y.Z                         stable
#   vX.Y.Z-(alpha|beta|rc).N       pre-release
#
# X, Y, Z and N are numbers without leading zeros. A glob cannot say this, and
# the one this replaces — `[0-9]*.[0-9]*.[0-9]*` — let `v0.1.0rc1`, `v0.1.0-`
# and `v1.0.0+build` through as *stable* releases, each of which then moved
# `latest` and `X.Y`. Anything that is not one of the two shapes is a typo until
# someone adds a third on purpose.
#
# Usage:
#   scripts/release-tag.sh <tag>   print the kind; exit 1 and say why if it is neither
#   scripts/release-tag.sh --self-test
set -euo pipefail

NUM='(0|[1-9][0-9]*)'
STABLE="^v$NUM\\.$NUM\\.$NUM\$"
PRERELEASE="^v$NUM\\.$NUM\\.$NUM-(alpha|beta|rc)\\.$NUM\$"

kind() {
	local tag="$1"
	if [[ $tag =~ $STABLE ]]; then
		echo stable
	elif [[ $tag =~ $PRERELEASE ]]; then
		echo prerelease
	else
		echo "tag '$tag' is neither vX.Y.Z nor vX.Y.Z-(alpha|beta|rc).N; nothing is published for it" >&2
		return 1
	fi
}

self_test() {
	local failures=0 tag want got
	# tag, then what it must print (`refused` for an exit of 1 with no output).
	while read -r tag want; do
		got="$(kind "$tag" 2>/dev/null)" || got=refused
		if [ "$got" != "$want" ]; then
			echo "FAIL: '$tag' is $got, want $want" >&2
			failures=$((failures + 1))
		fi
	done <<-'EOF_CASES'
		v0.1.0 stable
		v1.0.0 stable
		v10.20.30 stable
		v0.1.0-rc.1 prerelease
		v0.1.0-alpha.1 prerelease
		v0.1.0-beta.12 prerelease
		v0.1.0rc1 refused
		v0.1.0- refused
		v0.1.0-rc refused
		v0.1.0-rc. refused
		v0.1.0-rc.1.2 refused
		v0.1.0-rc1 refused
		v0.1.0-RC.1 refused
		v0.1.0-dev.1 refused
		v1.0.0+build refused
		v1.0.0+build.5 refused
		v1.0.0-rc.1+build refused
		v1.0 refused
		v1 refused
		v1.0.0.0 refused
		v01.0.0 refused
		v1.00.0 refused
		v1.0.0-rc.01 refused
		1.0.0 refused
		V1.0.0 refused
		vx.y.z refused
		v1.0.0-1 refused
		v1.0.0.rc.1 refused
		sdk/go/v0.1.0 refused
		sdk-py/v0.1.0 refused
		main refused
	EOF_CASES
	# An empty tag cannot be a line of the table above.
	got="$(kind '' 2>/dev/null)" || got=refused
	[ "$got" = refused ] || { echo "FAIL: the empty tag is $got" >&2; failures=$((failures + 1)); }
	if [ "$failures" -ne 0 ]; then
		echo "release-tag: $failures case(s) failed" >&2
		return 1
	fi
	echo "release-tag: every case answers as expected"
}

case "${1:-}" in
	--self-test) self_test ;;
	'') echo "usage: $0 <tag> | --self-test" >&2; exit 2 ;;
	*) kind "$1" ;;
esac
