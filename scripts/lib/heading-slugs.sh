# shellcheck shell=bash
# Sourced, not run. The ids the headings of one Markdown file get, in order,
# for the two readers that give them (spec 026 #6, spec 050 #19):
#
#   heading_slugs FILE github   GitHub's: lower-case; everything but letters,
#                               digits, `_`, `-` and spaces dropped; each space
#                               a hyphen (`A — B` is `a--b`); a repeat `-1`, `-2`
#   heading_slugs FILE site     the documentation site's, Python-Markdown's toc
#                               rule as mkdocs.yml leaves it: a link's text
#                               stays, the same characters dropped, the ends
#                               trimmed, a run of spaces and hyphens one hyphen
#                               (`A — B` is `a-b`); a repeat `_1`, `_2`
#
# One parser for both — fences, headings, repeats — so that a fix to one is a
# fix to the other (the third review of #228). Fenced code is no heading:
# `# comment` opens a shell block in half the documents.
heading_slugs() {
	awk -v flavor="$2" '
		function slugify(text, out) {
			out = text
			if (flavor == "site") {
				gsub(/\]\([^)]*\)/, "", out)
			}
			out = tolower(out)
			if (flavor == "site") {
				gsub(/[^-_ \ta-z0-9]/, "", out)
				gsub(/^[ \t]+|[ \t]+$/, "", out)
				gsub(/[- \t]+/, "-", out)
			} else {
				# Everything that is not a letter, a digit, an underscore, a
				# hyphen or a space goes. That is what drops the backticks of
				# a code span, and what leaves the two spaces around an em
				# dash as two hyphens — which is what GitHub produces too.
				gsub(/[^-_ a-z0-9]/, "", out)
				gsub(/ /, "-", out)
			}
			return out
		}
		/^[ \t]*(```|~~~)/ { fence = !fence; next }
		fence { next }
		/^#{1,6}[ \t]/ {
			text = $0
			sub(/^#+[ \t]+/, "", text)
			sub(/[ \t]+#+[ \t]*$/, "", text)
			slug = slugify(text)
			n = seen[slug]++
			print (n ? slug (flavor == "site" ? "_" : "-") n : slug)
		}
	' "$1"
}
