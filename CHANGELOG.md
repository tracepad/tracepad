# Changelog

All notable changes to Tracepad are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html) — with the
reading of "before 1.0" that the [README](README.md#what-beta-means) spells out.

The server, the CLI and the web interface are one binary and one version. The
Python, Node and Go packages are versioned on their own and are listed under
their own headings.

## [Unreleased]

## [0.1.1] - 2026-10-07

### Fixed

- The links to the documentation that the install script, `tracepad upgrade` and
  the agent skill print pointed at pages under the site's root, which does not
  hold them; they now go to `latest/`, and the gate holds each link's page and
  heading to `docs/`.
- `tracepad upgrade --plan` gives a Compose container whole commands: the
  service stopped through its project and Compose file (`-p`, `-f`), the volume
  Compose really named archived — the old advice, copied, archived an empty
  volume called `tracepad` — the file to edit, and `up -d`. An image pinned by
  digest is said, since Docker keeps the digest whatever the new tag says.
- Every command `tracepad upgrade` and the install script print quotes the
  values it did not choose — names, paths, labels, a version a server
  answered — so pasting one runs nothing but the command.
- The plan lists what is yours and needs nothing apart from what needs you,
  says when the trace counts will not be compared and why, and gives a
  development build at the install path the line that replaces it, exiting
  `4` (yours to do) rather than `0`; a package manager's binary there is
  always given its manager's command.
- The install script's check of what still runs an older version no longer
  goes to the network, which a firewall could hold past its fifteen seconds,
  and says why when it cannot check.
- The agent skill: its shell works in zsh (`path=` emptied `PATH` there, and
  the upgrade's `--to` stayed one word), a failed container start in setup is
  removed so the next try can take the name, it names no stale release
  candidate, it replaces a binary older than itself first, and the release it
  fetches to plan with stays in one directory, which a firewall asks about once.

### Changed

- `tracepad version --json` gives the commit the build was made from beside
  the version, as the log's first line does; `tracepad version` still prints
  the version alone. Any other argument is now a usage error (exit `2`,
  nothing on stdout) rather than ignored.
- `docs/configuration.md` says what reaches the network: the server nothing.

## [0.1.0] - 2026-10-06

The first release, **0.1.0**, is the beta, and everything below ships in it.
From the next release on, each section lists what changed since the previous
one.

### Added

#### Server

- OTLP/HTTP ingest (`/v1/traces`, protobuf and JSON, gzip) and the Langfuse
  SDK's endpoints, into an embedded SQLite database: one binary, one data
  directory, migrations applied on start. Raw export bodies are kept for
  replay.
- The attribute mapping for the OpenTelemetry GenAI conventions, Langfuse's
  and Tracepad's own, with the wire columns filters need (`release`, `version`,
  observation `type`, prompt, time to first token) and media taken out of
  payloads and stored once.
- The read API under `/api/v1` — flat JSON, cursor pagination both ways with an
  optional exact count, byte budgets with truncation markers, full-text search
  over what observations carried, facets — described by `GET /api/v1` and an
  OpenAPI document.
- Scores, prompts (versions, movable labels), datasets, runs and their
  comparison, score configs, annotation queues, per-user rollups and quality
  trends.
- Hourly statistics that outlive the traces they summarize, with token and
  cost breakdowns.
- Projects with scoped keys (`ingest`, `read`, `write`), accounts with email
  and password sign-in, owner/editor/viewer roles, invitations, and an admin
  token for administration.
- Retention windows per project (traces, raw bodies, statistics), an hourly
  sweeper, trace deletion with a dry run, and user-data erasure that reaches
  the raw archive, dataset items and the freed pages. Deleting a project is
  undoable for a week.
- Bounds on what one request may cost (body, spans per request, read
  concurrency and timeout), and rate limiting by source.
- Safe defaults for a bare binary: it listens on `localhost:4318` — both
  loopback addresses — until told otherwise, and warns while it serves plain
  HTTP to other machines. The key its first run prints holds `ingest` alone;
  a key that reads is minted in the interface, never printed to a log.
- Export of the raw archive to any OTLP receiver or to a directory, resumably.
- Response headers that refuse framing and sniffing, and a
  `Content-Security-Policy` on the web interface's page that lets only the
  bundle's own script run — no inline script, no `eval` — and sends requests
  nowhere but home.

#### CLI, MCP and the agent skill

- `tracepad` subcommands over the read API — traces, sessions, stats, scores,
  prompts, datasets, runs, queues, projects, keys, retention, export — that
  print JSON when piped.
- An MCP server at `/mcp` and over stdio (`tracepad mcp`), read-only.
- A skill for coding agents that ships inside the binary
  (`tracepad skills install`), including setting Tracepad up for a project
  from nothing: a local server, a key, the application connected, and the
  first trace read back
  ([docs/agent-setup.md](docs/agent-setup.md)).

#### Web interface

- Traces, sessions, users, stats and dashboard, prompts, scores, evals
  (datasets, runs, comparison), annotation queues and settings, with the
  project in every address and a peek panel over any listing.
- JSON payloads in an editor surface with folding and search, images shown in
  place, and a phone-width layout.

#### Packages

- **Python** (`tracepad` on PyPI), **Node** (`tracepad` on npm) and **Go**
  (`github.com/tracepad/tracepad/sdk/go`): an exporter added to the
  OpenTelemetry provider the application already has, `observe`/generation
  helpers, scores and prompts over the REST API, an eval harness, trace
  deletion, and test helpers for asserting on an application's instrumentation.
  They read `TRACEPAD_URL` and `TRACEPAD_API_KEY`; the earlier name
  `TRACEPAD_HOST` is a deprecated synonym.

#### Distribution

- Release archives for Linux, macOS and Windows on amd64 and arm64 with
  checksums and build-provenance attestations, and a multi-architecture image
  on `ghcr.io/tracepad/tracepad`, mirrored to Docker Hub as
  `tracepad/tracepad` at the same digest for stable releases. See
  [docs/install.md](docs/install.md) and [docs/docker.md](docs/docker.md).
- An install script for Linux and macOS,
  `curl -fsSL https://tracepad.github.io/tracepad/install.sh | sh`, which
  installs the binary and the agent skill into `~/.local/bin` without `sudo`.
  It verifies the checksum, and the attestation too when `gh` can. It
  installs a release candidate only when `TRACEPAD_VERSION` names one.
- `tracepad upgrade`: the binary and a server or container you started, to a
  newer release, with a backup first and a way back that deletes nothing.
  `--plan` changes nothing and says what is yours; the upgrade restarts the
  server with the same arguments and environment, or runs the release in a
  container's place with the `docker run` it was created with, checks it, and
  goes back at once when it exits or answers as another version. A container
  it cannot recreate exactly — Compose's, say — is yours: the plan gives the
  commands that upgrade it. A coding agent does it from one
  line, `Update Tracepad to the latest release: follow
  https://tracepad.github.io/tracepad/agent-upgrade.md`, and the install
  script names what still runs an older version
  ([docs/cli.md](docs/cli.md#upgrade)).
- The server's log opens with its version and commit
  (`tracepad 0.1.0 (b14b11e)`), in a container as on a host, and
  [docs/install.md](docs/install.md#what-a-version-is-called-where) lists what
  one version is called in each registry and how to install a candidate.
- The documentation for agents: `llms.txt`, `llms-full.txt`, and every page as
  Markdown beside its HTML, on the documentation site.

[Unreleased]: https://github.com/tracepad/tracepad/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/tracepad/tracepad/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/tracepad/tracepad/releases/tag/v0.1.0
