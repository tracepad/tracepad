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
# #11), 14,000 for the eval screens (spec 015 #9), and now 16,000 (spec 023
# #11).
#
# 9,000 was design §8's 6-9k envelope for the *MVP* interface, and it held for
# five specs. Spec 015 amends the envelope rather than quietly overrunning it:
# the eval screens of spec 016 are six screens, an editor and a comparison —
# the argument §8 asked the next feature to make — and a payload surface that
# both reads and writes replaces 219 lines with more. The number is `main`'s
# 6,678 plus a measured estimate for 016 with room for one review cycle, and it
# was raised by the spec before the one that spends it, because a budget raised
# by the spec that spends it is no budget.
#
# 16,000 was the owner's decision of 2026-09-07, taken for the *known* set
# rather than for one spec: 14,000 was reached at 13,895 with two screens
# landed and nothing left to cut, and the design's own list still holds the
# annotation queue and the quality trends. Raising it once for that set is the
# honest move; raising it per spec would be exactly the drift the number exists
# to catch. It stays a warning either way (design §8.2): a signal to revise,
# not a rule to satisfy.
#
# 18,000 is the owner's decision of 2026-09-08 (spec 024 #17), and it is what
# the estimate above was measured against: the annotation queues came to ~1,750
# lines where ~1,250 was planned, and the saving spec 024 #14 expected from
# extracting `ScoreControl` was not there — the component boundary costs props
# and types, and its second consumer arrived in the same spec that made it. The
# quality trends are still on the list. The revision the warning asked for is a
# PR of its own: the traces and sessions listings are twins at ~400 lines each
# and have never been read side by side.
#
# 18,500 is the owner's decision of 2026-09-09 (spec 027 #9), and it is the
# first raise since 018 measured against a spec that came in on its estimate:
# the facet field, its chip, the facets client and the sessions panel's share
# were estimated at 350-450 lines and came to 436 (`main` 17,600 -> 18,036). A checkbox
# list *is* its lines — there is no cheaper shape for "these, not this" — and
# the 500 is `main` plus that measurement plus room for one review cycle, which
# is how every raise since spec 015 #9 has been justified. Spec 026 is why the
# arithmetic starts where it does: the revision the warning asked for came out
# neutral, so the duplication it removed paid for the header fix and the
# checker rather than for this.
#
# 20,000 is spec 028 #17 (2026-09-10): the accounts — the login rewrite, the
# setup and invite screens, the account menu, the Account tab, the Accounts
# table with its dialogs — estimated at 1,300-1,500 lines, on `main`'s 18,141.
#
# 20,500 is spec 029 #9 (2026-09-11): the project in the URL and the switcher.
# The route move is a move and counts the same; the switcher with its filter
# box, the two not-there screens, the redirect routes, the `href` and
# `switchTarget` helpers and the shared create-project dialog were estimated at
# 300-400 lines on `main`'s 19,481 and came to 531 (`main` 19,481 -> 20,012):
# the switcher is 166 on its own, and the seventy links that go through `href`
# each cost a line or two of wrapping.
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
BUDGET="${1:-18500}"

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

# Split in two by what a file *is*, not by what it is for: the test half is the
# test sources — unit tests beside what they test, the end-to-end specs, and the
# harness both need. The config that shapes the build stays with the
# application, `playwright.config.ts` beside `vite.config.ts`, because a tool's
# settings are read by whoever changes the tool and not by whoever reads a test.
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
