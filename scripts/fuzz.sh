#!/usr/bin/env bash
# Runs every Go fuzz target for a short time each (spec 052 #4).
#
# `go test -fuzz` takes one package and one target per run, so this finds the
# targets — every `func FuzzXxx` in a _test.go file under internal/ — and runs
# them in turn. A crash stops nothing: every target runs, the failing ones are
# named at the end, and the exit status is 1 if any failed. The input that
# crashed is written to testdata/fuzz/<Target>/ beside the package, where `go
# test` replays it from then on; commit it with the fix.
#
# Usage: scripts/fuzz.sh [target-regexp]
#   FUZZTIME=10s      how long each target runs (`go test -fuzztime`)
#   FUZZ_PARALLEL=2   workers per target; the default of all cores is not for a
#                     machine other people's builds share
#   FUZZ_MINIMIZE=5s  how long a new input of the corpus may be minimized
#
# The minimize time is the one that matters for throughput. The engine's
# default is a minute for every input that reaches new code, during which its
# workers report nothing: a three-minute run of a decoder with a rich corpus
# spent most of it there, at under a hundred executions in the minute. Only
# the input that crashes is worth minimizing, and five seconds shrinks it.
#
# Not part of the gate: ten seconds a target is a smoke test, and the finding
# worth having takes minutes (Decision 4).
set -euo pipefail

cd "$(dirname "$0")/.."
FUZZTIME="${FUZZTIME:-10s}"
FUZZ_PARALLEL="${FUZZ_PARALLEL:-2}"
FUZZ_MINIMIZE="${FUZZ_MINIMIZE:-5s}"
only="${1:-.}"

failed=()
ran=0
while IFS=: read -r file name; do
	[[ "$name" =~ $only ]] || continue
	pkg="./$(dirname "$file")"
	ran=$((ran + 1))
	echo "=== $pkg $name ($FUZZTIME)"
	if ! go test "$pkg" -run '^$' -fuzz "^${name}\$" -fuzztime "$FUZZTIME" -fuzzminimizetime "$FUZZ_MINIMIZE" \
		-parallel "$FUZZ_PARALLEL"; then
		failed+=("$pkg $name")
	fi
done < <(grep -rEo --include='*_test.go' '^func Fuzz[A-Za-z0-9_]*' internal | sed -E 's/^([^:]+):func /\1:/' | sort)

if [ "$ran" -eq 0 ]; then
	echo "no fuzz target matches '$only'" >&2
	exit 1
fi
if [ "${#failed[@]}" -gt 0 ]; then
	echo "FAILED:" >&2
	printf '  %s\n' "${failed[@]}" >&2
	exit 1
fi
echo "ok: $ran fuzz targets, $FUZZTIME each"
