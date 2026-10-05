#!/usr/bin/env bash
# Puts one version of the site on a branch — and only on a branch, never past
# this checkout (spec 050 #2). `mike deploy` builds the documentation and
# commits it under a version directory; this script adds the one rule mike
# leaves open: what the root of the site redirects to. Publishing is a
# different act, done by a different job that holds the write scope and no
# toolchain (.github/workflows/site.yml).
#
#   scripts/docs-site/deploy.sh dev            the tip of main
#   scripts/docs-site/deploy.sh v0.2 latest    a release, and the newest one
#   scripts/docs-site/deploy.sh v0.1.5         a back-patch: its own line only
#
# DOCS_BRANCH names the branch (default gh-pages). `make docs-site` points it
# at a throwaway one, so a rehearsal can never be the thing that is published.
set -euo pipefail

version="${1:?usage: deploy.sh <version> [alias]}"
alias="${2:-}"
branch="${DOCS_BRANCH:-gh-pages}"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
run="$ROOT/scripts/docs-site/run.sh"

# `latest` is the canonical page of every version (mkdocs.yml), which is only
# an address that exists once a release has made it: this deploy's own, or one
# on the branch already. Before that no page names a canonical version, rather
# than one that is a 404.
versions="$(git show "$branch:versions.json" 2>/dev/null || true)"
if [ "$alias" = latest ] || grep -q '"latest"' <<<"$versions"; then
	export DOCS_CANONICAL=latest
fi

# The title is the version as the selector shows it. A release is `vX.Y`:
# the patch is not part of what the docs are versioned by, so a back-patch
# replaces its line's pages and the next patch does too.
"$run" mike deploy --branch "$branch" --update-aliases --title "$version" \
	--message "docs: $version${alias:+ ($alias)}" "$version" ${alias:+"$alias"}

# The root follows `latest` once a release has taken it and `dev` until then,
# so a site with no release has a front page and one with a release shows
# what a user can install. The branch is read again, into a variable and not
# through a pipe: under `pipefail` a `grep -q` that stops reading early hands
# the `if` the SIGPIPE of `git show`, which reads as "no `latest`".
versions="$(git show "$branch:versions.json" 2>/dev/null || true)"
if grep -q '"latest"' <<<"$versions"; then
	default=latest
else
	default=dev
fi
"$run" mike set-default --branch "$branch" "$default"

# The root also serves what agents fetch (spec 053 #3, #14): install.sh,
# llms.txt, llms-full.txt and every page's Markdown, copied from the version
# the root redirects to. mike's redirect alias holds HTML alone, so `latest/`
# has none of them; the copy is made from the version `latest` names. It is a
# commit of its own on the branch, made with git's plumbing and no checkout:
# root files of those kinds that the version no longer has are removed, the
# rest point at the version's own blobs. A deploy that changes none of them
# commits nothing.
versions="$(git show "$branch:versions.json")"
source="$(VERSIONS="$versions" DEFAULT="$default" "$run" python -c '
import json, os
want = os.environ["DEFAULT"]
for v in json.loads(os.environ["VERSIONS"]):
    if v["version"] == want or want in v["aliases"]:
        print(v["version"])
        break
')"
[ -n "$source" ] || { echo "docs-site: no version on $branch is $default" >&2; exit 1; }
index="$(mktemp)"
trap 'rm -f "$index"' EXIT
export GIT_INDEX_FILE="$index"
git read-tree "$branch"
git ls-files | awk '!/\// && (/\.md$/ || $0 == "install.sh" || $0 == "llms.txt" || $0 == "llms-full.txt")' |
	while IFS= read -r file; do git update-index --force-remove -- "$file"; done
git ls-tree "$branch" "$source/" | awk -F '\t' '
	{ split($1, meta, " "); name = substr($2, index($2, "/") + 1) }
	meta[2] == "blob" && (name ~ /\.md$/ || name == "install.sh" || name == "llms.txt" || name == "llms-full.txt") {
		print meta[1] "," meta[3] "," name
	}' |
	while IFS= read -r entry; do git update-index --add --cacheinfo "$entry"; done
tree="$(git write-tree)"
unset GIT_INDEX_FILE
if [ "$tree" != "$(git rev-parse "$branch^{tree}")" ]; then
	commit="$(git commit-tree "$tree" -p "$branch" -m "docs: the root's files for agents, from $source")"
	git update-ref "refs/heads/$branch" "$commit"
fi
echo "docs-site: $version${alias:+ ($alias)} is on $branch; the root goes to $default, and serves $source's files for agents"
