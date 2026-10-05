#!/usr/bin/env bash
# The release archive, started (spec 020 #34). The log's first line is the
# version and the commit the binary was built from (spec 001 #25), and the only
# thing that puts the commit there is `-X main.commit` in `.goreleaser.yaml`:
# drop it and a build in a checkout still compiles, still passes every test, and
# says `tracepad 0.1.0` with nothing after it. So the archive this machine can
# run is unpacked, started on a throwaway data directory, and its first line
# compared whole with `tracepad <version> (<first 7 of the commit>)`.
#
#   scripts/archive-check.sh <dist-dir> <commit>
#
# The version is read from the archive's own name (`tracepad_<version>_<os>_
# <arch>.tar.gz`, spec 020 #25), so a stamp that disagrees with the name is
# red as well. Only the archive for this machine can run; the six differ by the
# target alone, which the stamp is not.
set -euo pipefail

[ "$#" -eq 2 ] || { echo "usage: $0 <dist-dir> <commit>" >&2; exit 2; }
dist="$1"
commit="$2"

fail() { echo "archive-check: $*" >&2; exit 1; }

[[ "$commit" =~ ^[0-9a-f]{7,40}$ ]] || fail "the commit must be 7 to 40 hex digits, got '$commit'"

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) fail "no archive of this system is built" ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) fail "no archive of this architecture is built" ;; esac

archives=("$dist"/tracepad_*_"${os}_${arch}".tar.gz)
[ "${#archives[@]}" -eq 1 ] && [ -f "${archives[0]}" ] || fail "want exactly one tracepad_*_${os}_${arch}.tar.gz in $dist, found: ${archives[*]}"
archive="${archives[0]}"
name="$(basename "$archive")"
version="${name#tracepad_}"
version="${version%_"${os}_${arch}".tar.gz}"

work="$(mktemp -d)"
pid=""
cleanup() {
    if [ -n "$pid" ]; then
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    fi
    rm -rf "$work"
}
trap cleanup EXIT

tar -xzf "$archive" -C "$work" tracepad || fail "$name holds no 'tracepad'"
log="$work/log"
# Port 0: the check must not fail because something on the machine holds 4318.
"$work/tracepad" serve --data-dir "$work/data" --listen 127.0.0.1:0 >"$log" 2>&1 &
pid=$!

# The first line is written before the database is opened; waiting for it, not
# for the server to be up, keeps a slow start from being read as a missing line.
deadline=$((SECONDS + 30))
while ! [ -s "$log" ]; do
    kill -0 "$pid" 2>/dev/null || break
    [ "$SECONDS" -lt "$deadline" ] || fail "no log line after 30s"
    sleep 0.2
done
first="$(head -n 1 "$log" || true)"

want="tracepad $version (${commit:0:7})"
message="${first#* INFO }"
if [ "$message" != "$want" ]; then
    echo "--- log ---" >&2
    cat "$log" >&2
    fail "the first line of $name's log is '$first', want its message to be exactly '$want'"
fi
echo "archive-check: $name says '$message'"
