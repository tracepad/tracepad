#!/usr/bin/env bash
# Links into the documentation site that 404 (spec 050): the site's root holds
# the redirect to `latest/`, the Markdown of every page, `install.sh` and the
# `llms*.txt` files — and no HTML page. `https://tracepad.github.io/tracepad/install/`
# is a 404; `…/latest/install/` is the page. A link in code, a script or a doc
# that names an HTML page must carry `latest/`, `dev/` or a `vX.Y/`, and a page
# docs/ has: the binary and the skill print these links, and a person follows
# them in the middle of an upgrade (spec 050 #19). The anchor of such a link is
# checked against the ids Python-Markdown itself gives the page's headings, by
# scripts/docs-site/site_anchors_test.py: the slug rule is not copied here.
#
#   scripts/site-links.sh              check every tracked file (part of the gate)
#   scripts/site-links.sh --self-test  hold the rule to examples
set -euo pipefail

base='tracepad\.github\.io/tracepad'

# bad_links: the offending URLs among the lines on stdin, each with why. A
# page under latest/ or dev/ is a page of docs/ — `latest/docker/` is
# docs/docker.md, `latest/` docs/index.md (`docs` is the directory, for the
# self-test's pages). A page under vX.Y/ is that version's, which docs/ today
# does not answer for: its shape is all that is checked (the review of #228).
# The period that ends a sentence is not the link's.
bad_links() {
	local docs="${1:-docs}"
	grep -oE "${base}/[A-Za-z0-9_.#/-]*" | sed -e "s#^tracepad\.github\.io/tracepad/##" -e 's/[.]*$//' | while IFS= read -r rest; do
		case "$rest" in
		"" | install.sh | llms.txt | llms-full.txt) continue ;;
		*.md | *.md#*)
			# The root holds the Markdown of every page of docs/, and only
			# those (the sixth review of #228: the install script prints two).
			if [[ "$rest" == */* ]]; then
				echo "tracepad.github.io/tracepad/$rest: no Markdown page lives below the root"
			elif [ ! -f "$docs/${rest%%#*}" ]; then
				echo "tracepad.github.io/tracepad/$rest: $docs/${rest%%#*} is not there"
			fi
			continue
			;;
		latest | dev | latest/* | dev/*) page="${rest#*/}" ;;
		v[0-9]*.[0-9]*/*) continue ;;
		*)
			echo "tracepad.github.io/tracepad/$rest: an HTML page at the site's root, a 404; put latest/ (or dev/, vX.Y/) after /tracepad/"
			continue
			;;
		esac
		[ "$page" != "$rest" ] || page=""
		page="${page%%#*}"
		# A page is a path with no extension; what the build adds beside
		# the pages — assets/, search/, sitemap.xml, a file — is not docs/'s.
		case "$page" in assets/* | search/* | *.*) continue ;; esac
		page="${page%/}"
		md="$docs/${page:-index}.md"
		[ -f "$md" ] || echo "tracepad.github.io/tracepad/$rest: no page ${page:-index}, since $md is not there"
	done
}

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [ "${1:-}" = --self-test ]; then
	fixture=scripts/site-links-fixture
	ok=$(printf '%s\n' \
		'https://tracepad.github.io/tracepad/' \
		'https://tracepad.github.io/tracepad/install.sh' \
		'https://tracepad.github.io/tracepad/install.md' \
		'https://tracepad.github.io/tracepad/docker.md#x' \
		'https://tracepad.github.io/tracepad/llms-full.txt' \
		'https://tracepad.github.io/tracepad/latest/' \
		'https://tracepad.github.io/tracepad/latest/install/#upgrading' \
		'https://tracepad.github.io/tracepad/latest/assets/images/logo.png' \
		'https://tracepad.github.io/tracepad/latest/sitemap.xml' \
		'https://tracepad.github.io/tracepad/latest/search/' \
		'see https://tracepad.github.io/tracepad/latest/install/.' \
		'https://tracepad.github.io/tracepad/latest/docker/#upgrading-and-backing-up-first.' \
		'https://tracepad.github.io/tracepad/dev/' \
		'https://tracepad.github.io/tracepad/v0.1/docker/' \
		'https://tracepad.github.io/tracepad/v0.1/gone/#a-heading-renamed-since' \
		'https://tracepad.github.io/tracepad/latest/docker/#upgrading-and-backing-up-first' | bad_links "$fixture" || true)
	[ -z "$ok" ] || { echo "site-links self-test: refused what the site serves: $ok" >&2; exit 1; }
	bad=$(printf '%s\n' \
		'https://tracepad.github.io/tracepad/install/' \
		'https://tracepad.github.io/tracepad/install/#upgrading' \
		'https://tracepad.github.io/tracepad/docker/#upgrading-and-backing-up-first' \
		'https://tracepad.github.io/tracepad/quickstart' \
		'https://tracepad.github.io/tracepad/docs/page.md' \
		'https://tracepad.github.io/tracepad/latest/quickstart/' \
		'https://tracepad.github.io/tracepad/latest/quickstart/#a-heading' \
		'https://tracepad.github.io/tracepad/dev/gone/.' \
		'https://tracepad.github.io/tracepad/agent-upgade.md' | bad_links "$fixture" | wc -l | tr -d ' ')
	[ "$bad" = 9 ] || { echo "site-links self-test: found $bad of 9 links the site does not serve" >&2; exit 1; }
	echo "site-links: the rule answers as expected"
	exit 0
fi

status=0
while IFS= read -r file; do
	case "$file" in scripts/site-links.sh | scripts/site-links-fixture/*) continue ;; esac
	out="$(bad_links <"$file" || true)"
	if [ -n "$out" ]; then
		echo "site-links: $file links to what the site does not serve:" >&2
		echo "$out" | sort -u | sed 's/^/  /' >&2
		status=1
	fi
done < <(git ls-files | grep -vE '\.(png|jpg|svg|ico|woff2?|gz|zip|pb|db)$')
[ "$status" -ne 0 ] || echo "site-links: every link names a page the site serves"
exit "$status"
