#!/usr/bin/env bash
# The Python package's line budget (spec 017 #1).
#
# A warning, never a failure — the same instrument, and the same argument, as
# `ui-lines.sh`: the number is a signal to revise, not a rule to satisfy. 1,500
# lines covered spec 017 (init, the decorators, generations, prompts, scores) and
# spec 018 (the eval harness) together, because they are one package and a
# budget per spec would be a budget per PR; spec 031 #22 raised it to 1,600 for
# the streaming pass-through.
#
# What it counts is the application; the tests are reported beside it under no
# ceiling at all (design §8, amended; spec 010 #7). A number that charges for
# coverage argues for deleting tests.
#
# Usage: scripts/sdk-lines.sh [budget]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUDGET="${1:-1600}"

cd "$ROOT"

sources() {
	git ls-files 'sdk/python' | grep -E '\.py$' || true
}

app() { sources | grep -v -e '^sdk/python/tests/' || true; }
tests() { sources | grep -e '^sdk/python/tests/' || true; }

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
	echo "sdk-lines: no tracked package sources yet"
	exit 0
fi

total=$(lines "$APP")
covered=$(lines "$TEST")
printf 'sdk-lines: %d app lines across %d files (budget %d); %d test lines across %d files (no budget)\n' \
	"$total" "$(files "$APP")" "$BUDGET" "$covered" "$(files "$TEST")"

if [ "$total" -gt "$BUDGET" ]; then
	printf '::warning title=Python package line budget::the tracepad package is %d lines over its ~%d line budget (spec 017 #1)\n' \
		"$((total - BUDGET))" "$BUDGET"
fi
