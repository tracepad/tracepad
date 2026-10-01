#!/usr/bin/env bash
# What a Homebrew formula GoReleaser wrote must be before anything may commit
# it to the tap (spec 020 #27): valid Ruby, the class Homebrew looks for, and
# four downloads — macOS and Linux, Intel and ARM — each carrying the checksum
# `checksums.txt` has for the archive the URL names. A formula that fails any of
# these is what `brew install` would meet first, on a user's machine.
#
# Run by the `release-rehearsal` CI job over a snapshot build, which is how a
# change to `.goreleaser.yaml` or a bump of the pinned GoReleaser learns that
# the formula no longer comes out, before a tag does.
#
# Usage:
#   scripts/formula-check.sh <tracepad.rb> <checksums.txt>
set -euo pipefail

[ "$#" -eq 2 ] || { echo "usage: $0 <formula.rb> <checksums.txt>" >&2; exit 2; }
formula="$1"
sums="$2"

fail() { echo "formula-check: $*" >&2; exit 1; }

[ -s "$formula" ] || fail "$formula is missing or empty"
[ -s "$sums" ] || fail "$sums is missing or empty"

ruby -c "$formula" > /dev/null || fail "$formula is not valid Ruby"
grep -q '^class Tracepad < Formula$' "$formula" || fail "no 'class Tracepad < Formula'"
grep -qE '^  version "[^"]+"$' "$formula" || fail "no version line"

# Each `url` is followed by its `sha256`; the archive's name is the URL's last
# segment. awk pairs them, the loop looks each pair up in the checksum file.
pairs="$(awk '
	/^[[:space:]]+url "/ { n = $2; gsub(/"/, "", n); sub(/.*\//, "", n); next }
	/^[[:space:]]+sha256 "/ { s = $2; gsub(/"/, "", s); print s, n }
' "$formula")"

count=0
while read -r sha name; do
	[ -n "$sha" ] || continue
	count=$((count + 1))
	grep -qxF "$sha  $name" "$sums" || fail "$name: sha256 $sha is not in $sums"
done <<< "$pairs"
[ "$count" -eq 4 ] || fail "expected 4 downloads (macOS and Linux, amd64 and arm64), found $count"

for part in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
	grep -q "_${part}\.tar\.gz\"" "$formula" || fail "no download for $part"
done
echo "formula-check: $formula is well-formed, $count downloads match $sums"
