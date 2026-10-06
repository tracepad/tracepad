# Spec 050 — The documentation site: `docs/` as a versioned site on GitHub Pages

**Status:** ✅ SHIPPED
**Sprint:** October 2026

> `docs/` is twenty-four files that read well on GitHub and nowhere else: no
> search, no navigation but the file list, and one version — whatever `main`
> says today, for a person running last month's release. This spec lays the same
> files out as a site, searchable and versioned per release, at
> `https://tracepad.github.io/tracepad/`, without changing what GitHub shows. The
> files stay the source; `mkdocs.yml` is the layout; `mike` keeps one copy of the
> site per release line on the `gh-pages` branch. Nothing here switches Pages on
> or names a domain: that is the day the repository opens, and the owner's.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- **The build** (Decisions 1–5): `mkdocs.yml` (Material), a navigation over the
  existing files and a new `docs/index.md`, `mkdocs build --strict` clean, a
  hook that keeps the four links out of `docs/` working, and the whole of it in
  `make gate`.
- **The versions** (Decisions 6–8): `.github/workflows/site.yml` — `dev` on a
  push to `main` that touches the docs, `vX.Y` and `latest` on a stable server
  release — built with read access and published by a job that builds nothing.
- **The rehearsal** (Decision 9): `make docs-site` deploys what a release
  would onto a throwaway local branch and serves it.

