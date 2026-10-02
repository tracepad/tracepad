#!/usr/bin/env bash
# The notes of a server release, cut from CHANGELOG.md (spec 020 #29).
# `release-server.yml`'s `check` job runs it before anything is published, and
# `release` puts what it printed on the GitHub release; `ci.yml`'s
# `release-rehearsal` runs that hand-over on every pull request.
#
# Which section a tag takes:
#
#   vX.Y.Z                    `## [X.Y.Z]`, and nothing else: a stable release
#                             that the changelog does not describe is refused
#   vX.Y.Z-(alpha|beta|rc).N  `## [X.Y.Z-rc.N]` when there is one, otherwise
#                             `## [Unreleased]` — what the candidate previews
#
# A heading may carry a date after the brackets (`## [0.1.0] - 2026-10-20`).
# The section runs to the next `## ` heading outside a fenced block (``` or
# ~~~, indented or not). A release page resolves a relative link against
# itself, so links into the repository are made absolute at the tag: a page to
# `blob/<tag>/…`, an image to `raw/<tag>/…`, an anchor to the changelog's own.
# A link with a scheme or a protocol-relative one (`//host/…`) is left alone,
# and so is anything inside code. The link definitions at the foot of the file
# belong to no section; those the section uses are carried into the notes. The
# notes end with a link to the commits: since `--previous` when it is given,
# otherwise the tag's whole history.
#
# Bash and Perl 5, nothing else: no awk, whose dialects (mawk on Ubuntu, BWK
# awk on macOS) disagree on exactly the bracket expressions this needs.
# The self-test passes under bash 3.2 (macOS) and 5.2, with Perl 5.34.
#
# Usage:
#   scripts/release-notes.sh <tag> [--previous <tag>] [--changelog <file>] [--repo <owner/name>]
#   scripts/release-notes.sh --self-test
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The section of a changelog, with its links made absolute. Arguments: file,
# section name, repository, tag. Prints the section without the blank lines at
# either end, then the definitions it uses; exits 3 when there is no such
# heading.
read -r -d '' CUT <<'PERL' || true
use strict;
use warnings;

my ($file, $want, $repo, $tag) = @ARGV;
open my $fh, '<', $file or die "release-notes: cannot read $file: $!\n";
chomp(my @lines = <$fh>);
close $fh;

# A fence opens with three or more backticks or tildes, after any indentation
# (a block inside a list item is indented), and closes with a run of the same
# character at least as long, on a line of its own.
my ($fc, $flen);
sub fenced {
	my ($l) = @_;
	if (defined $fc) {
		undef $fc if $l =~ /^\s*([`~]{3,})\s*$/ && substr($1, 0, 1) eq $fc && length($1) >= $flen;
		return 1;
	}
	if ($l =~ /^\s*(`{3,}|~{3,})/) {
		($fc, $flen) = (substr($1, 0, 1), length $1);
		return 1;
	}
	return 0;
}

my (%defs, @body, $found, $inside);
for my $l (@lines) {
	if (fenced($l)) {
		push @body, [$l, 1] if $inside;
		next;
	}
	if ($l =~ /^##\s/) {
		$inside = !$found && $l =~ /^##\s+\[\Q$want\E\]/;
		$found ||= $inside;
		next;
	}
	if ($l =~ /^ {0,3}\[([^\]]+)\]:\s*(\S+)(.*)$/) {
		$defs{lc $1} //= [$1, $2, $3];
		next;
	}
	push @body, [$l, 0] if $inside;
}
exit 3 unless $found;
shift @body while @body && $body[0][0] =~ /^\s*$/;
pop @body while @body && $body[-1][0] =~ /^\s*$/;

