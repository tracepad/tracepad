#!/usr/bin/env bash
# The Docker Hub mirror's step, as it stands in `release-server.yml` (spec 020
# #33). The step is inline in the workflow because the job that holds Docker
# Hub's credentials checks nothing out; this takes its `run:` block out of the
# file, so that what is tested is what is committed, and runs it against a
# stand-in for skopeo that records its calls.
#
#   scripts/mirror-step-test.sh              the cases below (part of the gate)
#   scripts/mirror-step-test.sh --extract    print the step's script, to run by
#                                            hand against a registry of your own
#
# What it holds: without both credentials the step fails and says so, and calls
# nothing; the image is read by digest and never by tag; each tag keeps its
# name; the password goes through stdin; a copy that lands at another digest, a
# tag that is not GHCR's and a digest that is not one are refused.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="$root/.github/workflows/release-server.yml"

extract() {
	ruby -ryaml -e '
		steps = YAML.load_file(ARGV[0]).dig("jobs", "mirror", "steps") or abort "no mirror job in #{ARGV[0]}"
		step = steps.find { |s| s["name"] == "Copy the index to Docker Hub" } or abort "no mirror step"
		puts step["run"]
	' "$workflow"
}

if [ "${1:-}" = --extract ]; then
	extract
	exit
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
extract >"$tmp/step.sh"
[ -s "$tmp/step.sh" ] || { echo "mirror-step-test: the step has no script" >&2; exit 1; }

# A skopeo that records how it was called and answers `copy` with whatever
# STUB_DIGEST says. A login's password is read from stdin, as the real one is,
# and kept apart so the test can say it was never on a command line.
mkdir "$tmp/bin"
cat >"$tmp/bin/skopeo" <<'EOF_STUB'
#!/usr/bin/env bash
echo "skopeo $*" >>"$STUB_LOG"
case "$1" in
login) cat >>"$STUB_LOG.passwords" ;;
copy)
	while [ $# -gt 0 ]; do
		[ "$1" = --digestfile ] && printf '%s' "$STUB_DIGEST" >"$2"
		shift
	done
	;;
esac
EOF_STUB
chmod +x "$tmp/bin/skopeo"

d=sha256:0000000000000000000000000000000000000000000000000000000000000001
other=sha256:0000000000000000000000000000000000000000000000000000000000000002
src=ghcr.io/tracepad/tracepad
dst=docker.io/tracepad/tracepad
failures=0

fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}

# run <name> <want exit> [VAR=value...]: the step, with the environment the
# job gives it (VARs override), TAGS and DIGEST as for a stable release.
run() {
	name="$1" want="$2"
	shift 2
	: >"$tmp/log"
	rm -f "$tmp/log.passwords"
	set +e
	out="$(env -i PATH="$tmp/bin:$PATH" HOME="$tmp" RUNNER_TEMP="$tmp" GITHUB_ACTOR=actor GITHUB_REF_NAME=v0.1.0 \
		STUB_LOG="$tmp/log" STUB_DIGEST="$d" SOURCE="$src" MIRROR="$dst" DIGEST="$d" \
		TAGS="$src:0.1.0,$src:0.1,$src:latest" GH_TOKEN=ghcr-token \
		DOCKERHUB_USERNAME=hub DOCKERHUB_TOKEN=hub-token \
		${@+"$@"} bash "$tmp/step.sh" 2>&1 </dev/null)"
	status=$?
	set -e
	if [ "$status" != "$want" ]; then
		fail "$name: exit $status, want $want"
		echo "$out" >&2
	fi
}
says() { grep -qF -- "$2" <<<"$out" || fail "$1: the output lacks '$2'"; }
calls() { grep -qF -- "$2" "$tmp/log" || fail "$1: no call with '$2'"; }
calls_end() { grep -qE -- "$2\$" "$tmp/log" || fail "$1: no call ending '$2'"; }
never() { ! grep -qF -- "$2" "$tmp/log" || fail "$1: '$2' was called"; }
silent() { [ ! -s "$tmp/log" ] || fail "$1: skopeo was called"; }

# A stable release: three tags, each from the digest and under its own name.
run stable 0
calls stable "skopeo login ghcr.io --username actor --password-stdin"
calls stable "skopeo login docker.io --username hub --password-stdin"
calls stable "copy --all --preserve-digests"
calls_end stable "docker://$src@$d docker://$dst:0\\.1\\.0"
calls_end stable "docker://$src@$d docker://$dst:0\\.1"
calls_end stable "docker://$src@$d docker://$dst:latest"
[ "$(grep -c '^skopeo copy' "$tmp/log")" = 3 ] || fail "stable: not three copies"
never stable "hub-token"
never stable "ghcr-token"
never stable "$src:"
[ "$(cat "$tmp/log.passwords")" = "$(printf 'ghcr-token\nhub-token')" ] || fail "stable: the tokens did not go through stdin"
says stable "mirrored $dst:latest at $d"

# A back-patch moves its exact tag, and not what it did not move.
run "back-patch" 0 TAGS="$src:0.2.5"
calls_end "back-patch" "docker://$dst:0\\.2\\.5"
never "back-patch" ":latest"

# Another registry, as in a rehearsal: both logins and every copy follow it.
run "other registry" 0 SOURCE=reg:5000/ghcr/tracepad MIRROR=reg:5000/hub/tracepad TAGS=reg:5000/ghcr/tracepad:0.1.0
calls "other registry" "skopeo login reg:5000 --username hub"
calls "other registry" "docker://reg:5000/ghcr/tracepad@$d docker://reg:5000/hub/tracepad:0.1.0"

# No credentials, or half of them: an explicit error, and nothing called.
run "no credentials" 1 DOCKERHUB_USERNAME= DOCKERHUB_TOKEN=
says "no credentials" "::error"
says "no credentials" "DOCKERHUB_TOKEN"
silent "no credentials"
run "no username" 1 DOCKERHUB_USERNAME=
says "no username" "::error"
silent "no username"
run "no token" 1 DOCKERHUB_TOKEN=
says "no token" "::error"
silent "no token"

# What is wrong is refused, and nothing is half done.
run "digest moved" 1 STUB_DIGEST="$other"
says "digest moved" "is $other, not $d"
run "foreign tag" 1 TAGS="$src:0.1.0,ghcr.io/someone/else:0.1.0"
says "foreign tag" "is not a tag of $src"
never "foreign tag" "copy"
run "bare repository" 1 TAGS="$src:"
says "bare repository" "is not a tag of $src"
run "no tags" 1 TAGS=
says "no tags" "no tags to mirror"
run "bad digest" 1 DIGEST=latest
says "bad digest" "gave no digest"
silent "bad digest"
run "no digest" 1 DIGEST=
says "no digest" "gave no digest"

if [ "$failures" -gt 0 ]; then
	echo "mirror-step-test: $failures failure(s)" >&2
	exit 1
fi
echo "mirror-step-test: ok"
