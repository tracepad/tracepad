#!/usr/bin/env bash
# The notes of a server release, cut from CHANGELOG.md (spec 020 #29).
# `release-server.yml`'s `check` job runs it before anything is published, and
# `release` puts what it printed on the GitHub release.
#
# Which section a tag takes:
#
#   vX.Y.Z                    `## [X.Y.Z]`, and nothing else: a stable release
#                             that the changelog does not describe is refused
#   vX.Y.Z-(alpha|beta|rc).N  `## [X.Y.Z-rc.N]` when there is one, otherwise
#                             `## [Unreleased]` — what the candidate previews
#
# A heading may carry a date after the brackets (`## [0.1.0] - 2026-10-20`).
# The section runs to the next `## ` heading; the link definitions at the foot
# of the file are left out. Links relative to the repository are made absolute
# at the tag, because a release page resolves them against itself. The notes
# end with a link to the full list of commits: since `--previous` when it is
# given, otherwise the tag's whole history.
#
# Usage:
#   scripts/release-notes.sh <tag> [--previous <tag>] [--changelog <file>] [--repo <owner/name>]
#   scripts/release-notes.sh --self-test
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# section <file> <name>: the lines under `## [<name>]`, leading blank lines
# left out (the trailing ones go with the command substitution that reads it);
# exit 1 when there is no such heading. A `## ` inside a fenced block does not
# end the section.
section() {
	awk -v want="$2" '
		BEGIN { found = 0; inside = 0; fence = 0 }
		/^```/ && inside { fence = !fence }
		!fence && /^## / {
			if (inside) exit
			heading = $0
			sub(/^## +/, "", heading)
			if (index(heading, "[" want "]") == 1) { found = 1; inside = 1; next }
		}
		inside && !fence && /^\[[^]]+\]: / { next }
		inside { lines[++n] = $0 }
		END {
			if (!found) exit 1
			first = 1; while (first <= n && lines[first] ~ /^[[:space:]]*$/) first++
			for (i = first; i <= n; i++) print lines[i]
		}
	' "$1"
}

# absolute <repo> <tag>: `](path)` and `](#anchor)` become links into the
# repository at the tag. A link with a scheme (`https:`, `mailto:`) is left.
absolute() {
	REPO="$1" TAG="$2" perl -pe '
		s{\]\((?![a-zA-Z][a-zA-Z0-9+.-]*:)([^)\s]+)\)}{
			my $t = $1;
			$t = "CHANGELOG.md$t" if $t =~ /^#/;
			$t =~ s{^\./}{};
			"](https://github.com/$ENV{REPO}/blob/$ENV{TAG}/$t)"
		}ge
	'
}

notes() {
	local tag="" previous="" changelog="" repo="${GITHUB_REPOSITORY:-tracepad/tracepad}"
	while [ $# -gt 0 ]; do
		case "$1" in
			--previous) previous="${2-}"; shift 2 ;;
			--changelog) changelog="${2-}"; shift 2 ;;
			--repo) repo="${2-}"; shift 2 ;;
			-*) echo "release-notes: unknown option $1" >&2; return 2 ;;
			*) tag="$1"; shift ;;
		esac
	done
	[ -n "$tag" ] || { echo "usage: $0 <tag> [--previous <tag>] [--changelog <file>] [--repo <owner/name>]" >&2; return 2; }
	changelog="${changelog:-$here/../CHANGELOG.md}"

	local kind version body name
	kind="$("$here/release-tag.sh" "$tag")" || return 1
	version="${tag#v}"

	if [ "$kind" = stable ]; then
		name="$version"
		if ! body="$(section "$changelog" "$version")"; then
			echo "release-notes: $changelog has no '## [$version]' section; a stable release is described before it is tagged (move what it ships out of [Unreleased])" >&2
			return 1
		fi
	elif body="$(section "$changelog" "$version")"; then
		name="$version"
	else
		name=Unreleased
		if ! body="$(section "$changelog" Unreleased)"; then
			echo "release-notes: $changelog has neither '## [$version]' nor '## [Unreleased]'" >&2
			return 1
		fi
	fi
	if [ -z "$body" ]; then
		echo "release-notes: the '## [$name]' section of $changelog is empty; nothing would say what $tag ships" >&2
		return 1
	fi

	printf '%s\n' "$body" | absolute "$repo" "$tag"
	echo
	if [ -n "$previous" ]; then
		echo "**Every commit since $previous:** https://github.com/$repo/compare/$previous...$tag"
	else
		echo "**Every commit:** https://github.com/$repo/commits/$tag"
	fi
}

