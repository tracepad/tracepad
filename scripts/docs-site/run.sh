#!/usr/bin/env bash
# Runs a command of the documentation toolchain — mkdocs, mike — from the
# pinned set in uv.lock (spec 050 #3). `--locked` refuses a lock that does not
# match pyproject.toml instead of resolving a new one, and `uv` refuses a
# package whose hash is not the one written down, so this is the only way the
# site is ever built: on a laptop, in the gate and in the workflow alike. The
# environment is the one in `scripts/docs-site/.venv`, which is not committed.
#
#   scripts/docs-site/run.sh mkdocs build --strict
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"

# Material prints a banner about MkDocs 2.0 on every run, which is advice
# for people choosing a toolchain and not output of a build.
export NO_MKDOCS_2_WARNING=true

command -v uv >/dev/null || {
	echo "docs-site: uv is required here: it installs the toolchain pinned in $DIR/uv.lock (https://docs.astral.sh/uv/)" >&2
	exit 1
}

exec uv run --quiet --project "$DIR" --locked "$@"
