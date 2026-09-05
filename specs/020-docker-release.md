# Spec 020 — Packaging: the Docker image and the release workflow

**Status:** 🚧 IN PROGRESS
**Sprint:** September 2026

> The docs have promised a Docker image since spec 006 — "every official
> artifact carries the web interface: the release binaries, Homebrew and
> the Docker image" (`docs/ui.md`), "`/data` in the Docker image"
> (`docs/quickstart.md`) — and `.goreleaser.yaml` has been ready for a
> release workflow that does not exist. Design §12 calls this the last
> mile. This spec builds it: a Dockerfile anyone can build from a checkout,
> a workflow that publishes it beside the release binaries on a version
> tag, and the docs that stop describing something that is not there.

---

## Overview

Deliverable, in one PR (which flips the status):

- **`Dockerfile`** at the repository root — multi-stage, reproducible from
  a checkout with `docker build .`, producing a distroless image of the
  `ui`-tagged binary that runs as non-root with `/data` as its volume
  (Decisions 1, 2, 3).
- **`tracepad health`** — a CLI command for the container's `HEALTHCHECK`
  and for anyone else who wants a liveness probe without keys
  (Decision 4).
- **`release-server.yml`** — on a `v*` tag: GoReleaser publishes the
  archives and checksums to GitHub Releases, and a second job builds the
  Dockerfile for `linux/amd64` and `linux/arm64` and pushes one multi-arch
  manifest to `ghcr.io/tracepad/tracepad` (Decisions 5, 6).
- **A `docker` job in `ci.yml`** — builds the image on every push and
  proves it boots (Decision 7).
- `docs/docker.md` (new), the Docker variant in `docs/quickstart.md`'s
  first step, `README.md`'s install lines, `docs/ui.md`'s promise made
  true, AGENTS.md.

