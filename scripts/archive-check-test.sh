#!/usr/bin/env bash
# scripts/archive-check.sh, held to what it claims (spec 020 #34). The check
# only runs in CI's release rehearsal and in a release, so without this a
# change to it would be tried for the first time by a tag. Three archives of
# this machine's own kind are built from the tree: one stamped as a release
# stamps (passes), one whose `-X main.commit` was dropped (red), and one
# stamped with another commit than the one asked for (red).
set -euo pipefail

cd "$(dirname "$0")/.."
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "archive-check-test: unsupported system" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "archive-check-test: unsupported architecture" >&2; exit 1 ;; esac

commit=0123456789abcdef0123456789abcdef01234567
version=0.1.0-rc.1

archive() { # <dir> <ldflags>: build into <dir> as GoReleaser names the archive
    mkdir -p "$work/$1"
    go build -o "$work/$1/tracepad" -ldflags "$2" ./cmd/tracepad
    tar -czf "$work/$1/tracepad_${version}_${os}_${arch}.tar.gz" -C "$work/$1" tracepad
    rm "$work/$1/tracepad"
}

archive good "-X main.version=$version -X main.commit=0123456"
archive dropped "-X main.version=$version"
archive other "-X main.version=$version -X main.commit=89abcde"

scripts/archive-check.sh "$work/good" "$commit" > /dev/null ||
    { echo "archive-check-test: a correctly stamped archive was refused" >&2; exit 1; }
for bad in dropped other; do
    if scripts/archive-check.sh "$work/$bad" "$commit" > /dev/null 2>&1; then
        echo "archive-check-test: the '$bad' archive passed" >&2
        exit 1
    fi
done
echo "archive-check-test: a stamped archive passes; one without its commit, and one with another, are refused"