self_test() {
	local work failures=0
	work="$(mktemp -d)"
	trap 'rm -rf "$work"' RETURN

	cat > "$work/CHANGELOG.md" <<-'EOF_LOG'
		# Changelog

		Intro, linking the [README](README.md#what-beta-means).

		## [Unreleased]

		### Added

		- Something new, see [the guide](docs/guide.md) and [above](#unreleased).

		## [0.2.0-rc.1] - 2026-11-01

		- The candidate's own line.

		## [0.1.0] - 2026-10-20

		### Added

		#### Server

		- The first thing, at <https://example.com> and [there](https://example.com/x).
		- Run `tracepad` as in [install](./docs/install.md).

		```text
		## not a heading inside a fence
		```

		### Fixed

		- The last line of 0.1.0.

		## [0.0.9]

		- The oldest line.

		[Unreleased]: https://github.com/tracepad/tracepad/compare/v0.1.0...HEAD
		[0.1.0]: https://github.com/tracepad/tracepad/releases/tag/v0.1.0
	EOF_LOG
	printf '# Changelog\n\n## [Unreleased]\n\n## [0.1.0]\n\n\n' > "$work/EMPTY.md"
	printf '## [0.1.0-rc.1]\n\n- x\n' > "$work/RC.md"
	printf '# Changelog\n' > "$work/BARE.md"

	# expect <name> <file of the expected output> <arguments…>: must print it exactly.
	expect() {
		local name="$1" want="$2"; shift 2
		if ! notes "$@" > "$work/got" 2> "$work/err"; then
			echo "FAIL: $name: refused: $(cat "$work/err")" >&2
			failures=$((failures + 1))
		elif ! diff -u "$want" "$work/got" > "$work/diff"; then
			echo "FAIL: $name:" >&2
			cat "$work/diff" >&2
			failures=$((failures + 1))
		fi
	}
	# refused <name> <words the refusal must contain> <arguments…>
	refused() {
		local name="$1" says="$2"; shift 2
		if notes "$@" > /dev/null 2> "$work/err"; then
			echo "FAIL: $name: should be refused" >&2
			failures=$((failures + 1))
		elif ! grep -qF -- "$says" "$work/err"; then
			echo "FAIL: $name: refusal does not say '$says': $(cat "$work/err")" >&2
			failures=$((failures + 1))
		fi
	}

	local log="$work/CHANGELOG.md" r=(--repo o/r)

	# A stable tag takes its own section, to the next heading: the fence's
	# `## ` does not end it, relative links point into the tag, and the
	# footer counts from the previous release.
	cat > "$work/want" <<-'EOF_WANT'
		### Added

		#### Server

		- The first thing, at <https://example.com> and [there](https://example.com/x).
		- Run `tracepad` as in [install](https://github.com/o/r/blob/v0.1.0/docs/install.md).

		```text
		## not a heading inside a fence
		```

		### Fixed

		- The last line of 0.1.0.

		**Every commit since v0.0.9:** https://github.com/o/r/compare/v0.0.9...v0.1.0
	EOF_WANT
	expect "stable" "$work/want" v0.1.0 --changelog "$log" --previous v0.0.9 "${r[@]}"

	# A candidate with a section of its own takes it; the first tag has no
	# previous one and links the whole history.
	cat > "$work/want" <<-'EOF_WANT'
		- The candidate's own line.

		**Every commit:** https://github.com/o/r/commits/v0.2.0-rc.1
	EOF_WANT
	expect "candidate with a section" "$work/want" v0.2.0-rc.1 --changelog "$log" "${r[@]}"

	# A candidate without one takes [Unreleased]; an anchor alone is the
	# changelog's own.
	cat > "$work/want" <<-'EOF_WANT'
		### Added

		- Something new, see [the guide](https://github.com/o/r/blob/v0.2.0-rc.2/docs/guide.md) and [above](https://github.com/o/r/blob/v0.2.0-rc.2/CHANGELOG.md#unreleased).

		**Every commit since v0.1.0:** https://github.com/o/r/compare/v0.1.0...v0.2.0-rc.2
	EOF_WANT
	expect "candidate from Unreleased" "$work/want" v0.2.0-rc.2 --changelog "$log" --previous v0.1.0 "${r[@]}"

	# The link definitions at the foot belong to no section.
	cat > "$work/want" <<-'EOF_WANT'
		- The oldest line.

		**Every commit:** https://github.com/o/r/commits/v0.0.9
	EOF_WANT
	expect "the last section" "$work/want" v0.0.9 --changelog "$log" "${r[@]}"

	refused "stable with no section" "no '## [0.3.0]' section" v0.3.0 --changelog "$log"
	refused "stable not matched by prefix" "no '## [0.1.0]' section" v0.1.0 --changelog "$work/RC.md"
	refused "empty stable section" "section of" v0.1.0 --changelog "$work/EMPTY.md"
	refused "empty Unreleased" "section of" v0.2.0-rc.1 --changelog "$work/EMPTY.md"
	refused "candidate with nothing" "neither" v0.2.0-rc.1 --changelog "$work/BARE.md"
	refused "not a release tag" "neither vX.Y.Z" v0.1.0rc1 --changelog "$log"

	# The changelog this repository ships keeps the heading candidates read.
	if ! section "$here/../CHANGELOG.md" Unreleased > /dev/null; then
		echo "FAIL: CHANGELOG.md has no '## [Unreleased]' heading" >&2
		failures=$((failures + 1))
	fi

	if [ "$failures" -ne 0 ]; then
		echo "release-notes: $failures case(s) failed" >&2
		return 1
	fi
	echo "release-notes: every case answers as expected"
}

case "${1:-}" in
	--self-test) self_test ;;
	*) notes "$@" ;;
esac
