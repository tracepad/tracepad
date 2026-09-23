#!/usr/bin/env bash
# The Go package's line budget (spec 033 #11).
#
# A warning, never a failure — the same instrument, and the same argument, as
# `sdk-lines.sh` and `ui-lines.sh`: the number is a signal to revise, not a
# rule to satisfy. 1,600 lines covered the package (spec 033 #11) and 1,900
# cover it with the eval harness (spec 033 #17), because they are one module
# and a budget per spec would be a budget per PR; spec 036 #8 raised it to
# 2,100 for trace deletion, spec 040 #13 to 2,300 for `tracepadtest`.
#
# What it counts is the application — `tracepadtest` and the hook it opens
# under internal/ included; the tests, the e2e package and the programs
# under internal/ (the fixture writer, the smoke exporter) are
# reported beside it under no ceiling at all (design §8, amended; spec 010
# #7). A number that charges for coverage argues for deleting tests.
#
# Usage: scripts/sdk-go-lines.sh [budget]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUDGET="${1:-2300}"

cd "$ROOT"

sources() {
	git ls-files 'sdk/go' | grep -E '\.go$' || true
}

app() { sources | grep -v -E '_test\.go$|^sdk/go/e2e/|^sdk/go/internal/(fixture|smoke)/' || true; }
tests() { sources | grep -E '_test\.go$|^sdk/go/e2e/|^sdk/go/internal/(fixture|smoke)/' || true; }

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
	echo "sdk-go-lines: no tracked package sources yet"
	exit 0
fi

total=$(lines "$APP")
covered=$(lines "$TEST")
printf 'sdk-go-lines: %d app lines across %d files (budget %d); %d test lines across %d files (no budget)\n' \
	"$total" "$(files "$APP")" "$BUDGET" "$covered" "$(files "$TEST")"

if [ "$total" -gt "$BUDGET" ]; then
	printf '::warning title=Go package line budget::the tracepad Go package is %d lines over its ~%d line budget (spec 033 #11)\n' \
		"$((total - BUDGET))" "$BUDGET"
fi