Builds on spec 020 #19 (the supply chain of CI and releases: the build and
publish split, pinned actions, Dependabot) and spec 026 #6 (`doc-anchors`, the
other reader of the anchors in `docs/`).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-10-01** — **Host: GitHub Pages, from the `gh-pages` branch, at the default domain** `https://tracepad.github.io/tracepad/`. A domain of our own is a later, separate step and nothing here names one. Pages is not enabled by this spec: on the free plan a private repository has no Pages, so the owner switches it on (Settings → Pages → *Deploy from a branch* → `gh-pages`, `/ (root)`) on the day the repository opens, after running `site.yml` once by hand (*Run workflow*, `dev`) to create the branch | The site is a build of files already in the repository, so a host that is the repository's own adds no account, no token and no second place to look. A branch rather than the Pages-artifact action because `mike` keeps its versions as directories of a branch, and there is no other shape for "a copy per release" to live in. |
| 2 | **2026-10-01** — **`mkdocs` 1.6.1 with Material 9.7.7, `strict`.** `mkdocs build --strict` fails on any warning, and the validation settings name the three that matter (a link to no file, an anchor on no heading, a page in no navigation) rather than leave them to defaults. The navigation is five groups over the existing files — *Get started*, *Send traces*, *Use it*, *Read it back*, *Operate it* — plus the changelog as an external link. `docs/index.md` is new: a front page that is also a table of contents, which is what the site's root needs and what a person browsing `docs/` on GitHub finds first. mkdocs stays on 1.6: 2.0 is a rewrite that drops the plugin system Material is built on, and Dependabot is told not to move it | A site whose build can warn is a site whose links rot, and the gate already refuses that for `docs/` on GitHub (`doc-anchors`). Both checkers pass on the same 357 anchors: mkdocs' own heading ids and GitHub's agree on every heading that is linked to. Material is the theme with search, a version selector `mike` knows, and a dark mode that follows the system. |
| 3 | **2026-10-01** — **The toolchain is pinned by lock, run through `uv`.** `scripts/docs-site/pyproject.toml` names four exact versions (`mkdocs`, `mkdocs-material`, `mike`, `pymdown-extensions`), `scripts/docs-site/uv.lock` holds the whole set with hashes, and `scripts/docs-site/run.sh` runs a command with `uv run --project … --locked`, so a build never resolves anything and a package whose hash differs is refused. It is the only way the site is built — laptop, gate and workflow. Dependabot's `uv` ecosystem moves the pins and the lock together, grouped weekly like the others, with a seven-day cooldown. `uv` joins the gate's prerequisites for `docs-build`, which it already was for `py-lint` | The shape `sdk/python` already has, so one tool and one idea of "pinned" serve the repository, and Dependabot reads it without a requirements file's `pip-compile` header to guess at. A requirements file with hashes was the alternative; Dependabot's support for `uv pip compile` output is not something to rely on. |
| 4 | **2026-10-01** — **The site asks nothing of a third party to render**: `theme.font: false` (the system fonts, no Google Fonts), and no analytics, comments or social cards. The one request a page makes beyond its own host is Material's read of the repository's public facts (stars, latest release) from `api.github.com` — GitHub's own API, for the link to GitHub in the header | The product's web interface serves its fonts and charts itself (`docs/ui.md`); a documentation site that sent every reader's address to a font CDN would say otherwise. |
| 5 | **2026-10-01** — **`docs/` is not rewritten for the site; the site is built to read `docs/`.** (a) `scripts/docs-site/hooks.py` points the four links that leave `docs/` — `../README.md#getting-it`, `../README.md#what-it-is-built-for`, `../CHANGELOG.md`, `../scripts/smoke` — at the same path on GitHub (`blob/main/…`, `tree/main/…`), for the build only. They stay relative in the files, so GitHub follows them at whatever ref it is showing and `doc-anchors` still reads the README anchors. A link whose target exists nowhere is left alone, so strict mode reports it. (b) Python-Markdown nests a list, a fence or a second paragraph in a list item only at four spaces, and GitHub accepts two or three as well as four. Nine blocks in five files (`accounts.md`, `api.md`, `ingest.md`, `install.md`, `retention.md`) were indented two or three and rendered as a flat list or a fence outside its list item; they are indented four, which GitHub renders the same | A rewrite of the links to absolute URLs would have been simpler and would have lost the checker's coverage of the README anchors, and pinned every doc to `main` on GitHub. An extension for two-space lists would have been one more dependency in the supply chain to avoid editing nine blocks once. The rule for the next author is in CONTRIBUTING: nest at four. |
| 6 | **2026-10-01** — **Versions: `dev`, `vX.Y`, `latest`.** `mike` keeps one directory per version on `gh-pages`. A push to `main` that touches `docs/**`, `mkdocs.yml`, `scripts/docs-site/**` or the workflow deploys `dev`. The server's release workflow calls `site.yml` for `vX.Y` — the line, not the patch: a back-patch replaces its line's pages — with the alias `latest` exactly when `check` moves the image's `latest` (spec 020 #16), so a back-patch to an older line never takes `latest` from the newest. A pre-release deploys nothing; `dev` is where its docs are. Aliases are pages of redirects (`alias_type: redirect`), not symlinks, and every version names `latest` as its canonical page. The root of the site redirects to `latest` once a release has set it and to `dev` until then (`scripts/docs-site/deploy.sh`). The release's docs job needs `check` and `release` and gates nothing: a docs failure turns the run red after the archives and the image are out, which is where it belongs | The version a reader needs is the one they run, and the version selector is how they find it. A symlink in a branch is a thing a host may or may not follow; a redirect cannot be got wrong. Deploying after `release` means there are never docs for a release nobody can download. |
| 7 | **2026-10-01** — **Built where nothing can be published, published where nothing is built** (spec 020 #19(a), applied to the one thing this spec publishes). `site.yml` has two jobs. `build` (`contents: read`) checks out the tag or `main`, installs the locked toolchain, runs `mike deploy` onto a local `gh-pages` fetched from the remote and hands over the commits it made as a **git bundle** — only the new ones — as an artifact. `publish` (`contents: write`, the only job that has it) has no checkout, no toolchain and no install: it makes an empty repository, fetches the branch, verifies the bundle, fetches it and pushes it with no `--force`. A bundle that is not a fast-forward of the branch as it is now — another deploy landed in between — is refused and the run repeated. The token reaches `git` as an `http.extraheader` on the two commands that talk to GitHub, masked, and is in no file. The release workflow's `docs` job carries `contents: write` for the call and nothing else does; inside, `build` is capped back to `read`. The inputs are checked against the shapes the site publishes (`dev` or `vX.Y`, `latest` or nothing) before they touch a path. The workflow-level `concurrency` group serialises deploys, and is not what keeps the branch right | `mike deploy` runs MkDocs, its plugins and Material's templates — Python that, in a job holding a write token, could publish anything. Handing over a bundle rather than a directory keeps `mike`'s layout (version directories, `versions.json`, the root redirect) its own and out of a second implementation in shell. What `publish` runs is `git` and nothing of the project's. |
| 8 | **2026-10-01** — **Where this is verified and where it is not.** Pages is off and CI for the site is not enabled in the private repository, so `site.yml` has not run on GitHub. What was run: `actionlint` over both workflows; the build and publish scripts extracted from the workflow and run against a local bare repository — first deploy (no branch), a release on top (`latest` moved, root redirect), a repeat with nothing changed (no bundle, no publish), and a deploy raced by another push (rejected as a non-fast-forward). The source-selection step (#12) was run in a scratch repository with tags `v0.1.0`, `v0.2.0`, `v0.2.1`, `v0.2.2-rc.1` and `v0.3.0`: `dev` from `main` and from a branch, `v0.2` from `main` (builds `v0.2.1`), from tag `v0.2.1` and from tag `v0.3.0`, a line with no tag, and a tag that predates the site. What was not: `workflow_call` across the two workflows, the token header against GitHub, concurrency in a called workflow. The first run after the flip is the test of those, and `workflow_dispatch` is how to make it | A workflow cannot be rehearsed on a host that is not running it; saying which half was and which was not is the part of the verification that can be done now. |
| 9 | **2026-10-01** — **The rehearsal cannot become the site.** `make docs-site` builds strictly, deploys `dev` and a stand-in `v0.1` with `latest` onto **`gh-pages-rehearsal`** — a local branch, deleted and made again each run — and serves it with `mike serve`, so the version selector and the root redirect are the real ones; `make docs-site-clean` removes the branch and the build output. `scripts/docs-site/deploy.sh` is the one script both the rehearsal and the workflow run, and it never pushes: `DOCS_BRANCH` names the branch, and `gh-pages` is only its default | A rehearsal that wrote `gh-pages` could be one stray `git push --all` from being the published site. The workflow and the laptop sharing one script is what makes the rehearsal evidence about the workflow. |
| 10 | **2026-10-01** — **`docs-build` is in the gate.** `make gate` runs `scripts/docs-site/run.sh mkdocs build --strict`, and with it the pre-push hook and CI's `gate` job: a broken docs build is refused on the pull request, not discovered by a `dev` deploy that fails on `main`. Its first run on a machine installs the toolchain; after that it is a second or two | The failure is invisible in review — a renamed heading, a link to a page left out of the navigation — and is the kind `doc-anchors` was put in the gate for. |
| 11 | **2026-10-01** — **Canonical pages exist only once `latest` does.** Every version names `latest` as its canonical page (`canonical_version`), and before the first stable release `/latest/` is a 404. `mkdocs.yml` reads the option from `DOCS_CANONICAL` (`!ENV`), which `deploy.sh` sets to `latest` when the deploy makes the alias or the branch already has it. Until then Material's own canonical, the page's address, stands | A canonical link to a 404 tells a search engine the page is a duplicate of nothing. Setting it from the deploy needs no edit on the day of the first release, which is the edit that would be forgotten. |
| 12 | **2026-10-01** — **A release's docs are built from its tag, whatever ref the run started on.** Amends #7. `build` checks the ref against the version before it builds: `dev` only from `main`; `vX.Y` from the tag that called the workflow (which must be a stable `vX.Y.Z` on that line) or, for a run started by hand from a branch, from the **newest stable tag of the line**; a hand-run from a tag on another line is refused, and so is a tag that predates `scripts/docs-site/`. A `workflow_dispatch` on `main` with `v0.2` therefore cannot publish `main`'s docs as `v0.2` | The input and the ref were two independent facts and nothing tied them together; the version a reader picks in the selector must be the docs of that release. |
| 13 | **2026-10-01** — **Failures that were silent are not.** Amends #7. `build` takes `gh-pages` from the `origin/gh-pages` the checkout already fetched (`fetch-depth: 0`, with the checkout's credentials) and says, as a notice, when there is none; it no longer fetches again without credentials and reads any failure as "first deploy". `publish` asks `git ls-remote --exit-code`: 2 is a first deploy, any other failure stops the run with git's own message. `deploy.sh` reads `versions.json` into a variable instead of a pipe into `grep -q`, which under `pipefail` handed the `if` a SIGPIPE and sent the root's redirect to `dev` after the first release | A first deploy that is really a failed fetch ends in a non-fast-forward rejection on every run, with a message about the wrong thing. Silence is the one error this workflow cannot afford, because its failures happen on the day nobody is watching. |
| 14 | **2026-10-01** — **Concurrency groups: `dev` and releases are separate.** Amends #7 and the edge case below. A waiting run is replaced by a newer one in its group; `docs-site-dev` and `docs-site-release` keep a push to `main` from replacing a release's deploy that is waiting. A `dev` and a release that overlap can race for the branch, and the loser's `publish` is refused as a non-fast-forward and rerun by hand | One group was serialisation with a hole that dropped the one deploy that cannot be regenerated by the next push. |
| 15 | **2026-10-01** — **The hook is tested and does not touch code.** `hooks.py` skips fenced code and rewrites reference definitions (`[id]: ../README.md`) as well as inline links; `scripts/docs-site/hooks_test.py` asserts both, the directory and file cases, and that a link to nothing is left for strict mode. `make docs-site-test` is in the gate beside `docs-build` | A regex over a Markdown file that does not know where a fence is rewrites an example into a link to GitHub. |
| 16 | **2026-10-01** — **The mark.** `theme.logo` is `assets/mark-on-dark.svg` — the mark without its tile, light strokes drawn for a dark ground — on a black header (`primary: black` in all three palettes), and `theme.favicon` is `assets/favicon.svg`, the mark on its tile. `docs/assets/` holds the four files by their own names (`logo.svg`, `favicon.svg`, `mark-on-light.svg`, `mark-on-dark.svg`); the README uses the same files | The header colour is chosen for the mark rather than the other way round: a light mark on Material's default indigo reads as a different brand. |
| 17 | **2026-10-04** — **What the product says it is: "LLM observability and evals in a single binary."** (owner decision 2026-10-04). (a) The README opens with that line, three paragraphs and a list of six things the product does, and carries two demo GIFs recorded from a synthetic corpus: `docs/assets/demo-ui.gif` (a search, a trace's tree with a generation's usage and cost, two runs compared) under the opening, and `docs/assets/demo-cli.gif` (`tracepad traces last --error --full`, then the same trace as JSON through `jq`) under *Reading traces back*, each under 3 MB. The release archive does not carry them, unlike the two marks of its header (spec 006 #30): they would add 3.7 MB to an archive of about 8 MB to show an interface the binary itself serves. The README links them by absolute URL (`https://github.com/tracepad/tracepad/raw/main/docs/assets/…`), so the archive's copy shows them when there is a network and their alt text when there is not. The alt text says what they show. (b) `docs/index.md` opens with the same description, and `mkdocs.yml`'s `site_description` follows it. (c) Two pages join the site and its navigation: `docs/langfuse-sdk.md`, what the Langfuse SDK's traces carry over and which of its calls go through Tracepad's API instead, and `docs/compare.md`, other tools beside this one, giving what each needs to run, its licence, and where it does more than Tracepad. Every fact there is dated and sourced, and nothing is graded. These two are the only pages that set Tracepad beside other products. (d) The same line is now the summary of `openapi.json`, the opening of the agent skill's description (which keeps its trigger words: traces, spans, debugging, evals, scores, prompts, datasets), the first line of `AGENTS.md`, the first line of `tracepad help`, the Homebrew formula's description, the image's `org.opencontainers.image.description`, the first paragraph of the three packages' READMEs, the PyPI and npm `description` (with more keywords) and the Go package's doc comment. The README's two size figures are dated: the binary of about 20 MB and the 15 MiB an empty server holds at rest were measured on 2026-10-04, and *What it is built for* says which figure dates from when. (e) `docs/assets/social-preview.png` is rendered again from the owner's master drawing with only its subtitle changed, to the same line in two rows. This amends spec 006 #30's "byte for byte" for that one file | "OTLP-native store and viewer" named the transport, which every tool in the category now accepts, and hid half of what ships: prompts, datasets, runs, scores and review queues. The words people search for are "LLM observability" and "evals". "Single binary" is a difference a reader can check, where "lightweight" is not. The comparison is dated because the products change monthly, and a stale cell should read as an issue to open rather than as a claim. It says where other tools are ahead (built-in judges, online evaluation, a playground, alerts, SSO, scale, a cloud) because a page that only wins persuades nobody. The GIFs are recorded from a synthetic corpus on a copy of the demo database and checked, one frame a second, for a real prompt, path or account before they are committed. |
| 18 | **2026-10-06** — **A link to the site's root does not name an HTML page.** The root of `tracepad.github.io/tracepad/` holds the redirect to `latest/`, the Markdown of every page (`agent-setup.md`), `install.sh` and the `llms*.txt` files; the HTML pages are under `latest/`, `dev/` and `vX.Y/`. So `…/tracepad/install/` and `…/tracepad/docker/` are 404s, and four places linked to them (the install script's two, the upgrade plan's two doc links and the setup reference of the skill — all now `…/latest/install/…` and `…/latest/docker/…`). `scripts/site-links.sh`, in the gate (`make site-links`, with its own self-test), fails on any tracked file that names a root URL that is not one of those. | Found after v0.1.0: the links the binary prints in its refusals and the skill hands an agent went to a page that did not exist, and nothing could have noticed, since a link is only text until someone follows it. The Markdown pages and `install.sh` at the root are deliberate (they are what an agent fetches), so the check allows them and nothing else. |

## Application contract

- `mkdocs.yml`: site, theme, `strict`, the validation levels, the `mike`
  plugin (`alias_type: redirect`, `canonical_version: latest`), the hook and
  the navigation. Every file in `docs/` is in it; a new one that is not fails
  the build (`omitted_files`).
- `docs/index.md`: the front page; a new document is added to its list and to
  the navigation in the same change.
- `scripts/docs-site/`: `pyproject.toml` and `uv.lock` (Decision 3), `run.sh`
  (the one way to run the toolchain), `deploy.sh` (one version onto a branch,
  and the root's redirect), `hooks.py` and its test (Decisions 5a, 15).
- `.github/workflows/site.yml` (Decisions 6, 7), and the `docs` job and the
  `line` output of `check` in `release-server.yml`.
- `Makefile`: `docs-build` and `docs-site-test` (in the gate), `docs-site`,
  `docs-site-clean`.
- `docs/assets/`: the mark, the logo and the favicon (#16); the social preview and the two demo GIFs (#17).
- `.dockerignore`: `site/` and `scripts/docs-site/.venv/`, which `COPY . .` would
  carry into the image's context.
- `.github/dependabot.yml`: the `uv` entry for `/scripts/docs-site`; amends
  spec 020 #19(f).

## Testing

- **The build is the test of the docs**: `make docs-build` is `mkdocs build
  --strict` over every page, so a link to nothing, an anchor on no heading and
  a page in no navigation each fail it. `make doc-anchors` still reads the same
  files by GitHub's slugs; the two agree on all 357 anchors.
- **The deploy, locally** (Decisions 6, 9): `make docs-site` — `versions.json`
  holds `dev` and `v0.1` with `latest`, the root redirects to `latest/`, the
  version selector shows, a page's search and its table of contents work.
- **The hook** (#15): `make docs-site-test`.
- **The publish path, against a bare repository** (Decision 8): first deploy,
  a release on top, a repeat that changes nothing, a raced deploy.
- **After the flip**: `workflow_dispatch` with `dev`, then the first release
  tag. Nothing before that proves the workflow on GitHub.

## Edge cases

- **A docs-only push to `main` while a release's docs are deploying**: the two
  are in different groups (#14), so neither replaces the other; if they race
  for the branch the loser is refused as a non-fast-forward and rerun with
  `workflow_dispatch` (`vX.Y`, `latest` — built from the line's newest stable
  tag, #12). Two pushes to `main` in a row: the second waits, and a third
  replaces it, which loses nothing.
- **Nothing changed**: `mike` commits nothing, `build` reports it and `publish`
  does not run.
- **A first deploy**: no `gh-pages` to fetch; `build` starts the branch and the
  bundle holds all of it.
- **Two releases of one line**: the second replaces `vX.Y`. The docs of `v0.2.0`
  are not kept once `v0.2.1` ships, which is what "the line" means.
- **A link from `docs/` into the repository that the hook does not know**
  (anything but one `../` to an existing file or directory) is a strict-mode
  warning, which is the intent: it is a link that works on GitHub and nowhere
  else.
- **`edit_uri` and `blob/main` links name `main`**: on a version's page they
  open the file as it is now, not as it was.

## Docs to touch

- `docs/index.md` (new) and `mkdocs.yml`.
- `docs/accounts.md`, `api.md`, `ingest.md`, `install.md`, `retention.md`: the
  nine blocks of Decision 5(b), indentation only.
- `README.md`: the site, beside the pointer to `docs/quickstart.md`.
- `CONTRIBUTING.md`: building and rehearsing the docs, and the indentation rule.
- `AGENTS.md`: status, *Where things are*, *Commands*, and *Releasing* (what
  the release's docs job does and the owner's flip-day steps).

## Out of scope

- A custom domain and its DNS; enabling Pages. Both are the owner's, on the day
  the repository opens.
- Generated reference (an API reference from `openapi.json`, the CLI's help):
  `docs/api.md` and `docs/cli.md` are the reference, by hand.
- Old versions' docs from before this spec: the first release is the first
  version.
- Search beyond the theme's: no Algolia, no hosted index (Decision 4).
- Translations; a docs-only license page; analytics.
- A required check on the docs deploy: it runs after merge and after release,
  and a red run is the signal.
