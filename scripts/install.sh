#!/bin/sh
# Installs the tracepad binary and the agent skill it carries (spec 053).
#
#   curl -fsSL https://tracepad.github.io/tracepad/install.sh | sh
#   curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION=0.1.0-rc.1 sh
#
# The newest stable release by default; TRACEPAD_VERSION pins one, a release
# candidate included. The archive is checked against the release's
# checksums.txt, and against its build attestation as well when `gh` is
# installed and logged in. The binary goes to ~/.local/bin, or
# TRACEPAD_INSTALL_DIR — never with sudo. Running it again upgrades, or does
# nothing when the version is already there. TRACEPAD_NO_SKILL=1 leaves the
# skill alone; TRACEPAD_DOWNLOAD_URL points at a mirror of the releases page.
#
# POSIX sh: it runs under dash as well as bash, and everything is inside
# functions so that a download cut short runs nothing.
set -eu

REPO=tracepad/tracepad
SCRIPT_URL=https://tracepad.github.io/tracepad/install.sh
AGENT_LINE='Set up Tracepad for this project: follow https://tracepad.github.io/tracepad/agent-setup.md'

say() { printf '%s\n' "$*"; }
fail() {
	printf 'tracepad install: %s\n' "$*" >&2
	exit 1
}

# fetch URL FILE: the file, or curl's exit status. https and file:// only — a
# mirror that is plain HTTP is refused rather than trusted.
fetch() {
	curl --proto '=https,file' -fsSL --retry 2 -o "$2" "$1"
}

platform() {
	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) fail "$(uname -s) is not a system this script installs for; the archives for Windows and the rest are listed in https://tracepad.github.io/tracepad/install/" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "$(uname -m) is not an architecture Tracepad is built for (amd64, arm64)" ;;
	esac
	# A shell under Rosetta reports x86_64 on Apple silicon; the native build
	# is the one to install there.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi
}

# no_stable explains the one failure everyone meets before 0.1.0, with the
# line that gets past it. The candidate is named from GitHub's API when the
# releases are GitHub's; the API is asked nothing else, and decides nothing.
no_stable() {
	candidate=""
	if [ -z "${TRACEPAD_DOWNLOAD_URL:-}" ]; then
		candidate="$(curl --proto '=https' -fsSL "https://api.github.com/repos/$REPO/releases?per_page=10" 2>/dev/null |
			sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n 1 || true)"
	fi
	{
		say "tracepad install: there is no stable release of Tracepad yet, and this script installs a release candidate only when you name it."
		if [ -n "$candidate" ]; then
			say "The newest release is $candidate. To install it:"
		else
			say "The releases are listed at https://github.com/$REPO/releases. To install one:"
			candidate="<version>"
		fi
		say ""
		say "  curl -fsSL $SCRIPT_URL | TRACEPAD_VERSION=$candidate sh"
	} >&2
	exit 1
}

resolve_version() {
	if [ -n "${TRACEPAD_VERSION:-}" ]; then
		version="${TRACEPAD_VERSION#v}"
		fetch "$base/download/v$version/checksums.txt" "$tmp/checksums.txt" ||
			fail "no release v$version at $base (its checksums.txt did not download); the releases are listed at https://github.com/$REPO/releases"
	else
		# Not `fetch`: GitHub answers 404 when no stable release exists, and
		# `curl -f` reports that as 22 or, over HTTP/2, as 56 — the code of a
		# broken connection. The status code is the answer; under file://
		# there is none, and a missing file is curl's 37.
		status=0
		code="$(curl --proto '=https,file' -sSL --retry 2 -o "$tmp/checksums.txt" -w '%{http_code}' \
			"$base/latest/download/checksums.txt" 2>/dev/null)" || status=$?
		case "$status/$code" in
		0/200 | 0/000) ;;
		0/404 | 37/*) no_stable ;;
		0/*) fail "$base answered HTTP $code for the latest release's checksums.txt" ;;
		*) fail "could not reach $base (curl exit $status)" ;;
		esac
		# The version is in the archives' names: tracepad_<version>_<os>_<arch>.
		version="$(sed -n 's/^[0-9a-f]*[ *]*tracepad_\([^_]*\)_.*/\1/p' "$tmp/checksums.txt" | head -n 1)"
	fi
	case "$version" in
	"" | *[!0-9A-Za-z.-]*) fail "could not read a version from the release's checksums.txt" ;;
	esac
}

# sha256 FILE prints its digest with the tool main found.
sha256() {
	if [ "$sha_tool" = sha256sum ]; then
		sha256sum "$1" | cut -d ' ' -f 1
	else
		shasum -a 256 "$1" | cut -d ' ' -f 1
	fi
}

