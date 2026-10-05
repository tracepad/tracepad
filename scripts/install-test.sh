#!/usr/bin/env bash
# The install script (spec 053 #10), run as a person runs it — `sh install.sh`
# — against releases built here, in a temporary directory, and read through
# `file://` addresses: no network and no server. The archives are real
# tar.gz files with a stand-in `tracepad` inside and a real checksums.txt
# beside them; `uname`, `sysctl` and `gh` are stand-ins too, and the PATH the
# script sees holds nothing else of this machine's but the tools it needs, so
# a `gh` installed here cannot answer for the stand-in.
#
#   scripts/install-test.sh        (part of the gate: make install-script-test)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root/scripts/install.sh"
agent_line='  Set up Tracepad for this project: follow https://tracepad.github.io/tracepad/agent-setup.md'

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
failures=0
fail() {
	echo "install-test: $*" >&2
	failures=$((failures + 1))
}

# The system tools the script uses, and only those, by symlink.
sys="$tmp/sys"
mkdir -p "$sys"
for tool in sh curl tar gzip sed awk cut head mktemp rm mkdir cp chmod mv cat sha256sum shasum; do
	if path="$(command -v "$tool")"; then
		ln -s "$path" "$sys/$tool"
	fi
done
[ -e "$sys/sha256sum" ] || [ -e "$sys/shasum" ] || {
	echo "install-test: needs sha256sum or shasum" >&2
	exit 1
}