sub target {
	my ($t, $image) = @_;
	my $angle = $t =~ s/^<(.*)>$/$1/;
	unless ($t =~ m{^[A-Za-z][A-Za-z0-9+.-]*:} || $t =~ m{^//}) {
		$t = "CHANGELOG.md$t" if $t =~ /^#/;
		$t =~ s{^(?:\./|/)+}{};
		$t = "https://github.com/$repo/" . ($image ? 'raw' : 'blob') . "/$tag/$t";
	}
	return $angle ? "<$t>" : $t;
}

my %used;
my $text = qr/(?:[^\[\]]|\[[^\]]*\])*/;
my $title = qr/\s+(?:"[^"]*"|'[^']*'|\([^)]*\))/;
sub prose {
	my ($s) = @_;
	# A link's text is prose too: a badge is an image inside a link.
	$s =~ s{(!?)\[($text)\]\(\s*(<[^>]*>|[^\s)]+)($title)?\s*\)}{
		my ($bang, $inner, $t, $ti) = ($1, $2, $3, $4 // "");
		"$bang\[" . prose($inner) . "](" . target($t, $bang eq "!") . "$ti)"
	}ge;
	$used{lc($2 eq "" ? $1 : $2)} = 1 while $s =~ /\[([^\]]+)\]\[([^\]]*)\]/g;
	$used{lc $1} = 1 while $s =~ /\[([^\]]+)\](?![\[(])/g;
	return $s;
}

for my $line (@body) {
	my ($l, $code) = @$line;
	# Code spans are left as written: a run of backticks to the next run of
	# the same length.
	$l =~ s{(`+)(.*?)\1|([^`]+)}{defined $3 ? prose($3) : "$1$2$1"}ge unless $code;
	print "$l\n";
}

my @refs = grep { $used{$_} } sort keys %defs;
print "\n" if @refs;
printf "[%s]: %s%s\n", $defs{$_}[0], target($defs{$_}[1], 0), $defs{$_}[2] for @refs;
PERL

# section <file> <name> <repo> <tag>
section() {
	perl -e "$CUT" "$@"
}

