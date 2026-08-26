#!/usr/bin/env bash
# Stop-hook: format git-dirty Go files once at the end of an agent turn.
#
# Why Stop and not PostToolUse: a hook that rewrites the file after every
# edit invalidates the agent's in-memory copy, forcing a full re-Read before
# the next edit and burning tokens. At turn end the whole turn edits against
# a stable file. `make format-check` in the gate remains the read-only
# safety net for non-agent edits.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$ROOT"

files=$( (git diff --name-only; git diff --cached --name-only; git ls-files --others --exclude-standard) | sort -u | grep '\.go$' || true)
[ -z "$files" ] && exit 0

for f in $files; do
  [ -f "$f" ] && gofmt -w "$f"
done
exit 0
