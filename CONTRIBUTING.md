# Contributing to Tracepad

Thank you for looking. This page is what you need to build the project, change
it, and get the change reviewed. [AGENTS.md](AGENTS.md) is the longer map of the
code, written for coding agents and useful to people too.

## Before you start

- **A bug, a question, a rough edge**: open an
  [issue](https://github.com/tracepad/tracepad/issues/new/choose). A trace body
  or a log line that reproduces it is worth a paragraph of description; remove
  anything private from it first.
- **A vulnerability**: not an issue. See [SECURITY.md](SECURITY.md).
- **A feature**: open an issue and say what you are trying to do before the code
  exists. Tracepad is deliberately small — one binary, one file, sized for a
  team's traces ([README](README.md#what-it-is-built-for)) — and a good idea can
  still be one that belongs elsewhere.

## Building

You need Go (the version in `go.mod`) and, for the web interface, Node 24 —
`make` checks the version and stops at once if it is another. [uv](https://docs.astral.sh/uv/)
is needed for the Python package and for the gate's lint step.

```sh
make build         # the binary with the web interface, into ./bin
make build-server  # without it: no Node needed, serves a stub page
make dev           # run the server locally
make help          # every target
```

## The gate

```sh
make gate
```

is what CI runs and what the pre-push hook runs: formatting, `go vet`, the Go
tests (the server and the Go package), the unit suites of the Python and Node
packages, the anchor check for the docs, the licence-file check, the Python
lint, and the interface's type-check and unit tests. The first `make`
installs the hooks, so a push that would fail CI fails on your machine first.

The Playwright suite drives the real binary in a browser and is heavier:

```sh
make e2e
```

The Python and Node packages have their own suites, run against a real binary
as well: `make sdk-test`, `make sdk-js-test` and `make sdk-go-test`.

While iterating, run what you touched — `go test ./internal/<package>/`,
`npx vitest run <file>` in `ui/`, `npx playwright test <spec>` — and the whole
gate once, before you push.

## Specs first

A feature is written down before it is built. [`specs/`](specs/) holds one file
per feature: the decision log, the contract, and what is out of scope. The
rules:

- A feature needs a spec (`specs/NNN-name.md`); a fix for behaviour the spec
  already describes does not.
- **The spec outranks the code and the tests.** A failing test is a question
  about which one is wrong, not an instruction to make it pass.
- Changing behaviour on purpose means a new dated entry in the spec's
  *Decisions* table — with the reason — and the test updated in the same change.
- **Docs ship in the same pull request** as the behaviour they describe.

Small changes — a typo, a clearer sentence, a fix that matches its spec — need
none of this; just open the pull request.

## Code and commits

- Match the surrounding code: naming, how much it comments, how it handles
  errors. Comments say *why*, and cite the spec decision when there is one.
- Everything public is in English: code, comments, commit messages, docs,
  interface text and error messages.
- Pull requests are squash-merged, so the **title** becomes the commit. Use
  [Conventional Commits](https://www.conventionalcommits.org/): `feat(ui): …`,
  `fix(server): …`, `docs: …`, `refactor(cli): …`, `test: …`.
- One change per pull request; a refactor and a fix are two.
- Stage files by name rather than `git add .`, and read
  `git diff --cached --name-only` before committing.

## Licence, and no CLA

Tracepad is [Apache-2.0](LICENSE). There is no contributor licence agreement
and no sign-off (DCO) requirement: by opening a pull request you agree that your
contribution is licensed under the same terms as the project (Apache-2.0, section 5).

A new dependency needs a reason in the pull request and a licence the project
can carry; `make notices` regenerates `THIRD_PARTY_NOTICES`, and the release
archives ship it.

## Where things live

`cmd/tracepad` the binary · `internal/` the server, store, CLI and MCP ·
`ui/` the SvelteKit interface · `sdk/{python,js,go}` the packages · `docs/` what
users read · `specs/` the decisions · `agent/skills/` the agent skill.
