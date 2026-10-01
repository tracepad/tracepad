#!/usr/bin/env bash
# Holds the three tag checks to what they promise (spec 020 #28): each accepts
# the tag of the version its package declares today, and refuses a tag that says
# anything else — in particular a pre-release spelled the other registry's way,
# a version that is not canonical, or a Go major the module path cannot carry,
# which would otherwise be a second tag for the same version or one `go get`
# cannot resolve.
#
#   scripts/sdk-release-check-test.sh
#
# The release workflows are never run before the first tag. These are the part
# of them that can be. The scripts are run as the workflows run them, and what
# they print and whether they fail is all that is looked at: the tags in the
# fixtures below are written out, not computed, so that this file does not
# carry a second copy of the rules.
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

# The trees as they are: the tag each check says releases it must pass, and no
# other package's tag, nor the bare server tag, may.
py_tag="$("$scripts/sdk-py-release-check.sh" --print-tag)"
go_tag="$("$scripts/sdk-go-release-check.sh" --print-tag)"
js_tag="$("$scripts/sdk-js-release-check.sh" --print-tag)"
ok sdk-py-release-check.sh "$py_tag"
ok sdk-go-release-check.sh "$go_tag"
ok sdk-js-release-check.sh "$js_tag"
refused sdk-py-release-check.sh "$js_tag"
refused sdk-go-release-check.sh "$py_tag"
refused sdk-js-release-check.sh "$go_tag"
refused sdk-py-release-check.sh "v${py_tag#sdk-py/v}"

# Python: the tag is semver's, PyPI's spelling is what the package says (PEP 440).
py_spell() { printf 'VERSION = "%s"\n' "$1" > "$work/_config.py"; }
py_env="SDK_PY_CONFIG=$work/_config.py"

py_spell 0.2.0;      ok      sdk-py-release-check.sh sdk-py/v0.2.0 "$py_env"
py_spell 0.2.0;      refused sdk-py-release-check.sh sdk-py/v0.2.1 "$py_env"
py_spell 0.2.0rc1;   ok      sdk-py-release-check.sh sdk-py/v0.2.0-rc.1 "$py_env"
py_spell 0.2.0b2;    ok      sdk-py-release-check.sh sdk-py/v0.2.0-beta.2 "$py_env"
py_spell 0.2.0a3;    ok      sdk-py-release-check.sh sdk-py/v0.2.0-alpha.3 "$py_env"
py_spell 0.2.0rc1;   refused sdk-py-release-check.sh sdk-py/v0.2.0 "$py_env"          # the release is not its candidate
py_spell 0.2.0rc1;   refused sdk-py-release-check.sh sdk-py/v0.2.0-rc.2 "$py_env"
py_spell 0.2.0rc1;   refused sdk-py-release-check.sh sdk-py/v0.2.0rc1 "$py_env"       # one tag per version, in one spelling
py_spell 0.2.0rc1;   refused sdk-py-release-check.sh sdk-py/v0.2.0-rc1 "$py_env"
py_spell 0.2.0;      refused sdk-py-release-check.sh sdk-py/v0.2.0-rc.1 "$py_env"
py_spell 0.2.0-rc.1; refused sdk-py-release-check.sh sdk-py/v0.2.0-rc.1 "$py_env"     # the package's own spelling must be PEP 440's
py_spell 0.2;        refused sdk-py-release-check.sh sdk-py/v0.2 "$py_env"
py_spell 0.02.0;     refused sdk-py-release-check.sh sdk-py/v0.02.0 "$py_env"         # no leading zeros, in the tag or the package
py_spell 0.2.0;      refused sdk-py-release-check.sh sdk-py/v0.02.0 "$py_env"
: > "$work/_config.py"; refused sdk-py-release-check.sh sdk-py/v0.2.0 "$py_env"       # no version found is not a match

