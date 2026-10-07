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
UPGRADE_PAGE=https://tracepad.github.io/tracepad/agent-upgrade.md

# q quotes a word for the shell, for a command this script prints: a path is
# the person's to choose, and one with a quote, a space or a `$(` in it must
# paste as itself (the second review of #228).
q() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

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
	*) fail "$(uname -s) is not a system this script installs for; the archives for Windows and the rest are listed in https://tracepad.github.io/tracepad/latest/install/" ;;
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
		# Printed below, so a version's characters only (spec 054 #15).
		case "$candidate" in *[!0-9A-Za-z.-]*) candidate="" ;; esac
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

# put_in_place copies the binary beside its destination, runs it there, and
# only then renames it over the old one: a running server keeps the file it
# has, and a binary that does not run here (a noexec mount, another
# architecture) leaves the old one as it was.
put_in_place() {
	tar -xzf "$tmp/$archive" -C "$tmp" tracepad || fail "$archive has no tracepad in it"
	mkdir -p "$dir" || fail "could not create $dir"
	new="$dir/.tracepad.$$"
	cp "$tmp/tracepad" "$new" || fail "could not write to $dir"
	chmod 0755 "$new"
	now="$("$new" version 2>/dev/null || true)"
	case "$now" in *[!0-9A-Za-z.-]*) now="something else" ;; esac
	if [ "$now" != "$version" ]; then
		rm -f "$new"
		fail "the new binary says it is '$now', not $version (is $dir mounted noexec?); $bin is unchanged"
	fi
	mv -f "$new" "$bin" || {
		rm -f "$new"
		fail "could not replace $bin"
	}
}

# newer A B: whether version A is later than B, by semver's order — a release
# is later than its candidates, and alpha < beta < rc. A version that is not
# X.Y.Z[-pre.N] (a development build's `dev`) is later than nothing.
newer() {
	awk -v a="$1" -v b="$2" '
	function parse(v, out,   i, n, parts) {
		out[4] = ""
		i = index(v, "-")
		if (i) { out[4] = substr(v, i + 1); v = substr(v, 1, i - 1) }
		n = split(v, parts, ".")
		if (n != 3) return 0
		for (i = 1; i <= 3; i++) {
			if (parts[i] !~ /^[0-9]+$/) return 0
			out[i] = parts[i] + 0
		}
		return 1
	}
	BEGIN {
		if (!parse(a, x) || !parse(b, y)) exit 1
		for (i = 1; i <= 3; i++) if (x[i] != y[i]) exit !(x[i] > y[i])
		if (x[4] == y[4] || y[4] == "") exit 1
		if (x[4] == "") exit 0
		split(x[4], p, "."); split(y[4], q, ".")
		if (p[1] != q[1]) exit !(p[1] > q[1])
		exit !(p[2] + 0 > q[2] + 0)
	}'
}

# skill_into runs `tracepad skills install ARGS…` and adds what it said to
# skill_lines, or fails with it.
skill_into() {
	out="$("$bin" skills install "$@" 2>&1)" || fail "tracepad skills install $*: $out"
	skill_lines="${skill_lines:+$skill_lines
}$out"
}

