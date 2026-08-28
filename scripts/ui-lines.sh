#!/usr/bin/env bash
# The web interface's line budget (design §8, spec 006 #11).
#
# A warning, never a failure: the number is a signal to revise, not a rule to
# satisfy, and a UI that must ship one honest screen over the line should ship
# it and be argued about in review. Generated files do not count — the point of
# the budget is code somebody has to read and maintain.
#
# The default is the budget of the newest spec that moved it: 3,813 lines after
# spec 006 plus the ~3,500 spec 007 allows itself (spec 007 #13). Design §8's
# 6-9k envelope for the finished MVP interface is what both numbers live under.
#
# Usage: scripts/ui-lines.sh [budget]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUDGET="${1:-7300}"

cd "$ROOT"

# Hand-written sources only: the app, its tests, its end-to-end suite and the
# handful of config files that shape the build.
files() {
	# grep exits 1 on an empty tree, which is not an error here.
	git ls-files 'ui/src' 'ui/tests' 'ui/*.ts' 'ui/*.js' 'ui/*.json' |
		grep -v -e '^ui/src/lib/api/schema.d.ts$' -e '^ui/package-lock.json$' || true
}

count=$(files | wc -l | tr -d ' ')
if [ "$count" -eq 0 ]; then
	echo "ui-lines: no tracked interface sources yet"
	exit 0
fi

# Blank lines are formatting, not code.
total=$(files | tr '\n' '\0' | xargs -0 grep -chv '^[[:space:]]*$' | awk '{ sum += $1 } END { print sum + 0 }')

printf 'ui-lines: %d lines across %d files (budget %d)\n' "$total" "$count" "$BUDGET"

if [ "$total" -gt "$BUDGET" ]; then
	printf '::warning title=UI line budget::the web interface is %d lines over its ~%d line budget (design §8)\n' \
		"$((total - BUDGET))" "$BUDGET"
fi
