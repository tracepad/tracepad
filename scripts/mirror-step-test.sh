#!/usr/bin/env bash
# The Docker Hub mirror's step, as it stands in `mirror-image.yml` (spec 020
# #33). The step is inline because the job that holds Docker Hub's credentials
# checks nothing out; this takes its `run:` block out of the file, so that what
# is tested is what is committed, and runs it against stand-ins for `skopeo`
# and `gh` that record their calls.
#
#   scripts/mirror-step-test.sh              the cases below (part of the gate)
#   scripts/mirror-step-test.sh --extract    print the step's script, to run by
#                                            hand against registries of your own
#
# What it holds: without both credentials the step fails and says so, and calls
# nothing; the image is read by digest and never by tag, and from GHCR once —
# the other tags are made from what that left on Docker Hub; the exact version
# always moves, `X.Y` and `latest` only when the tag is the newest by the tags
# that exist now, asked after the first copy (a re-run of an older release must
# not walk them backwards) — and by the same answer as `check` gives for GHCR,
# which is run here over the same tag sets; the password goes through stdin; a
# copy that lands at another digest stops the step before the next tag, and a
# tag that is not a stable one is refused.
#
# The two steps are taken out of the workflow files with awk: nothing here
# needs more than the gate already does.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="$root/.github/workflows/mirror-image.yml"

# extract_step <workflow> <step name>: the `run: |` block of the step, which in
# these files sits at ten spaces under a step at six.
extract_step() {
	awk -v name="$2" '
		$0 == "      - name: " name { found = 1; next }
		found && /^      - / { exit }
		found && /^        run: \|$/ { inrun = 1; next }
		inrun && /^          / { sub(/^          /, ""); print; next }
		inrun && /^[[:space:]]*$/ { print ""; next }
		inrun { exit }
	' "$1"
}
extract() { extract_step "$workflow" "Copy the index to Docker Hub"; }

if [ "${1:-}" = --extract ]; then
	extract
	exit
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# Run from a git hook (the pre-push of `make gate`) this inherits GIT_DIR and
# its kin, which would point a scratch repository at the real one; every
# repository made here goes through the shared isolation (spec 020 #35), which
# also keeps the machine's own git configuration out of it.
# shellcheck source=scripts/lib/isolated-git.sh
. "$root/scripts/lib/isolated-git.sh"
isolated_git_sandbox "$tmp"

# What the real repository looked like before: this test makes repositories of
# its own and must not have touched this one (scripts/gate-guard.sh does the
# same for the whole gate).
outer_state() { (cd "$root" && "$root/scripts/repo-fingerprint.sh"); }
outer_before="$(outer_state)"
extract >"$tmp/step.sh"
extract_step "$root/.github/workflows/release-server.yml" "The tag, and the image tags it moves" >"$tmp/check.sh"
for f in step check; do
	if ! [ -s "$tmp/$f.sh" ] || ! bash -n "$tmp/$f.sh"; then
		echo "mirror-step-test: the $f step was not found, or is not a script" >&2
		exit 1
	fi
done

# A skopeo that records how it was called and answers `copy` with whatever
# STUB_DIGEST says, and `inspect --raw` with a manifest of its own; a login's
# password is read from stdin, as the real one is, and kept apart so the test
# can say it was never on a command line. A gh that answers the list of refs
# the API would, or fails.
mkdir "$tmp/bin"
cat >"$tmp/bin/skopeo" <<'EOF_STUB'
#!/usr/bin/env bash
echo "skopeo $*" >>"$STUB_LOG"
case "$1" in
login) cat >>"$STUB_LOG.passwords" ;;
inspect) printf '%s' '{"stub":"manifest"}' ;;
copy)
	while [ $# -gt 0 ]; do
		[ "$1" = --digestfile ] && printf '%s' "$STUB_DIGEST" >"$2"
		shift
	done
	;;
esac
EOF_STUB
cat >"$tmp/bin/gh" <<'EOF_STUB'
#!/usr/bin/env bash
echo "gh $*" >>"$STUB_LOG"
[ -z "${STUB_GH_FAIL:-}" ] || { echo "gh: HTTP 502" >&2; exit 1; }
for t in $STUB_TAGS; do
	case "$t" in v*) echo "refs/tags/$t" ;; esac # the API lists by prefix
