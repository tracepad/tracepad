#!/usr/bin/env bash
# Links into the documentation site that 404 (spec 050): the site's root holds
# the redirect to `latest/`, the Markdown of every page, `install.sh` and the
# `llms*.txt` files — and no HTML page. `https://tracepad.github.io/tracepad/install/`
# is a 404; `…/latest/install/` is the page. A link in code, a script or a doc
# that names an HTML page must carry `latest/`, `dev/` or a `vX.Y/`.
#
#   scripts/site-links.sh              check every tracked file (part of the gate)
#   scripts/site-links.sh --self-test  hold the rule to examples
set -euo pipefail

base='tracepad\.github\.io/tracepad'

# bad_links: the offending URLs among the lines on stdin.
bad_links() {
	grep -oE "${base}/[A-Za-z0-9_.#/-]*" | sed "s#^tracepad\.github\.io/tracepad/##" | while IFS= read -r rest; do
		case "$rest" in
		"" | install.sh | llms.txt | llms-full.txt) ;;
		*.md | *.md#*) [[ "$rest" != */* ]] || echo "tracepad.github.io/tracepad/$rest" ;;
		latest/* | dev/* | latest | dev) ;;
		v[0-9]*.[0-9]*/*) ;;
		*) echo "tracepad.github.io/tracepad/$rest" ;;
		esac
	done
}

if [ "${1:-}" = --self-test ]; then
	ok=$(printf '%s\n' \
		'https://tracepad.github.io/tracepad/' \
		'https://tracepad.github.io/tracepad/install.sh' \
		'https://tracepad.github.io/tracepad/agent-setup.md' \
		'https://tracepad.github.io/tracepad/agent-upgrade.md#x' \
		'https://tracepad.github.io/tracepad/llms-full.txt' \
		'https://tracepad.github.io/tracepad/latest/install/#upgrading' \
		'https://tracepad.github.io/tracepad/dev/' \
		'https://tracepad.github.io/tracepad/v0.1/docker/' | bad_links || true)
	[ -z "$ok" ] || { echo "site-links self-test: refused what the site serves: $ok" >&2; exit 1; }
	bad=$(printf '%s\n' \
		'https://tracepad.github.io/tracepad/install/' \
		'https://tracepad.github.io/tracepad/install/#upgrading' \
		'https://tracepad.github.io/tracepad/docker/#upgrading-and-backing-up-first' \
		'https://tracepad.github.io/tracepad/quickstart' \
		'https://tracepad.github.io/tracepad/docs/page.md' | bad_links | wc -l | tr -d ' ')
	[ "$bad" = 5 ] || { echo "site-links self-test: found $bad of 5 root HTML links" >&2; exit 1; }
	echo "site-links: the rule answers as expected"
	exit 0
fi

cd "$(dirname "${BASH_SOURCE[0]}")/.."
status=0
while IFS= read -r file; do
	case "$file" in scripts/site-links.sh) continue ;; esac
	out="$(bad_links <"$file" || true)"
	if [ -n "$out" ]; then
		echo "site-links: $file links to an HTML page at the site's root, which is a 404; put latest/ (or dev/, vX.Y/) after /tracepad/:" >&2
		echo "$out" | sort -u | sed 's/^/  /' >&2
		status=1
	fi
done < <(git ls-files | grep -vE '\.(png|jpg|svg|ico|woff2?|gz|zip|pb|db)$')
[ "$status" -ne 0 ] || echo "site-links: no link names an HTML page at the site's root"
exit "$status"