Not here: the Homebrew tap (a second repository — a later decision of the
owner's), Windows containers, a Helm chart, a
`docker-compose.yml` beyond the example in the docs, image signing.

---

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-05** — The `Dockerfile` is **multi-stage and self-contained**: a Node stage (`node:22-alpine`, the `engines` floor of `ui/package.json`) runs `npm ci` and builds the bundle; a Go stage (`golang:1.27-alpine`, the `go.mod` toolchain) stages the bundle where the `ui` tag embeds it and builds `./cmd/tracepad` with `CGO_ENABLED=0`, `-tags ui`, `-ldflags "-s -w -X main.version=<version>"` from a `VERSION` build argument (default `dev`); the final stage copies one binary. The release binaries and the image are built from the **same sources by the same flags** but not from the same bytes: GoReleaser builds its own, the Dockerfile builds its own | An image that copies a binary GoReleaser built is an image only the release workflow can build; a contributor, a fork, an operator who wants a patch cannot `docker build .` and get the artifact the docs describe. Self-contained means the Dockerfile is the whole recipe, and the recipe is the same one `make build` follows (`Makefile:18-19`): the two stages are `make ui` and `go build -tags ui`. The bytes differ because the build stamps differ; the flags, the tag and the version string do not, so a binary from either path answers `/health` with the same version. |
| 2 | **2026-09-05** — The runtime stage is **`gcr.io/distroless/static-debian12:nonroot`**: no shell, no package manager, `nonroot` (uid 65532) as the user, `tzdata` and CA certificates present. The binary is `/tracepad`, the entrypoint; `TRACEPAD_DATA_DIR=/data` and `TRACEPAD_LISTEN=:4318` are set in the image; `VOLUME /data`; `EXPOSE 4318`. The `/data` directory is created in the image owned by `nonroot` | The binary is static (`modernc.org/sqlite`, `CGO_ENABLED=0` — `.goreleaser.yaml:17`), so the image needs nothing of an OS but the two things a Go program reads from it: zone data for `time.LoadLocation` and roots for the one outbound TLS call the CLI can make. Distroless static is the smallest image that has both and a non-root passwd entry; `scratch` would have neither, `alpine` would have a shell nobody should be able to exec into a trace store. Non-root by default because the store holds every prompt the application ever sent, and a container that runs as root writes `/data` as root, which is the bind-mount permission problem every operator hits once. The environment defaults make `docker run -v tracepad:/data -p 4318:4318 ghcr.io/tracepad/tracepad` the whole command. |
| 3 | **2026-09-05** — The first-run output — the `default` project's keys, printed once (`docs/quickstart.md`) — is printed to the container's **stdout exactly as it is on a host**, and `docs/docker.md` says to read it with `docker logs` and that the keys are not printed again. Nothing about key creation changes for the container | A container's stdout is its terminal; inventing a file under `/data` for the keys would put a secret on a volume that outlives the container and would be a second way to learn a key that the host install does not have. `docker logs` is one command, and the docs say it in the first ten lines. |
| 4 | **2026-09-05** — `tracepad health [--server URL]` performs `GET /health` and exits `0` on a `200` whose body carries a `version`, `1` otherwise, printing the version on success and the failure on stderr; **no key is needed**, and `--server` defaults as every command's does. The Dockerfile declares `HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/tracepad", "health"]` | Distroless has no `curl`, `wget` or shell, so a `HEALTHCHECK` can only be the binary; the binary already has the client and the route is the one unauthenticated one (`internal/server/routes.go:23`). The command is also the probe an operator writes into a systemd unit or a load balancer without pasting a key into it. Exit codes follow `docs/cli.md`'s table. |
| 5 | **2026-09-05** — `release-server.yml` runs on tags matching `v*`: job `release` runs GoReleaser (`goreleaser/goreleaser-action`, the committed `.goreleaser.yaml`) with the repository token, publishing archives, checksums and the changelog to GitHub Releases; job `image` (needs `release`) runs `docker/setup-buildx-action` and `docker/build-push-action` with `platforms: linux/amd64,linux/arm64`, `build-args: VERSION=<tag without v>`, pushing `ghcr.io/tracepad/tracepad:<X.Y.Z>`, `:<X.Y>` and `:latest`, authenticated to GHCR with `GITHUB_TOKEN` (`packages: write`). The image is not built by GoReleaser | The release pipeline was always planned as this split — GoReleaser for GitHub Releases, a GHCR image beside it — and the split keeps each tool on the thing it is good at: GoReleaser owns the six archives and their checksums, `buildx` owns a multi-arch manifest from one Dockerfile. GoReleaser's own Docker support wants a prebuilt binary per architecture copied into a per-arch Dockerfile, which is Decision 1's rejected shape. Three tags because `latest` is what the quickstart says and `X.Y` is what an operator pins to when they want patches but not surprises. |
| 6 | **2026-09-05** — The image's version is the tag's; a locally built image reports `dev` unless `--build-arg VERSION=` says otherwise; `ghcr.io/tracepad/tracepad:latest` is moved only by a release, never by a push to `main`. Pre-release tags (`v0.2.0-rc.1`) publish `:<X.Y.Z-rc.1>` only — no `:<X.Y>`, no `:latest` | An image tagged `latest` that a `main` push moves is a deploy nobody asked for; the release tag is the one act the owner performs deliberately, and the image follows it. A pre-release is for the person who asked for it. |
| 7 | **2026-09-05** — `ci.yml` gains a `docker` job: `docker build` for the runner's architecture with `VERSION=ci`, then `docker run` the image on an ephemeral volume, wait for `tracepad health` against the published port to pass (the container's own `HEALTHCHECK` is what the wait reads: `docker inspect --format '{{.State.Health.Status}}'`), assert `GET /health` reports `ci`, assert the process runs as uid 65532, stop. No push. The job is a required check like the others | A Dockerfile that is not built on every push rots the day a stage's base image or a Makefile target moves; the build alone is not the proof — an image that builds and exits on start because `/data` is not writable is the failure this job is for, and it is the one a release would otherwise be the first to find. Reading the container's `HEALTHCHECK` rather than curling from the runner tests Decision 4 in the place it will be used. |
| 8 | **2026-09-05** — `.dockerignore` excludes everything the build does not read: `.git`, `bin/`, `ui/node_modules`, `ui/build`, `testdata/`, `scratch`, `specs/`, `docs/`, `sdk/`, `.github/`. The build context is the Go sources, `ui/` sources, `go.mod`/`go.sum`, `Makefile`, `LICENSE`, `NOTICE`, `third_party/` | `internal/ui` embeds the bundle, so the Go stage needs `ui/`'s *output*, not its `node_modules`; sending a checkout's `node_modules` and `testdata` to the daemon is the difference between a two-second and a two-minute context upload. `LICENSE`, `NOTICE` and `third_party/` travel into the image under `/usr/share/doc/tracepad/` because Apache-2.0 §4(d) applies to a container image as it does to an archive (`.goreleaser.yaml`'s comment). |

---

## Image contract

| | |
|---|---|
| Name | `ghcr.io/tracepad/tracepad` |
| Tags | `X.Y.Z`, `X.Y`, `latest` on a release; `X.Y.Z-<pre>` alone on a pre-release |
| Platforms | `linux/amd64`, `linux/arm64` |
| User | `nonroot` (65532:65532) |
| Entrypoint | `["/tracepad"]`; `CMD []` — arguments are the server's flags |
| Port | `4318` |
| Volume | `/data` (`TRACEPAD_DATA_DIR`) |
| Environment set in the image | `TRACEPAD_DATA_DIR=/data`, `TRACEPAD_LISTEN=:4318` |
| Health | `HEALTHCHECK` via `/tracepad health` (Decision 4) |
| Labels | `org.opencontainers.image.{source,version,revision,licenses,title,description}` |
| Licence files | `/usr/share/doc/tracepad/{LICENSE,NOTICE}`, `third_party/` beneath it |

Every other `TRACEPAD_*` variable (`cmd/tracepad/main.go`'s usage) is the
operator's to pass with `-e`. The CLI inside the image works against the
server with `docker exec … /tracepad <command> --server http://localhost:4318`
— the docs show it for `keys new`.

## CLI contract

```
tracepad health [--server URL] [--json]
```

Exit `0` and the version on stdout for a `200`; `1` and the reason on
stderr for anything else (connection refused, a non-200, a body without
`version`). `--json`: `{"version": "…", "ok": true}`. Listed in
`docs/cli.md` under *Administration*, covered by the usage parity test.

## Workflow contract

`release-server.yml`: `on: push: tags: ['v*']`; permissions `contents:
write`, `packages: write`; job `release` (Go from `go.mod`, Node from
`ui/package.json`, GoReleaser v2 with `--clean`); job `image` after it.
Nothing in it runs on `main`.

`ci.yml`: job `docker` as Decision 7, alongside `gate`, `e2e`, `smoke`,
`sdk`.

---

## Testing

- **Go**: `tracepad health` against a test server (`200` → exit 0 and the
  version; a `500` → exit 1; a refused connection → exit 1 with the
  address in the message; `--json` shape); the usage parity test.
- **The `docker` CI job** is the image's test (Decision 7); it also asserts
  the licence files are present in the image and that `docker run --rm
  ghcr… health --server http://127.0.0.1:1` exits 1 (the probe fails
  honestly).
- **Local**: `make image` builds the Dockerfile with `VERSION=$(VERSION)`
  and tags `tracepad:dev`; `make image-check` runs Decision 7's sequence
  locally. Both documented in AGENTS.md's Commands.
- **Mutation** for the Go part; the image contract by the CI job's
  assertions, each of which must fail on the corresponding Dockerfile edit
  (`USER` removed → the uid assertion; `HEALTHCHECK` removed → the wait
  never reaches `healthy`; `ENV TRACEPAD_DATA_DIR` removed → the boot
  fails on `~/.local/share` under `nonroot`). The PR shows each.
- **The release workflow** cannot run before the first tag; the PR
  includes `act`-free validation: `goreleaser check`, `actionlint` on both
  workflows, and a dry `docker buildx build --platform linux/amd64,linux/arm64`
  without push in the `docker` CI job once (a build-only step, not the
  boot test, which stays single-arch).

**Measurements in the PR**: the image size (compressed and on disk) per
architecture; the build time cold and warm on the CI runner; the binary's
`/health` version string from a `VERSION=` build.

---

## Edge cases

- **`/data` bind-mounted from a host directory owned by root**: the
  server fails to create its database and exits with the path in the
  message; `docs/docker.md` shows `chown 65532:65532` and the named-volume
  alternative first.
- **`--user` overridden to root by the operator**: works; the docs say why
  they should not.
- **The server started with `--listen 127.0.0.1:4318`** inside the
  container is unreachable from outside; the docs name it as the mistake
  it is (`TRACEPAD_LISTEN=:4318` is the image default for this reason).
- **Upgrading the image** across a schema migration: the migration runs on
  start as it does on a host (spec 001); the docs say to back up the
  volume first and how (`docker run --rm -v tracepad:/data -v $PWD:/backup
  … tar`), and that a downgrade is not supported.
- **`TRACEPAD_ADMIN_TOKEN`** passed via `-e` is visible in `docker inspect`;
  the docs recommend `--env-file` or a secret, as for any container.
- **A release tag pushed while `main`'s CI is red**: the workflow runs
  anyway (tags are the owner's act); the docs for maintainers (AGENTS.md)
  say to tag only a green commit.
- **GHCR package visibility**: the first push creates the package
  private; making it public is a one-time act in the GitHub UI the owner
  performs before the beta (noted in AGENTS.md's release checklist).

---

## Config additions

None on the server. The image sets two existing variables (Decision 2).

---

## Out of scope

- The Homebrew tap (needs a repository; owner's decision).
- Image signing (cosign), SBOMs, provenance attestations — a later spec
  once the image is public.
- A Helm chart or a Kubernetes manifest; `docs/docker.md`'s compose
  example is the extent of orchestration guidance.
- Running the CLI or MCP from the image against a remote server as a
  documented use; it works, it is not the image's purpose.