done
EOF_STUB
chmod +x "$tmp/bin/skopeo" "$tmp/bin/gh"
if ! command -v sha256sum >/dev/null; then
	printf '#!/usr/bin/env bash\nshasum -a 256 "$@"\n' >"$tmp/bin/sha256sum"
	chmod +x "$tmp/bin/sha256sum"
fi

d=sha256:0000000000000000000000000000000000000000000000000000000000000001
other=sha256:0000000000000000000000000000000000000000000000000000000000000002
inspected="sha256:$(printf '%s' '{"stub":"manifest"}' | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-64)"
src=ghcr.io/tracepad/tracepad
dst=docker.io/tracepad/tracepad
failures=0

fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}
# shellcheck disable=SC2001 # a character class, which ${//} cannot say
re() { sed 's/[][\.*^$+?(){}|]/\\&/g' <<<"$1"; } # a string as an extended regular expression

# run <name> <want exit> [VAR=value...]: the step, with the environment the
# job gives it (VARs override), run on v0.1.0, the only tag, as the first
# release is.
run() {
	name="$1" want="$2"
	shift 2
	: >"$tmp/log"
	rm -f "$tmp/log.passwords"
	set +e
	out="$(env -i PATH="$tmp/bin:$PATH" HOME="$tmp" RUNNER_TEMP="$tmp" GITHUB_ACTOR=actor \
		GITHUB_REF_NAME=v0.1.0 GITHUB_REPOSITORY=tracepad/tracepad STUB_TAGS="v0.1.0" \
		STUB_LOG="$tmp/log" STUB_DIGEST="$d" SOURCE="$src" MIRROR="$dst" DIGEST="$d" GH_TOKEN=ghcr-token \
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
# calls_end <name> <from> <to>: a copy of <from> to the tag <to>, and nothing after it
calls_end() { grep -qE -- "^skopeo copy .* docker://$(re "$2") docker://$(re "$3")\$" "$tmp/log" || fail "$1: no copy of $2 to $3"; }
never() { ! grep -qF -- "$2" "$tmp/log" || fail "$1: '$2' was called"; }
silent() { [ ! -s "$tmp/log" ] || fail "$1: skopeo or gh was called"; }
copies() { [ "$(grep -c '^skopeo copy' "$tmp/log")" = "$2" ] || fail "$1: not $2 copies"; }

# The first release: its exact tag, `0.1` and `latest`. The first copy is from
# GHCR by digest; the other two are made from Docker Hub's own, by digest.
run first-release 0
calls first-release "skopeo login ghcr.io --username actor --password-stdin"
calls first-release "skopeo login docker.io --username hub --password-stdin"
calls first-release "copy --all --preserve-digests"
calls_end first-release "$src@$d" "$dst:0.1.0"
calls_end first-release "$dst@$d" "$dst:0.1"
calls_end first-release "$dst@$d" "$dst:latest"
copies first-release 3
[ "$(grep -c "docker://$(re "$src")" "$tmp/log")" = 1 ] || fail "first-release: GHCR was read more than once"
never first-release "hub-token"
never first-release "ghcr-token"
never first-release "$src:"
[ "$(cat "$tmp/log.passwords")" = "$(printf 'ghcr-token\nhub-token')" ] || fail "first-release: the tokens did not go through stdin"
says first-release "mirrored $dst:latest at $d"
never first-release "skopeo inspect"
# The tags that exist are asked for after the exact version is copied, and
# before the moving ones are.
first_copy="$(grep -n '^skopeo copy' "$tmp/log" | head -1 | cut -d: -f1)"
asked="$(grep -n '^gh api' "$tmp/log" | head -1 | cut -d: -f1)"
second_copy="$(grep -n '^skopeo copy' "$tmp/log" | sed -n 2p | cut -d: -f1)"
{ [ "$first_copy" -lt "$asked" ] && [ "$asked" -lt "$second_copy" ]; } || fail "first-release: the tags are not asked between the first copy and the second"

# By hand there is no digest: it is the hash of what GHCR holds under the version.
run by-hand 0 DIGEST= STUB_DIGEST="$inspected"
calls by-hand "skopeo inspect --raw docker://$src:0.1.0"
calls_end by-hand "$src@$inspected" "$dst:0.1.0"

# A newer release on the same line, and a candidate beside it, which counts for nothing.
run newest 0 GITHUB_REF_NAME=v0.2.1 STUB_TAGS="v0.1.0 v0.2.0 v0.2.1 v0.3.0-rc.1"
calls_end newest "$src@$d" "$dst:0.2.1"
calls_end newest "$dst@$d" "$dst:0.2"
calls_end newest "$dst@$d" "$dst:latest"

# An older release's mirror re-run after a newer one: its exact tag, and
# neither of the moving ones — what #16 refuses on GHCR, refused on Docker Hub.
run re-run 0 GITHUB_REF_NAME=v0.2.0 STUB_TAGS="v0.2.0 v0.2.1"
calls_end re-run "$src@$d" "$dst:0.2.0"
copies re-run 1
says re-run "not moving 0.2: v0.2.1 is newer on that line"
says re-run "not moving latest: v0.2.1 is newer"

# A back-patch to an older line: its version, and its own line's tag.
run back-patch 0 GITHUB_REF_NAME=v0.2.5 STUB_TAGS="v0.2.4 v0.2.5 v0.3.0"
calls_end back-patch "$src@$d" "$dst:0.2.5"
calls_end back-patch "$dst@$d" "$dst:0.2"
copies back-patch 2
never back-patch ":latest"
says back-patch "not moving latest: v0.3.0 is newer"

# `0.10` is not on `0.1`'s line, and 0.9 is older than 0.10.
run "two digits" 0 GITHUB_REF_NAME=v0.1.5 STUB_TAGS="v0.1.5 v0.10.0"
calls_end "two digits" "$dst@$d" "$dst:0.1"
never "two digits" ":latest"
run "two digits, newer" 0 GITHUB_REF_NAME=v0.10.0 STUB_TAGS="v0.9.0 v0.10.0"
calls_end "two digits, newer" "$dst@$d" "$dst:latest"

# Names that mean something to a regular expression are matched as names:
# the escaping itself, and two registries whose names would match other lines.
[ "$(re 'a+b(c){d}|e?.f[g]*^$')" = 'a\+b\(c\)\{d\}\|e\?\.f\[g\]\*\^\$' ] || fail "re: not every metacharacter is escaped"
[ "$(re 'a/b:c@d')" = 'a/b:c@d' ] || fail "re: it escaped what is no metacharacter"
run "metacharacters" 0 SOURCE='reg:5000/g+h(1)/tp' MIRROR='reg:5000/h{2}x?/tp'
calls_end "metacharacters" 'reg:5000/g+h(1)/tp'"@$d" 'reg:5000/h{2}x?/tp:0.1.0'
calls_end "metacharacters" 'reg:5000/h{2}x?/tp'"@$d" 'reg:5000/h{2}x?/tp:latest'

# Another registry, as in a rehearsal: both logins and every copy follow it.
run "other registry" 0 SOURCE=reg:5000/ghcr/tracepad MIRROR=reg:5000/hub/tracepad
calls "other registry" "skopeo login reg:5000 --username hub"
calls_end "other registry" "reg:5000/ghcr/tracepad@$d" "reg:5000/hub/tracepad:0.1.0"

# No credentials, or half of them: an explicit error that names the way back,
# and nothing called.
run "no credentials" 1 DOCKERHUB_USERNAME= DOCKERHUB_TOKEN=
says "no credentials" "::error"
says "no credentials" "DOCKERHUB_TOKEN"
says "no credentials" "gh workflow run mirror-image.yml --ref v0.1.0"
silent "no credentials"
run "no username" 1 DOCKERHUB_USERNAME=
says "no username" "::error"
silent "no username"
run "no token" 1 DOCKERHUB_TOKEN=
says "no token" "::error"
silent "no token"

# What is wrong is refused, and nothing is half done: after a copy that lands
# elsewhere the next tag is not made.
run "digest moved" 1 STUB_DIGEST="$other"
says "digest moved" "is $other, not $d"
copies "digest moved" 1
never "digest moved" ":latest"
for ref in v0.1.0-rc.1 v0.1 v01.0.0 vx.y.z; do
	run "ref $ref" 1 GITHUB_REF_NAME="$ref"
	says "ref $ref" "is not a stable release tag"
	silent "ref $ref"
done
# The exact version is copied before the question is asked, and nothing moves
# after an answer that is none.
run "tag unlisted" 1 STUB_TAGS=""
says "tag unlisted" "is not among the tags"
copies "tag unlisted" 1
run "api failure" 1 STUB_GH_FAIL=1
copies "api failure" 1
run "bad digest" 1 DIGEST=latest
says "bad digest" "no digest to mirror"
copies "bad digest" 0

# `check` and this step both answer "which tags may this release move" (#16),
# one from `git tag` and the other from the API; they must give the same
# answer over the same tags. `check`'s step is run for real, in a repository
# that has exactly these tags.
agree() { # agree <ref> <tags...>
	local ref="$1" repo="$tmp/repo" tags="${*:2}" t by_check by_mirror
	rm -rf "$repo"
	scratch_repo "$repo"
	git -C "$repo" commit -q --allow-empty -m x
	for t in $tags; do git -C "$repo" tag "$t"; done
	ln -s "$root/scripts" "$repo/scripts"
	: >"$tmp/output"
	# As Actions runs a `run:` block: `bash -e -o pipefail`; what it said is kept
	# for the failure.
	if ! check_out="$(cd "$repo" && GITHUB_REF_NAME="$ref" GITHUB_REPOSITORY=tracepad/tracepad GITHUB_OUTPUT="$tmp/output" \
		bash -e -o pipefail "$tmp/check.sh" 2>&1)"; then
		fail "agree $ref in [$tags]: check refused it:"
		echo "$check_out" >&2
		return
	fi
	by_check="$(sed -n 's/^tags=//p' "$tmp/output" | tr ',' '\n' | sed -E "s#^$(re "$src"):##" | sort | paste -sd' ' -)"
	run "agree $ref" 0 GITHUB_REF_NAME="$ref" STUB_TAGS="$tags"
	by_mirror="$(sed -En "s#^skopeo copy .* docker://$(re "$dst"):##p" "$tmp/log" | sort | paste -sd' ' -)"
	[ "$by_check" = "$by_mirror" ] || fail "check moves [$by_check] and the mirror [$by_mirror] for $ref among [$tags]"
}
agree v0.1.0 v0.1.0
agree v0.2.0 v0.2.0 v0.2.1
agree v0.2.1 v0.2.0 v0.2.1
agree v0.2.1 v0.1.0 v0.2.0 v0.2.1 v0.3.0-rc.1
agree v0.2.4 v0.2.4 v0.2.5 v0.3.0
agree v0.2.5 v0.2.4 v0.2.5 v0.3.0
agree v0.3.0 v0.2.4 v0.2.5 v0.3.0
agree v0.1.5 v0.1.5 v0.10.0
agree v0.10.0 v0.1.5 v0.10.0
agree v0.9.0 v0.9.0 v0.10.0
agree v0.10.0 v0.9.0 v0.10.0
agree v0.2.0 v0.2.0 v0.3.0-rc.1
agree v0.3.0 v0.3.0-rc.1 v0.3.0
agree v0.2.0 v0.2.0 "v0.3.0+x"
agree v0.2.9 v0.2.9 v0.3.0-rc.2 "v0.3.0+x" sdk/go/v9.0.0 sdk-py/v9.0.0 sdk-js/v9.0.0
# A tag the shape refuses — a leading zero — is no release for either: it must
# not be the newest, whichever way a version sort would read it.
agree v0.3.0 v0.3.0 v0.3.00
agree v0.3.0 v0.3.0 v0.3.00 v01.2.3 v0.03.1
agree v0.3.1 v0.3.0 v0.3.00 v0.3.1 v0.3.01
agree v0.2.9 v0.2.9 v0.3.00 v1.02.0 v01.0.0
agree v1.0.0 v0.9.9 v0.10.0 v1.0.0
agree v0.10.0 v0.9.9 v0.10.0 v1.0.0

[ "$(outer_state)" = "$outer_before" ] || fail "the repository this runs in was changed (its configuration, its tags, or which repository it is): was [$outer_before], is [$(outer_state)]"

if [ "$failures" -gt 0 ]; then
	echo "mirror-step-test: $failures failure(s)" >&2
	exit 1
fi
echo "mirror-step-test: ok"