# The stand-ins. Each reads its behaviour from the environment of the run.
fake="$tmp/fake"
mkdir -p "$fake"
cat >"$fake/uname" <<'EOF'
#!/bin/sh
case "$1" in
-s) echo "${FAKE_UNAME_S:-Linux}" ;;
-m) echo "${FAKE_UNAME_M:-x86_64}" ;;
esac
EOF
cat >"$fake/sysctl" <<'EOF'
#!/bin/sh
[ -n "${FAKE_TRANSLATED:-}" ] || exit 1
echo "$FAKE_TRANSLATED"
EOF
cat >"$fake/gh" <<'EOF'
#!/bin/sh
case "$1 ${FAKE_GH:-loggedout}" in
"auth loggedout") exit 1 ;;
"auth "*) exit 0 ;;
"attestation pass") exit 0 ;;
"attestation fail") echo "no attestation matched" >&2; exit 1 ;;
esac
exit 2
EOF
chmod +x "$fake"/*

# release VERSION [stable] builds the release's archives and checksums.txt
# under $releases/download/vVERSION; `stable` also makes it what
# latest/download answers with, as GitHub's redirect does.
releases="$tmp/releases"
release() {
	local version="$1" dir="$releases/download/v$1" os arch work
	mkdir -p "$dir"
	for os in linux darwin; do
		for arch in amd64 arm64; do
			# The newest candidate in these tests has no linux/arm64 build.
			[ "$version/$os/$arch" = "0.4.0-rc.1/linux/arm64" ] && continue
			work="$tmp/build/$version-$os-$arch"
			mkdir -p "$work"
			cat >"$work/tracepad" <<EOF
#!/bin/sh
case "\$1" in
version) echo "$version" ;;
__arch) echo "$os/$arch" ;;
health)
	[ -n "\${FAKE_RUNNING:-}" ] || exit 1
	printf '{"version":"%s","ok":true}\n' "\$FAKE_RUNNING" ;;
skills)
	if [ "\${3:-}" = --dir ]; then d="\$4"; else d="\$HOME/.claude/skills"; fi
	mkdir -p "\$d/tracepad" && echo "$version" >"\$d/tracepad/.version"
	echo "installed $version to \$d/tracepad" ;;
esac
EOF
			chmod +x "$work/tracepad"
			echo "license" >"$work/LICENSE"
			tar -czf "$dir/tracepad_${version}_${os}_${arch}.tar.gz" -C "$work" tracepad LICENSE
		done
	done
	(cd "$dir" && if command -v sha256sum >/dev/null; then sha256sum tracepad_*; else shasum -a 256 tracepad_*; fi) >"$dir/checksums.txt"
	if [ "${2:-}" = stable ]; then
		mkdir -p "$releases/latest/download"
		cp "$dir/checksums.txt" "$releases/latest/download/checksums.txt"
	fi
}

# run NAME WANT_EXIT [VAR=value …]: the script under a clean environment, with
# HOME at $home and PATH the stand-ins and the system tools. Its output is in
# $out and $err.
home="$tmp/home"
run() {
	local name="$1" want="$2" got=0
	shift 2
	out="$tmp/$name.out"
	err="$tmp/$name.err"
	/usr/bin/env -i HOME="$home" PATH="$fake:$sys" \
		TRACEPAD_DOWNLOAD_URL="file://$releases" "$@" \
		sh "$script" >"$out" 2>"$err" || got=$?
	if [ "$got" != "$want" ]; then
		fail "$name: exit $got, want $want"
		sed 's/^/    out: /' "$out" >&2
		sed 's/^/    err: /' "$err" >&2
	fi
}
has() { grep -qF -- "$2" "$1" || fail "$3: $(basename "$1") lacks: $2"; }
lacks() { if grep -qF -- "$2" "$1"; then fail "$3: $(basename "$1") has: $2"; fi; }
ends_with_agent_line() { [ "$(tail -n 1 "$out")" = "$agent_line" ] || fail "$1: the last line is not the agent line"; }
fresh_home() {
	rm -rf "$home"
	mkdir -p "$home" "$@"
}
bin="$home/.local/bin/tracepad"

# --- No stable release yet: no fall-back to a candidate.
release 0.1.0-rc.1
fresh_home "$home/.claude"
run no-stable 1
has "$err" "there is no stable release" no-stable
has "$err" "| TRACEPAD_VERSION=<version> sh" no-stable
[ ! -e "$bin" ] || fail "no-stable: something was installed"

# --- The newest stable release, by default.
release 0.2.0 stable
run latest 0
has "$out" "installed tracepad 0.2.0 at $bin" latest
has "$out" "sha256 matches checksums.txt" latest
has "$out" "attestation not checked: gh is not logged in" latest
has "$out" "installed 0.2.0 to $home/.claude/skills/tracepad" latest
has "$out" "$home/.local/bin is not on your PATH" latest
ends_with_agent_line latest
[ "$("$bin" version)" = 0.2.0 ] || fail "latest: the binary is not 0.2.0"
[ "$("$bin" __arch)" = linux/amd64 ] || fail "latest: not the linux/amd64 archive"
[ ! -e "$home/.agents" ] || fail "latest: ~/.agents was made with no agent there"

# --- Again: nothing to do, and the skill reinstalled.
run again 0
has "$out" "tracepad 0.2.0 is already installed at $bin" again
has "$out" "verified  unchanged" again
ends_with_agent_line again

# --- A newer stable release: an upgrade, and the running server named.
release 0.3.0 stable
run upgrade 0 FAKE_RUNNING=0.2.0
has "$out" "updated tracepad 0.2.0 → 0.3.0 at $bin" upgrade
has "$out" "is still running 0.2.0; restart it to run 0.3.0" upgrade
ends_with_agent_line upgrade

# --- A pinned candidate, with or without its v.
release 0.4.0-rc.1
run pinned 0 TRACEPAD_VERSION=v0.4.0-rc.1
has "$out" "updated tracepad 0.3.0 → 0.4.0-rc.1" pinned
run pinned-bare 0 TRACEPAD_VERSION=0.4.0-rc.1
has "$out" "already installed" pinned-bare

# --- A version that does not exist.
run missing-version 1 TRACEPAD_VERSION=9.9.9
has "$err" "no release v9.9.9" missing-version

# --- No build for this platform in the release.
fresh_home
run no-build 1 TRACEPAD_VERSION=0.4.0-rc.1 FAKE_UNAME_M=aarch64
has "$err" "no build for linux/arm64" no-build
[ ! -e "$bin" ] || fail "no-build: something was installed"

# --- An archive that is not the one checksums.txt names.
release 0.5.0
printf 'tampered' >>"$releases/download/v0.5.0/tracepad_0.5.0_linux_amd64.tar.gz"
run tampered 1 TRACEPAD_VERSION=0.5.0
has "$err" "does not match checksums.txt" tampered
[ ! -e "$bin" ] || fail "tampered: something was installed"

# --- The attestation: checked when gh can, and a failure is fatal.
run attest-fail 1 FAKE_GH=fail
has "$err" "did not verify; nothing was installed" attest-fail
has "$err" "no attestation matched" attest-fail
[ ! -e "$bin" ] || fail "attest-fail: something was installed"
run attest-pass 0 FAKE_GH=pass
has "$out" "build attestation verified (gh)" attest-pass
# No gh at all: the stand-ins for the system, without the one for gh.
mkdir -p "$tmp/nogh"
cp "$fake/uname" "$fake/sysctl" "$tmp/nogh/"
fresh_home
run attest-no-gh 0 PATH="$tmp/nogh:$sys"
has "$out" "attestation not checked: gh is not installed" attest-no-gh

# --- Where the skill goes.
fresh_home
run skill-none 0
has "$out" "not installed: no ~/.claude, ~/.agents or ~/.codex here" skill-none
[ ! -e "$home/.claude" ] || fail "skill-none: ~/.claude was made"
fresh_home "$home/.codex"
run skill-codex 0
has "$out" "installed 0.3.0 to $home/.agents/skills/tracepad" skill-codex
lacks "$out" ".claude/skills" skill-codex
fresh_home "$home/.claude" "$home/.agents"
run skill-both 0
has "$out" "installed 0.3.0 to $home/.claude/skills/tracepad" skill-both
has "$out" "installed 0.3.0 to $home/.agents/skills/tracepad" skill-both
fresh_home "$home/.claude"
run skill-off 0 TRACEPAD_NO_SKILL=1
has "$out" "skipped (TRACEPAD_NO_SKILL=1)" skill-off
[ ! -e "$home/.claude/skills" ] || fail "skill-off: the skill was installed"

# --- Another directory, on the PATH: no advice about the PATH.
fresh_home
run install-dir 0 TRACEPAD_INSTALL_DIR="$home/bin/" PATH="$home/bin:$fake:$sys"
has "$out" "installed tracepad 0.3.0 at $home/bin/tracepad" install-dir
lacks "$out" "is not on your PATH" install-dir

# --- Platforms.
fresh_home
run darwin 0 FAKE_UNAME_S=Darwin FAKE_UNAME_M=arm64
[ "$("$bin" __arch)" = darwin/arm64 ] || fail "darwin: not the darwin/arm64 archive"
fresh_home
run rosetta 0 FAKE_UNAME_S=Darwin FAKE_UNAME_M=x86_64 FAKE_TRANSLATED=1
[ "$("$bin" __arch)" = darwin/arm64 ] || fail "rosetta: under Rosetta, not the native archive"
fresh_home
run intel-mac 0 FAKE_UNAME_S=Darwin FAKE_UNAME_M=x86_64 FAKE_TRANSLATED=0
[ "$("$bin" __arch)" = darwin/amd64 ] || fail "intel-mac: not the darwin/amd64 archive"
run windows 1 FAKE_UNAME_S=MINGW64_NT-10.0
has "$err" "is not a system this script installs for" windows
run riscv 1 FAKE_UNAME_M=riscv64
has "$err" "is not an architecture Tracepad is built for" riscv

# --- A mirror over plain HTTP is refused, not trusted.
run plain-http 1 TRACEPAD_DOWNLOAD_URL=http://127.0.0.1:9/releases
has "$err" "could not reach http://127.0.0.1:9/releases" plain-http

if [ "$failures" -gt 0 ]; then
	echo "install-test: $failures failure(s)" >&2
	exit 1
fi
echo "install-test: ok"
