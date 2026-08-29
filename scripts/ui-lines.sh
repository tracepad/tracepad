#!/usr/bin/env bash
# The web interface's line budget (design §8, spec 006 #11).
#
# A warning, never a failure: the number is a signal to revise, not a rule to
# satisfy, and a UI that must ship one honest screen over the line should ship
# it and be argued about in review. Generated files do not count — the point of
# the budget is code somebody has to read and maintain.
#
# The default is the budget of the newest spec that moved it: 7,300 after spec
# 007 (#13), 8,300 for the peek panel (spec 008 #14), then 9,000 for listing
# pagination and the regression tests five review rounds asked of it (spec 009
# #11). Design §8's 6-9k envelope for the finished MVP interface is what every
# one of those numbers lives under, and 9,000 *is* its ceiling: no spec raises
# it, and the number does not move again.
#
# What it counts, though, has: the budget covers the application, and the tests
# are reported beside it under no ceiling at all (design §8, amended; spec 010
# #7). Spec 010 measured what a budget over both actually buys — extracting the
# listing three screens shared came out line-neutral, and the ceiling was
# crossed by the tests that hold its invariants. A number that charges for
# coverage argues for deleting tests, which is the opposite of what it exists
# to protect.
#
# Usage: scripts/ui-lines.sh [budget]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUDGET="${1:-9000}"

cd "$ROOT"

# Hand-written sources only: the app, its tests, its end-to-end suite and the
# handful of config files that shape the build. By extension rather than by
# exclusion, because the tree also carries assets nobody reads: the two web
# fonts under `ui/src/lib/fonts` were 319 "lines" of this count from the day it
# was written, and every number the budget has ever been set against stood on
# top of them (spec 010 #7).
sources() {
	# grep exits 1 on an empty tree, which is not an error here.
	git ls-files 'ui/src' 'ui/tests' 'ui/*.ts' 'ui/*.js' 'ui/*.json' |
		grep -E '\.(svelte|ts|js|json|css|html)$' |
		grep -v -e '^ui/src/lib/api/schema.d.ts$' -e '^ui/package-lock.json$' || true
}

# Split in two: the application, and the tests over it — unit tests beside what
# they test, the Playwright suite, and the harness both need.
app() { sources | grep -v -e '\.test\.ts$' -e '^ui/tests/' -e '^ui/src/tests/' || true; }
tests() { sources | grep -e '\.test\.ts$' -e '^ui/tests/' -e '^ui/src/tests/' || true; }

files() {
	if [ -z "$1" ]; then echo 0; else printf '%s\n' "$1" | wc -l | tr -d ' '; fi
}

# Blank lines are formatting, not code.
lines() {
	if [ -z "$1" ]; then
		echo 0
	else
		printf '%s\n' "$1" | tr '\n' '\0' | xargs -0 grep -chv '^[[:space:]]*$' |
			awk '{ sum += $1 } END { print sum + 0 }'
	fi
}

APP="$(app)"
TEST="$(tests)"
if [ -z "$APP" ] && [ -z "$TEST" ]; then
	echo "ui-lines: no tracked interface sources yet"
	exit 0
fi

total=$(lines "$APP")
covered=$(lines "$TEST")
printf 'ui-lines: %d app lines across %d files (budget %d); %d test lines across %d files (no budget)\n' \
	"$total" "$(files "$APP")" "$BUDGET" "$covered" "$(files "$TEST")"

if [ "$total" -gt "$BUDGET" ]; then
	printf '::warning title=UI line budget::the web interface is %d lines over its ~%d line budget (design §8)\n' \
		"$((total - BUDGET))" "$BUDGET"
fi