notes() {
	local tag="" previous="" changelog="" repo="${GITHUB_REPOSITORY:-tracepad/tracepad}"
	while [ $# -gt 0 ]; do
		case "$1" in
			--previous | --changelog | --repo)
				if [ $# -lt 2 ] || [ -z "$2" ]; then
					echo "release-notes: $1 needs a value" >&2
					return 2
				fi
				case "$1" in
					--previous) previous="$2" ;;
					--changelog) changelog="$2" ;;
					--repo) repo="$2" ;;
				esac
				shift 2
				;;
			-*) echo "release-notes: unknown option $1" >&2; return 2 ;;
			*)
				if [ -n "$tag" ]; then
					echo "release-notes: one tag at a time; got $tag and $1" >&2
					return 2
				fi
				tag="$1"; shift
				;;
		esac
	done
	[ -n "$tag" ] || { echo "usage: $0 <tag> [--previous <tag>] [--changelog <file>] [--repo <owner/name>]" >&2; return 2; }
	changelog="${changelog:-$here/../CHANGELOG.md}"
	[ -r "$changelog" ] || { echo "release-notes: cannot read $changelog" >&2; return 1; }

	local kind version body name status
	kind="$("$here/release-tag.sh" "$tag")" || return 1
	version="${tag#v}"

	name="$version"
	status=0
	body="$(section "$changelog" "$version" "$repo" "$tag")" || status=$?
	if [ "$status" -eq 3 ] && [ "$kind" = prerelease ]; then
		name=Unreleased
		status=0
		body="$(section "$changelog" Unreleased "$repo" "$tag")" || status=$?
		if [ "$status" -eq 3 ]; then
			echo "release-notes: $changelog has neither '## [$version]' nor '## [Unreleased]'" >&2
			return 1
		fi
	elif [ "$status" -eq 3 ]; then
		echo "release-notes: $changelog has no '## [$version]' section; a stable release is described before it is tagged (move what it ships out of [Unreleased])" >&2
		return 1
	fi
	[ "$status" -eq 0 ] || return "$status"
	if [ -z "$body" ]; then
		echo "release-notes: the '## [$name]' section of $changelog is empty; nothing would say what $tag ships" >&2
		return 1
	fi

	printf '%s\n\n' "$body"
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
		- Run `tracepad` as in [install](./docs/install.md), [root](/docs/a.md "The A page")
		  and [elsewhere](//cdn.example.com/p), but not `[code](docs/code.md)`.
		- ![The mark](docs/assets/mark.png) and [![a badge](docs/b.svg)](docs/badge.md)
		- See [the guide][guide], [the README] and [GitHub][].

		```text
		## not a heading inside a fence
		[not](rewritten.md)
		```

		~~~
		## nor inside a tilde fence
		~~~

		- An item with a block:

		  ````md
		  ## nor inside an indented one, where [this](stays.md)
		  [and this]: is no definition
		  ```
		  ## nor after a shorter run of backticks
		  ````

		### Fixed

		- The last line of 0.1.0.

		## [0.0.9]

		- The oldest line.

		[Unreleased]: https://github.com/tracepad/tracepad/compare/v0.1.0...HEAD
		[0.1.0]: https://github.com/tracepad/tracepad/releases/tag/v0.1.0
		[guide]: docs/guide.md "The guide"
		[the readme]: README.md
		[GitHub]: https://github.com
		[unused]: docs/unused.md
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
	# refused <name> <exit status> <words the refusal must contain> <arguments…>
	refused() {
		local name="$1" code="$2" says="$3" got=0; shift 3
		notes "$@" > /dev/null 2> "$work/err" || got=$?
		if [ "$got" -ne "$code" ]; then
			echo "FAIL: $name: exit $got, want $code: $(cat "$work/err")" >&2
			failures=$((failures + 1))
		elif ! grep -qF -- "$says" "$work/err"; then
			echo "FAIL: $name: refusal does not say '$says': $(cat "$work/err")" >&2
			failures=$((failures + 1))
		fi
	}

	local log="$work/CHANGELOG.md" r=(--repo o/r)

	# A stable tag takes its own section, to the next heading: no fence's
	# `## ` ends it, links point into the tag — images to the raw file — but
	# not those in code or with a host, the definitions it uses follow it, and
	# the footer counts from the previous release.
	cat > "$work/want" <<-'EOF_WANT'
		### Added

		#### Server

		- The first thing, at <https://example.com> and [there](https://example.com/x).
		- Run `tracepad` as in [install](https://github.com/o/r/blob/v0.1.0/docs/install.md), [root](https://github.com/o/r/blob/v0.1.0/docs/a.md "The A page")
		  and [elsewhere](//cdn.example.com/p), but not `[code](docs/code.md)`.
		- ![The mark](https://github.com/o/r/raw/v0.1.0/docs/assets/mark.png) and [![a badge](https://github.com/o/r/raw/v0.1.0/docs/b.svg)](https://github.com/o/r/blob/v0.1.0/docs/badge.md)
		- See [the guide][guide], [the README] and [GitHub][].

		```text
		## not a heading inside a fence
		[not](rewritten.md)
		```

		~~~
		## nor inside a tilde fence
		~~~

		- An item with a block:

		  ````md
		  ## nor inside an indented one, where [this](stays.md)
		  [and this]: is no definition
		  ```
		  ## nor after a shorter run of backticks
		  ````

		### Fixed

		- The last line of 0.1.0.

		[GitHub]: https://github.com
		[guide]: https://github.com/o/r/blob/v0.1.0/docs/guide.md "The guide"
		[the readme]: https://github.com/o/r/blob/v0.1.0/README.md

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

	refused "stable with no section" 1 "no '## [0.3.0]' section" v0.3.0 --changelog "$log"
	refused "stable not matched by prefix" 1 "no '## [0.1.0]' section" v0.1.0 --changelog "$work/RC.md"
	refused "empty stable section" 1 "section of" v0.1.0 --changelog "$work/EMPTY.md"
	refused "empty Unreleased" 1 "section of" v0.2.0-rc.1 --changelog "$work/EMPTY.md"
	refused "candidate with nothing" 1 "neither" v0.2.0-rc.1 --changelog "$work/BARE.md"
	refused "no such changelog" 1 "cannot read" v0.1.0 --changelog "$work/missing.md"
	refused "not a release tag" 1 "neither vX.Y.Z" v0.1.0rc1 --changelog "$log"
	refused "--previous last" 2 "--previous needs a value" v0.1.0 --changelog "$log" --previous
	refused "--changelog last" 2 "--changelog needs a value" v0.1.0 --changelog
	refused "--repo empty" 2 "--repo needs a value" v0.1.0 --repo ""
	refused "two tags" 2 "one tag at a time" v0.1.0 v0.2.0
	refused "no tag" 2 "usage" --changelog "$log"

	# The changelog this repository ships keeps the heading candidates read.
	if ! section "$here/../CHANGELOG.md" Unreleased o/r v0.0.0 > /dev/null; then
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