# Go: the module's own spelling is the tag's, and its major is the path's.
go_spell() { printf 'const Version = "%s"\n' "$1" > "$work/http.go"; }
go_mod() { printf 'module %s\n\ngo 1.25.0\n' "$1" > "$work/go.mod"; }
go_env=("SDK_GO_VERSION_FILE=$work/http.go" "SDK_GO_MOD=$work/go.mod")
v1=github.com/tracepad/tracepad/sdk/go

go_mod $v1
go_spell 0.2.0;      ok      sdk-go-release-check.sh sdk/go/v0.2.0 "${go_env[@]}"
go_spell 0.2.0;      refused sdk-go-release-check.sh sdk/go/v0.2.1 "${go_env[@]}"
go_spell 1.0.0;      ok      sdk-go-release-check.sh sdk/go/v1.0.0 "${go_env[@]}"
go_spell 0.2.0-rc.1; ok      sdk-go-release-check.sh sdk/go/v0.2.0-rc.1 "${go_env[@]}"
go_spell 0.2.0-rc.1; refused sdk-go-release-check.sh sdk/go/v0.2.0 "${go_env[@]}"
go_spell 0.2.0rc1;   refused sdk-go-release-check.sh sdk/go/v0.2.0rc1 "${go_env[@]}"  # not a version the Go toolchain parses
go_spell 0.2.0-rc1;  refused sdk-go-release-check.sh sdk/go/v0.2.0-rc1 "${go_env[@]}"
go_spell 0.01.0;     refused sdk-go-release-check.sh sdk/go/v0.01.0 "${go_env[@]}"    # canonical, or not at all
go_spell 0.2.0;      refused sdk-go-release-check.sh sdk/go/v0.02.0 "${go_env[@]}"
go_spell 2.0.0;      refused sdk-go-release-check.sh sdk/go/v2.0.0 "${go_env[@]}"     # v2 needs /v2 in the module path
go_spell 2.0.0-rc.1; refused sdk-go-release-check.sh sdk/go/v2.0.0-rc.1 "${go_env[@]}"
go_mod $v1/v2
go_spell 2.0.0;      ok      sdk-go-release-check.sh sdk/go/v2.0.0 "${go_env[@]}"
go_spell 1.9.0;      refused sdk-go-release-check.sh sdk/go/v1.9.0 "${go_env[@]}"     # and only v2 may carry it
go_spell 3.0.0;      refused sdk-go-release-check.sh sdk/go/v3.0.0 "${go_env[@]}"
: > "$work/http.go"; go_mod $v1
refused sdk-go-release-check.sh sdk/go/v0.2.0 "${go_env[@]}"

# Node: the same canonical spelling, the package's own.
js_spell() { printf '{"name": "tracepad", "version": "%s"}\n' "$1" > "$work/package.json"; }
js_env="SDK_JS_PACKAGE_JSON=$work/package.json"

js_spell 0.2.0;      ok      sdk-js-release-check.sh sdk-js/v0.2.0 "$js_env"
js_spell 0.2.0;      refused sdk-js-release-check.sh sdk-js/v0.2.1 "$js_env"
js_spell 0.2.0-rc.1; ok      sdk-js-release-check.sh sdk-js/v0.2.0-rc.1 "$js_env"
js_spell 0.2.0-rc.1; refused sdk-js-release-check.sh sdk-js/v0.2.0 "$js_env"
js_spell 0.2.0-rc1;  refused sdk-js-release-check.sh sdk-js/v0.2.0-rc1 "$js_env"      # npm accepts it; we do not tag it
js_spell 0.2.0-next.1; refused sdk-js-release-check.sh sdk-js/v0.2.0-next.1 "$js_env"
js_spell 0.02.0;     refused sdk-js-release-check.sh sdk-js/v0.02.0 "$js_env"

if [ "$failures" -ne 0 ]; then
    echo "sdk-release-check-test: $failures failed" >&2
    exit 1
fi
echo "sdk-release-check-test: ok"
