#!/usr/bin/env bash
# The Node package's line budget (spec 032 #11).
#
# A warning, never a failure — the same instrument, and the same argument, as
# `sdk-lines.sh` and `ui-lines.sh`: the number is a signal to revise, not a
# rule to satisfy. 1,800 lines were to cover spec 032's package and its eval
# harness together, because they are one package and a budget per PR would
# be a budget per spec — higher than Python's 1,600 because TypeScript spends
# lines on types that Python spends on nothing — and spec 032 #16 raised it
# to 1,900 for the harness, spec 036 #8 to 2,000 for trace deletion, spec
# 040 #13 to 2,150 for `tracepad/testing`.
#
# What it counts is the application; the tests are reported beside it under
# no ceiling at all (design §8, amended; spec 010 #7).
#
# Usage: scripts/sdk-js-lines.sh [budget]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUDGET="${1:-2150}"

cd "$ROOT"

sources() {
	git ls-files 'sdk/js/src' 'sdk/js/test' | grep -E '\.ts$' || true
}

app() { sources | grep -v -e '^sdk/js/test/' || true; }
tests() { sources | grep -e '^sdk/js/test/' || true; }

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
	echo "sdk-js-lines: no tracked package sources yet"
	exit 0
fi

total=$(lines "$APP")
covered=$(lines "$TEST")
printf 'sdk-js-lines: %d app lines across %d files (budget %d); %d test lines across %d files (no budget)\n' \
	"$total" "$(files "$APP")" "$BUDGET" "$covered" "$(files "$TEST")"

if [ "$total" -gt "$BUDGET" ]; then
	printf '::warning title=Node package line budget::the tracepad package is %d lines over its ~%d line budget (spec 032 #11)\n' \
		"$((total - BUDGET))" "$BUDGET"
fi
