#!/usr/bin/env bash
# Holds the three tag checks to what they promise (spec 020 #28): each accepts
# the tag of the version its package declares today, and refuses a tag that says
# anything else — in particular a pre-release spelled the other registry's way,
# which would otherwise be a second tag for the same version.
#
#   scripts/sdk-release-check-test.sh
#
# The release workflows are never run before the first tag. These are the part
# of them that can be.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scripts="$repo_root/scripts"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

failures=0

# ok <script> <tag> [VAR=value…]: the check must pass.
ok() {
    local script="$1" tag="$2"; shift 2
    if ! env "$@" "$scripts/$script" "$tag" >/dev/null 2>"$work/err"; then
        echo "FAIL: $script $tag should pass: $(cat "$work/err")" >&2
        failures=$((failures + 1))
    fi
}

# refused <script> <tag> [VAR=value…]: the check must fail.
refused() {
    local script="$1" tag="$2"; shift 2
    if env "$@" "$scripts/$script" "$tag" >/dev/null 2>&1; then
        echo "FAIL: $script $tag should be refused" >&2
        failures=$((failures + 1))
    fi
}

# What each package declares today, read the way each script reads it, so that
# bumping a version without its tag check's agreement shows up here and not on
# the day.
py_declared="$(sed -n 's/^VERSION = "\(.*\)"$/\1/p' "$repo_root/sdk/python/src/tracepad/_config.py")"
go_declared="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$repo_root/sdk/go/http.go")"
js_declared="$(node -p "require('$repo_root/sdk/js/package.json').version")"
[ -n "$py_declared" ] && [ -n "$go_declared" ] && [ -n "$js_declared" ] || {
    echo "FAIL: a package's version was not found (py '$py_declared', go '$go_declared', js '$js_declared')" >&2
    exit 1
}

# The tag of the version in the tree: semver's spelling, whatever PyPI's is.
py_tag="$(printf '%s\n' "$py_declared" | sed -E 's/rc([0-9]+)$/-rc.\1/; s/b([0-9]+)$/-beta.\1/; s/a([0-9]+)$/-alpha.\1/')"
ok sdk-py-release-check.sh "sdk-py/v$py_tag"
ok sdk-go-release-check.sh "sdk/go/v$go_declared"
ok sdk-js-release-check.sh "sdk-js/v$js_declared"

# A tag for another package, or for no package, is nobody's release.
refused sdk-py-release-check.sh "sdk-js/v$py_tag"
refused sdk-go-release-check.sh "sdk-py/v$go_declared"
refused sdk-js-release-check.sh "sdk/go/v$js_declared"
refused sdk-py-release-check.sh "v$py_tag"

# Python: the tag is semver's, PyPI's spelling is derived (PEP 440).
spell() { printf 'VERSION = "%s"\n' "$1" > "$work/_config.py"; }
py() { echo "SDK_PY_CONFIG=$work/_config.py"; }

spell 0.2.0;      ok      sdk-py-release-check.sh sdk-py/v0.2.0 "$(py)"
spell 0.2.0;      refused sdk-py-release-check.sh sdk-py/v0.2.1 "$(py)"
spell 0.2.0rc1;   ok      sdk-py-release-check.sh sdk-py/v0.2.0-rc.1 "$(py)"
spell 0.2.0b2;    ok      sdk-py-release-check.sh sdk-py/v0.2.0-beta.2 "$(py)"
spell 0.2.0a3;    ok      sdk-py-release-check.sh sdk-py/v0.2.0-alpha.3 "$(py)"
spell 0.2.0rc1;   refused sdk-py-release-check.sh sdk-py/v0.2.0 "$(py)"          # the release is not its candidate
spell 0.2.0rc1;   refused sdk-py-release-check.sh sdk-py/v0.2.0-rc.2 "$(py)"
spell 0.2.0rc1;   refused sdk-py-release-check.sh sdk-py/v0.2.0rc1 "$(py)"       # one tag per version, in one spelling
spell 0.2.0rc1;   refused sdk-py-release-check.sh sdk-py/v0.2.0-rc1 "$(py)"
spell 0.2.0;      refused sdk-py-release-check.sh sdk-py/v0.2.0-rc.1 "$(py)"
spell 0.2.0-rc.1; refused sdk-py-release-check.sh sdk-py/v0.2.0-rc.1 "$(py)"     # the package's own spelling must be PEP 440's
spell 0.2;        refused sdk-py-release-check.sh sdk-py/v0.2 "$(py)"
: > "$work/_config.py"; refused sdk-py-release-check.sh sdk-py/v0.2.0 "$(py)"   # no version found is not a match

# Go: the module's own spelling is the tag's.
go() { echo "SDK_GO_VERSION_FILE=$work/http.go"; }
gospell() { printf 'const Version = "%s"\n' "$1" > "$work/http.go"; }

gospell 0.2.0;      ok      sdk-go-release-check.sh sdk/go/v0.2.0 "$(go)"
gospell 0.2.0;      refused sdk-go-release-check.sh sdk/go/v0.2.1 "$(go)"
gospell 0.2.0-rc.1; ok      sdk-go-release-check.sh sdk/go/v0.2.0-rc.1 "$(go)"
gospell 0.2.0-rc.1; refused sdk-go-release-check.sh sdk/go/v0.2.0 "$(go)"
gospell 0.2.0rc1;   refused sdk-go-release-check.sh sdk/go/v0.2.0rc1 "$(go)"     # not a version the Go toolchain parses
: > "$work/http.go"; refused sdk-go-release-check.sh sdk/go/v0.2.0 "$(go)"

# Node: the tag and package.json spell it alike, so only the mismatch is left.
refused sdk-js-release-check.sh "sdk-js/v${js_declared}1"

if [ "$failures" -ne 0 ]; then
    echo "sdk-release-check-test: $failures failed" >&2
    exit 1
fi
echo "sdk-release-check-test: ok"