download_and_verify() {
	archive="tracepad_${version}_${os}_${arch}.tar.gz"
	expected="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
	[ -n "$expected" ] || fail "release v$version has no $archive in its checksums.txt: no build for $os/$arch"
	say "downloading $archive"
	fetch "$base/download/v$version/$archive" "$tmp/$archive" ||
		fail "could not download $base/download/v$version/$archive"
	actual="$(sha256 "$tmp/$archive")"
	[ "$actual" = "$expected" ] ||
		fail "$archive does not match checksums.txt (expected $expected, got $actual); nothing was installed"
	verified="sha256 matches checksums.txt"

	# Provenance when it can be checked; a failed check is never a warning.
	if ! command -v gh >/dev/null 2>&1; then
		verified="$verified; attestation not checked: gh is not installed"
	elif ! gh auth status >/dev/null 2>&1; then
		verified="$verified; attestation not checked: gh is not logged in"
	elif gh attestation verify "$tmp/$archive" --repo "$REPO" >"$tmp/attestation.log" 2>&1; then
		verified="$verified; build attestation verified (gh)"
	else
		cat "$tmp/attestation.log" >&2
		fail "the build attestation of $archive did not verify; nothing was installed"
	fi
}

# put_in_place copies the binary beside its destination and renames it over
# it: a running server keeps the file it has, and a failure leaves the old one.
put_in_place() {
	tar -xzf "$tmp/$archive" -C "$tmp" tracepad || fail "$archive has no tracepad in it"
	mkdir -p "$dir" || fail "could not create $dir"
	cp "$tmp/tracepad" "$dir/.tracepad.$$" || fail "could not write to $dir"
	chmod 0755 "$dir/.tracepad.$$"
	mv -f "$dir/.tracepad.$$" "$bin" || {
		rm -f "$dir/.tracepad.$$"
		fail "could not replace $bin"
	}
	now="$("$bin" version 2>/dev/null || true)"
	[ "$now" = "$version" ] || fail "$bin says it is '$now', not $version"
}

# install_skill installs the skill where an agent on this machine reads
# skills: Claude Code's directory, and the shared one Codex reads. A directory
# no agent has made is not made here.
install_skill() {
	if [ "${TRACEPAD_NO_SKILL:-}" = 1 ]; then
		skill_lines="skipped (TRACEPAD_NO_SKILL=1)"
		return
	fi
	skill_lines=""
	if [ -d "$HOME/.claude" ]; then
		out="$("$bin" skills install 2>&1)" || fail "tracepad skills install: $out"
		skill_lines="$out"
	fi
	if [ -d "$HOME/.agents" ] || [ -d "$HOME/.codex" ]; then
		out="$("$bin" skills install --dir "$HOME/.agents/skills" 2>&1)" ||
			fail "tracepad skills install --dir $HOME/.agents/skills: $out"
		skill_lines="${skill_lines:+$skill_lines
}$out"
	fi
	if [ -z "$skill_lines" ]; then
		skill_lines="not installed: no ~/.claude, ~/.agents or ~/.codex here. For your agent, one of:
    tracepad skills install                          # Claude Code
    tracepad skills install --dir ~/.agents/skills   # Codex, and agents that read ~/.agents/skills"
	fi
}

main() {
	base="${TRACEPAD_DOWNLOAD_URL:-https://github.com/$REPO/releases}"
	base="${base%/}"
	dir="${TRACEPAD_INSTALL_DIR:-$HOME/.local/bin}"
	dir="${dir%/}"
	bin="$dir/tracepad"

	command -v curl >/dev/null 2>&1 || fail "curl is required"
	command -v tar >/dev/null 2>&1 || fail "tar is required"
	if command -v sha256sum >/dev/null 2>&1; then
		sha_tool=sha256sum
	elif command -v shasum >/dev/null 2>&1; then
		sha_tool=shasum
	else
		fail "neither sha256sum nor shasum is installed, and an archive that cannot be checked is not installed"
	fi
	platform

	tmp="$(mktemp -d 2>/dev/null || mktemp -d -t tracepad)"
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' INT TERM HUP

	resolve_version

	before=""
	[ -x "$bin" ] && before="$("$bin" version 2>/dev/null || true)"
	if [ "$before" = "$version" ]; then
		headline="tracepad $version is already installed at $bin"
		verified="unchanged"
	else
		download_and_verify
		put_in_place
		if [ -n "$before" ]; then
			headline="updated tracepad $before → $version at $bin"
		else
			headline="installed tracepad $version at $bin"
		fi
	fi

	install_skill

	say ""
	say "$headline"
	say "  verified  $verified"
	say "$skill_lines" | sed '1s/^/  skill     /; 2,$s/^/            /'

	case ":$PATH:" in
	*":$dir:"*)
		first="$(command -v tracepad 2>/dev/null || true)"
		if [ -n "$first" ] && [ "$first" != "$bin" ]; then
			say ""
			say "Another tracepad comes first on your PATH: $first. Remove it, or run $bin."
		fi
		;;
	*)
		say ""
		say "$dir is not on your PATH. Add it in your shell's profile (~/.zshrc, ~/.bashrc), then open a new terminal:"
		say "  export PATH=\"$dir:\$PATH\""
		;;
	esac

	# A server started from the old binary keeps running it until restarted.
	running="$("$bin" health 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' || true)"
	if [ -n "$running" ] && [ "$running" != "$version" ]; then
		say ""
		say "The server tracepad health reaches is still running $running; restart it to run $version."
	fi

	say ""
	say "Next, paste this into your coding agent (Claude Code, Codex, Cursor, …):"
	say ""
	say "  $AGENT_LINE"
}

main "$@"