# plan asks the binary just put in place what still runs an older version
# (spec 054 #15): each server and container, whose it is, and what to do. Its
# report escapes what other programs supplied. Fifteen seconds at most: this is
# advice, not a step that may hang: the watchdog's SIGTERM ends it with exit 1
# and no verdict, and leaves $tmp/timeout behind — the plan catches SIGTERM, so
# its exit status cannot say it was the watchdog (the review of #228).
# plan_status is its exit status: 10 or 4 when something runs older, 0 when
# nothing does, 2 from a binary from before the command; anything else — a
# refusal, the watchdog — is "could not check", never "runs older".
plan() {
	"$bin" upgrade --plan --to "$version" >"$tmp/plan" 2>/dev/null &
	planner=$!
	# The marker only for a plan still running: one that ended on its own at
	# the fifteenth second says why itself (the sixth review of #228).
	(sleep 15 && kill -0 "$planner" && : >"$tmp/timeout" && kill "$planner") >/dev/null 2>&1 &
	watchdog=$!
	plan_status=0
	wait "$planner" || plan_status=$?
	kill "$watchdog" 2>/dev/null || true
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
		skill_into
	fi
	if [ -d "$HOME/.agents" ] || [ -d "$HOME/.codex" ]; then
		skill_into --dir "$HOME/.agents/skills"
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

	# change is what this run did to $bin: none, installed (nothing was
	# there), updated or downgraded (a file was there, whatever it answers).
	before=""
	change=none
	[ -x "$bin" ] && before="$("$bin" version 2>/dev/null || true)"
	# Printed below: a version's characters only, never whatever another
	# binary says (spec 054 #15).
	case "$before" in *[!0-9A-Za-z.-]*) before="" ;; esac
	warning=""
	if [ "$before" = "$version" ]; then
		headline="tracepad $version is already installed at $bin"
		verified="unchanged"
	elif [ -z "${TRACEPAD_VERSION:-}" ] && newer "$before" "$version"; then
		# Unpinned is "the newest stable release", never a step back: the
		# database the newer binary ran on may be migrated past this one.
		headline="tracepad $before is installed at $bin, newer than the newest stable release, $version: nothing changed"
		verified="unchanged"
		# Into the directory it speaks of, quoted (the seventh review of #228).
		over="TRACEPAD_VERSION=$version"
		[ -z "${TRACEPAD_INSTALL_DIR:-}" ] || over="$over TRACEPAD_INSTALL_DIR=$(q "$dir")"
		warning="To install $version over it anyway: curl -fsSL $SCRIPT_URL | $over sh"
		version="$before"
	else
		existed=no
		[ ! -e "$bin" ] || existed=yes
		download_and_verify
		put_in_place
		if [ "$existed" = no ]; then
			change=installed
			headline="installed tracepad $version at $bin"
		elif newer "$before" "$version"; then
			change=downgraded
			headline="downgraded tracepad $before → $version at $bin, as TRACEPAD_VERSION asked"
			warning="An older binary does not open a database a newer one migrated: restore the backup taken before that upgrade (https://tracepad.github.io/tracepad/latest/install/#upgrading)."
		else
			change=updated
			headline="updated tracepad ${before:-(a binary that gave no version)} → $version at $bin"
		fi
	fi

	install_skill

	say ""
	say "$headline"
	say "  verified  $verified"
	say "$skill_lines" | sed '1s/^/  skill     /; 2,$s/^/            /'
	if [ -n "$warning" ]; then
		say ""
		say "$warning"
	fi

	case ":$PATH:" in
	*":$dir:"*)
		first="$(command -v tracepad 2>/dev/null || true)"
		if [ -n "$first" ] && [ "$first" != "$bin" ]; then
			say ""
			say "Another tracepad comes first on your PATH: $first. Remove it, or run $(q "$bin")."
		fi
		;;
	*)
		say ""
		say "$dir is not on your PATH. Add it in your shell's profile (~/.zshrc, ~/.bashrc), then open a new terminal:"
		say "  export PATH=$(q "$dir"):\"\$PATH\""
		;;
	esac

	# A server or container started from an older version keeps running it
	# until it is restarted: the binary's plan names each, whose it is, and
	# what to do. After a downgrade, what runs is newer, and the plan says to
	# leave it. Not after a first install: nothing of this binary's runs yet
	# (the agent's bridge removes its binary first, so it is always one, and
	# runs its own plan next).
	plan_status=none
	if [ "$change" != installed ]; then
		plan
		case "$plan_status/$change" in
		10/* | 4/*)
			say ""
			say "Still running an older version:"
			sed 's/^/  /' "$tmp/plan"
			;;
		1/downgraded)
			say ""
			sed 's/^/  /' "$tmp/plan"
			;;
		0/* | 2/*) ;;
		*)
			# Why, in the plan's own first line, or the watchdog's: a new
			# binary's first connection may wait on a firewall or a scan.
			why="it did not finish in 15 seconds"
			[ -e "$tmp/timeout" ] || why="$(head -n 1 "$tmp/plan")"
			say ""
			say "Could not check what still runs an older version (${why:-it said nothing}); to see it: $(q "$bin") upgrade --plan"
			;;
		esac
	fi

	# While something runs older — the plan's 10 or 4 — the agent's part is the
	# upgrade: the server restarted on a backup, the skill's other copies, the
	# project's package (spec 054 #15). Otherwise, an update included, there is
	# nothing of Tracepad's to upgrade, and the next step is a project's setup.
	line="$AGENT_LINE"
	case "$plan_status" in
	10 | 4) line="Update Tracepad to $version: follow $UPGRADE_PAGE" ;;
	esac
	say ""
	say "Next, paste this into your coding agent (Claude Code, Codex, Cursor, …):"
	say ""
	say "  $line"
}

main "$@"
