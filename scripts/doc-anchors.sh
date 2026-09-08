#!/usr/bin/env bash
# Every anchor in the docs, checked against the heading it names (spec 026 #6).
#
# The documentation cross-references itself by heading — a hundred links of the
# form `[…](retention.md#what-outlives-what)` and `[…](#counting)` — and until
# now nothing read them: a heading renamed in one PR leaves a link that quietly
# scrolls nowhere, and spec 023's worker found one that had been broken for
# weeks.
#
# The rule is GitHub's own slug, stated rather than guessed: lower-case;
# punctuation other than hyphens and spaces removed; spaces to hyphens; a
# repeated slug gets `-1`, `-2`, … in the order the headings appear.
#
# Fenced code blocks are not read at all — neither for headings nor for links.
# `# comment` opens a shell block in half these documents (`datasets.md` alone
# has nine), and a naive heading grep takes every one of them for a heading it
# can then be linked to.
#
# Every broken anchor is reported, not just the first (spec 026 #13): a run
# that stops at one turns a documentation sweep into as many runs as there are
# breakages, and the fixture below asserts two in a single pass.
#
# Usage:
#   scripts/doc-anchors.sh              check docs/*.md, README.md and AGENTS.md
#   scripts/doc-anchors.sh --self-test  check the fixture and assert what it finds
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

FIXTURE="scripts/doc-anchors-fixture"

# The headings of one file as GitHub would slug them, in order: the order is
# what numbers a repeat.
slugs() {
	awk '
		function slugify(text,   out) {
			out = tolower(text)
			# Everything that is not a letter, a digit, an underscore, a
			# hyphen or a space goes. That is what drops the backticks of
			# a code span, and what leaves the two spaces around an em
			# dash as two hyphens — which is what GitHub produces too.
			gsub(/[^-_ a-z0-9]/, "", out)
			gsub(/ /, "-", out)
			return out
		}
		/^[ \t]*(```|~~~)/ { fence = !fence; next }
		fence { next }
		/^#{1,6}[ \t]/ {
			text = $0
			sub(/^#+[ \t]+/, "", text)
			sub(/[ \t]+#+[ \t]*$/, "", text)
			slug = slugify(text)
			seen[slug]++
			print (seen[slug] > 1 ? slug "-" (seen[slug] - 1) : slug)
		}
	' "$1"
}

# Every `](…#…)` of one file as "line<TAB>target". External links are somebody
# else's headings (Out of scope), and a bare `](file.md)` names no anchor.
links() {
	awk '
		/^[ \t]*(```|~~~)/ { fence = !fence; next }
		fence { next }
		{
			rest = $0
			# A code span is prose about a link, not a link: this very
			# file documents the shape `[…](file.md#anchor)`, and a
			# checker that read it would demand a file called file.md.
			gsub(/`[^`]*`/, "", rest)
			while ((i = index(rest, "](")) > 0) {
				rest = substr(rest, i + 2)
				j = index(rest, ")")
				if (j == 0) break
				target = substr(rest, 1, j - 1)
				rest = substr(rest, j + 1)
				if (index(target, "#") == 0) continue
				if (target ~ /^[A-Za-z][A-Za-z0-9+.-]*:/) continue
				print FNR "\t" target
			}
		}
	' "$1"
}

checked=0
broken=0

# Reports each broken anchor on stdout as `file:line: …`, and counts both what
# it read and what it could not resolve.
check() {
	local source line target file anchor path
	for source in "$@"; do
		while IFS="$(printf '\t')" read -r line target; do
			file="${target%%#*}"
			anchor="${target#*#}"
			# `](file.md#)` names no heading; it is a link to the top.
			[ -n "$anchor" ] || continue
			checked=$((checked + 1))
			if [ -z "$file" ]; then
				path="$source"
			else
				path="$(dirname "$source")/$file"
			fi
			if [ ! -f "$path" ]; then
				printf '%s:%s: no such file: %s\n' "$source" "$line" "$file"
				broken=$((broken + 1))
				continue
			fi
			if ! slugs "$path" | grep -qxF -- "$anchor"; then
				printf '%s:%s: broken anchor: %s\n' "$source" "$line" "$target"
				broken=$((broken + 1))
			fi
		done < <(links "$source")
	done
}

# The fixture holds the cases: a broken cross-file anchor, a same-file anchor
# whose only heading is a `#` line inside a shell block, a heading with
# backticks, a repeated heading, a link inside a code span, and the fence
# itself. Two of them must be reported and the rest must not, which is the whole
# contract in one run — and a checker that reads fences would resolve the second
# and report one.
self_test() {
	local got want
	got="$(check "$FIXTURE/guide.md" "$FIXTURE/other.md")"
	want="$FIXTURE/guide.md:12: broken anchor: other.md#the-missing-heading
$FIXTURE/guide.md:13: broken anchor: #running-the-server"
	if [ "$got" != "$want" ]; then
		printf 'doc-anchors: the fixture run reported\n%s\n\nand should have reported\n%s\n' \
			"$got" "$want" >&2
		exit 1
	fi
	printf 'doc-anchors: the fixture'"'"'s two broken anchors are found and its traps are not\n'
}

if [ "${1:-}" = "--self-test" ]; then
	self_test
	exit 0
fi

# Not in a command substitution: the counters below are `check`'s own, and a
# subshell would take them with it.
check docs/*.md README.md AGENTS.md >&2
if [ "$broken" -gt 0 ]; then
	printf 'doc-anchors: %d of %d anchors point at no heading\n' "$broken" "$checked" >&2
	exit 1
fi
printf 'doc-anchors: %d anchors, every one of them landing on a heading\n' "$checked"
