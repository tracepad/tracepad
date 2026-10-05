#!/usr/bin/env bash
# scripts/archive-check.sh, held to what it claims (spec 020 #34). The check
# only runs in CI's release rehearsal and in a release, so without this a
# change to it would be tried for the first time by a tag. What the check
# decides depends on the archive's name and on the first line its `tracepad`
# prints, so the archives hold a stub that prints one: no server is built, and
# the binary's own side is TestBuildLabel's. Three are made — a line as a
# release stamps it (passes), one without the commit (red), one with another
# (red) — and a name that disagrees with the line.
set -euo pipefail

cd "$(dirname "$0")/.."
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

target="$(scripts/archive-check.sh --target)"
commit=0123456789abcdef0123456789abcdef01234567
version=0.1.0-rc.1

archive() { # <dir> <archive's version> <log line's message>
    mkdir -p "$work/$1"
    cat > "$work/$1/tracepad" <<STUB
#!/bin/sh
echo "2026/01/01 00:00:00 INFO $3" >&2
exec sleep 60
STUB
    chmod +x "$work/$1/tracepad"
    tar -czf "$work/$1/tracepad_${2}_${target}.tar.gz" -C "$work/$1" tracepad
    rm "$work/$1/tracepad"
}

archive good "$version" "tracepad $version (0123456)"
archive dropped "$version" "tracepad $version"
archive other "$version" "tracepad $version (89abcde)"
archive renamed "0.1.0" "tracepad $version (0123456)"

scripts/archive-check.sh "$work/good" "$commit" > /dev/null ||
    { echo "archive-check-test: a correctly stamped archive was refused" >&2; exit 1; }
for bad in dropped other renamed; do
    if scripts/archive-check.sh "$work/$bad" "$commit" > /dev/null 2>&1; then
        echo "archive-check-test: the '$bad' archive passed" >&2
        exit 1
    fi
done
echo "archive-check-test: a stamped archive passes; one without its commit, with another, or under another version is refused"
