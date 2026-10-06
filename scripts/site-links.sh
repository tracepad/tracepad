#!/usr/bin/env bash
# Links into the documentation site that 404 (spec 050): the site's root holds
# the redirect to `latest/`, the Markdown of every page, `install.sh` and the
# `llms*.txt` files — and no HTML page. `https://tracepad.github.io/tracepad/install/`
# is a 404; `…/latest/install/` is the page. A link in code, a script or a doc
# that names an HTML page must carry `latest/`, `dev/` or a `vX.Y/`, name a
# page docs/ has, and an anchor that is one of that page's headings as the site
# gives it an id: the binary and the skill print these links, and a person
# follows them in the middle of an upgrade (spec 050 #19).
#
#   scripts/site-links.sh              check every tracked file (part of the gate)
#   scripts/site-links.sh --self-test  hold the rule to examples
set -euo pipefail

base='tracepad\.github\.io/tracepad'

# slugs: the ids the site gives the headings of one page — Python-Markdown's
# toc rule, which mkdocs.yml leaves at its default, not GitHub's that
# doc-anchors.sh holds docs/ to: a link's text stays, characters other than
# letters, digits, `_`, `-` and spaces go, and a run of spaces and hyphens is
# one hyphen (`A — B` is `a-b` here, `a--b` on GitHub); a repeat gets `_1`,
# `_2`, …. Fenced code is no heading.
slugs() {
	awk '
		function slugify(text, out) {
			out = text
			gsub(/\]\([^)]*\)/, "", out)
			out = tolower(out)
			gsub(/[^-_ \ta-z0-9]/, "", out)
			gsub(/^[ \t]+|[ \t]+$/, "", out)
			gsub(/[- \t]+/, "-", out)
			return out
		}
		/^[ \t]*(```|~~~)/ { fence = !fence; next }
		fence { next }
		/^#{1,6}[ \t]/ {
			text = $0
			sub(/^#+[ \t]+/, "", text)
			sub(/[ \t]+#+[ \t]*$/, "", text)
			slug = slugify(text)
			print (seen[slug]++ ? slug "_" (seen[slug] - 1) : slug)
		}
	' "$1"
}

# bad_links: the offending URLs among the lines on stdin, each with why. A
# page under latest/ or dev/ is a page of docs/ — `latest/docker/` is
# docs/docker.md, `latest/` docs/index.md — and its anchor one of its headings
# (`docs` is the directory, for the self-test's pages). A page under vX.Y/ is
# that version's, which docs/ today does not answer for: its shape is all that
# is checked (the review of #228).
bad_links() {
	local docs="${1:-docs}"
	grep -oE "${base}/[A-Za-z0-9_.#/-]*" | sed "s#^tracepad\.github\.io/tracepad/##" | while IFS= read -r rest; do
		case "$rest" in
		"" | install.sh | llms.txt | llms-full.txt) continue ;;
		*.md | *.md#*)
			[[ "$rest" != */* ]] || echo "tracepad.github.io/tracepad/$rest: no Markdown page lives below the root"
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
		anchor=""
		case "$page" in *"#"*) anchor="${page#*#}" page="${page%%#*}" ;; esac
		page="${page%/}"
		md="$docs/${page:-index}.md"
		if [ ! -f "$md" ]; then
			echo "tracepad.github.io/tracepad/$rest: no page ${page:-index}, since $md is not there"
		elif [ -n "$anchor" ] && ! slugs "$md" | grep -qxF -- "$anchor"; then
			echo "tracepad.github.io/tracepad/$rest: $md has no heading whose id is #$anchor"
		fi
	done
}

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [ "${1:-}" = --self-test ]; then
	fixture=scripts/site-links-fixture
	ok=$(printf '%s\n' \
		'https://tracepad.github.io/tracepad/' \
		'https://tracepad.github.io/tracepad/install.sh' \
		'https://tracepad.github.io/tracepad/agent-setup.md' \
		'https://tracepad.github.io/tracepad/agent-upgrade.md#x' \
		'https://tracepad.github.io/tracepad/llms-full.txt' \
		'https://tracepad.github.io/tracepad/latest/' \
		'https://tracepad.github.io/tracepad/latest/install/#upgrading' \
		'https://tracepad.github.io/tracepad/latest/install/#upgrading_1' \
		'https://tracepad.github.io/tracepad/latest/install/#the-serve-command-and-its-flags' \
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
		'https://tracepad.github.io/tracepad/latest/install/#upgrading_2' \
		'https://tracepad.github.io/tracepad/latest/install/#a-comment-not-a-heading' \
		'https://tracepad.github.io/tracepad/latest/install/#the-serve-command--and-its-flags' \
		'https://tracepad.github.io/tracepad/latest/docker/#upgrading' | bad_links "$fixture" | wc -l | tr -d ' ')
	[ "$bad" = 10 ] || { echo "site-links self-test: found $bad of 10 links the site does not serve" >&2; exit 1; }
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
[ "$status" -ne 0 ] || echo "site-links: every link names a page the site serves, and a heading on it"
exit "$status"
