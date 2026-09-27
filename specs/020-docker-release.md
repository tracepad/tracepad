# Spec 020 — Packaging: the Docker image and the release workflow

**Status:** ✅ SHIPPED
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
| 4 | **2026-09-05** — `tracepad health [--url URL]` performs `GET /health` and exits `0` on a `200` whose body carries a `version`, `1` otherwise, printing the version on success and the failure on stderr; **no key is needed**, and `--url` defaults as every command's does (#9). The Dockerfile declares `HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/tracepad", "health"]` | Distroless has no `curl`, `wget` or shell, so a `HEALTHCHECK` can only be the binary; the binary already has the client and the route is the one unauthenticated one (`internal/server/routes.go:23`). The command is also the probe an operator writes into a systemd unit or a load balancer without pasting a key into it. Exit codes follow `docs/cli.md`'s table. |
| 5 | **2026-09-05** — `release-server.yml` runs on tags matching `v*`: job `release` runs GoReleaser (`goreleaser/goreleaser-action`, the committed `.goreleaser.yaml`) with the repository token, publishing archives, checksums and the changelog to GitHub Releases; job `image` (needs `release`) runs `docker/setup-buildx-action` and `docker/build-push-action` with `platforms: linux/amd64,linux/arm64`, `build-args: VERSION=<tag without v>`, pushing `ghcr.io/tracepad/tracepad:<X.Y.Z>`, `:<X.Y>` and `:latest`, authenticated to GHCR with `GITHUB_TOKEN` (`packages: write`). The image is not built by GoReleaser | The release pipeline was always planned as this split — GoReleaser for GitHub Releases, a GHCR image beside it — and the split keeps each tool on the thing it is good at: GoReleaser owns the six archives and their checksums, `buildx` owns a multi-arch manifest from one Dockerfile. GoReleaser's own Docker support wants a prebuilt binary per architecture copied into a per-arch Dockerfile, which is Decision 1's rejected shape. Three tags because `latest` is what the quickstart says and `X.Y` is what an operator pins to when they want patches but not surprises. |
| 6 | **2026-09-05** — The image's version is the tag's; a locally built image reports `dev` unless `--build-arg VERSION=` says otherwise; `ghcr.io/tracepad/tracepad:latest` is moved only by a release, never by a push to `main`. Pre-release tags (`v0.2.0-rc.1`) publish `:<X.Y.Z-rc.1>` only — no `:<X.Y>`, no `:latest` | An image tagged `latest` that a `main` push moves is a deploy nobody asked for; the release tag is the one act the owner performs deliberately, and the image follows it. A pre-release is for the person who asked for it. |
| 7 | **2026-09-05** — `ci.yml` gains a `docker` job: `docker build` for the runner's architecture with `VERSION=ci`, then `docker run` the image on an ephemeral volume, wait for `tracepad health` against the published port to pass (the container's own `HEALTHCHECK` is what the wait reads: `docker inspect --format '{{.State.Health.Status}}'`), assert `GET /health` reports `ci`, assert the process runs as uid 65532, stop. No push. The job is a required check like the others | A Dockerfile that is not built on every push rots the day a stage's base image or a Makefile target moves; the build alone is not the proof — an image that builds and exits on start because `/data` is not writable is the failure this job is for, and it is the one a release would otherwise be the first to find. Reading the container's `HEALTHCHECK` rather than curling from the runner tests Decision 4 in the place it will be used. |
| 8 | **2026-09-05** — `.dockerignore` excludes everything the build does not read: `.git`, `bin/`, `ui/node_modules`, `ui/build`, `testdata/`, `scratch`, `specs/`, `docs/`, `sdk/`, `.github/`. The build context is the Go sources, `ui/` sources, `go.mod`/`go.sum`, `Makefile`, `LICENSE`, `NOTICE`, `third_party/` | `internal/ui` embeds the bundle, so the Go stage needs `ui/`'s *output*, not its `node_modules`; sending a checkout's `node_modules` and `testdata` to the daemon is the difference between a two-second and a two-minute context upload. `LICENSE`, `NOTICE` and `third_party/` travel into the image under `/usr/share/doc/tracepad/` because Apache-2.0 §4(d) applies to a container image as it does to an archive (`.goreleaser.yaml`'s comment). |
| 9 | **2026-09-05** — The connection flag is **`--url`**, as on every other command; #4, the CLI contract and Testing said `--server`, which was a drafting error and is corrected above | There is one connection flag on this command line and it is `--url` (`internal/cli/cli.go`, `TRACEPAD_URL`, `docs/cli.md`'s *Connecting*). #4's own rationale asked for the flag "as every command's", so this is what it was asking for; a second spelling — or an alias for one — would be a name the usage parity test and the docs would then have to keep in step for no gain. |
| 10 | **2026-09-05** — Both builder stages run on `--platform=$BUILDPLATFORM` and cross-compile to `$TARGETOS/$TARGETARCH`; the release workflow therefore sets up no QEMU | `CGO_ENABLED=0` is what makes cross-compiling exact rather than a compromise (#1), and the bundle is static files with no architecture at all — so under `--platform linux/amd64,linux/arm64` the Node stage runs once instead of twice, and neither Go build runs under emulation. Emulating a Go toolchain for arm64 turns a one-minute build into a fifteen-minute one, and the runtime stage only copies, so nothing in the whole build ever executes a foreign binary. |
| 11 | **2026-09-05** — `.dockerignore` excludes `ui/dist` and `internal/ui/dist`, which are what this tree's build outputs are called; #8 said `ui/build` | `ui/build` is SvelteKit's default name and not this project's: `make ui` builds `ui/dist` and stages it at `internal/ui/dist` (`Makefile`). Both are excluded rather than only the first, and the Go stage takes the bundle from the Node stage — so what an image serves cannot depend on whether the person building it had run `make ui` that day, which is the same reproducibility #1 is about. |
| 12 | **2026-09-05** — A second build argument, `REVISION`, fills `org.opencontainers.image.revision`; it defaults to empty and only the release workflow passes it (`github.sha`) | The image contract asks for that label and #1 named only `VERSION`. A build context carries no commit — `.git` is not even in it (#8) — so there is nothing for a plain `docker build .` to claim, and an empty label is the honest answer where a fabricated one would be worse than none. |
| 13 | **2026-09-05** — Two of the three mutations Testing names do not kill the assertion they were paired with, and the section is corrected: (a) removing the `USER` line changes nothing on its own, because `gcr.io/distroless/static-debian12:nonroot` sets `User=65532` itself — and dropping the `nonroot` tag from the base image changes nothing on its own either, because the `USER` line then supplies it. The two say the same thing twice on purpose, and the uid assertion is what catches losing **both**, which is the mutation the PR shows; (b) removing `ENV TRACEPAD_DATA_DIR` does **not** fail the boot: Docker sets `HOME` from the image's passwd entry, `/home/nonroot` is writable, and the server starts healthy while writing to `/home/nonroot/.local/share/tracepad` in the container's writable layer. `image-check` therefore also asserts that `tracepad.db` is on the volume | Found by running them (the PR's table). The `USER` line stays beside the `nonroot` tag rather than being pruned as redundant, because the file should state its own contract rather than inherit it silently from a tag someone may change — and because either one alone still holds the line if the other is lost. The second finding is the more serious one and is why the assertion was added: an image whose data quietly does not survive `docker rm` is a worse failure than one that refuses to start, and the boot test as specified would have passed it. |
| 14 | **2026-09-05** — **The release PR ships no binary with unreachable commands.** `cmd/tracepad` no longer keeps its own copy of the command list: it routes on `cli.Commands()`, which is the same table `cli.Run` dispatches from, and `datasets`, `runs`, `score-configs` and `export` become reachable for the first time. A test in `cmd/tracepad` holds the two sides equal and refuses a CLI command that collides with `serve`, `mcp`, `version` or `help` | Found while wiring `health` in, and out of scope until this spec's own subject changed what it costs: this is the PR that makes a version tag publish a binary and an image, so a defect that ships four documented command families answering `unknown command` ships *here*. It also undercut the `docker` job's own justification — the job is defended as the seam that proves the real binary dispatches at all, and it exercised one command while four were broken. One table read by both sides is the shape in which the drift cannot recur; the hand-written copy is what let it happen through three specs, because every CLI test calls `cli.Run` directly and skips the binary's routing entirely. |
| 15 | **2026-09-05** — With neither `--url` nor `TRACEPAD_URL`, `tracepad health` takes its target from **`TRACEPAD_LISTEN`** (a wildcard or empty bind host reads as `127.0.0.1`), and only then falls back to `client.DefaultURL`. The `--listen` flag is deliberately not consulted; `docs/docker.md` and the Dockerfile say to move the port with `-e TRACEPAD_LISTEN` | #4 gave the probe the same default every command has, and for this one command that default is wrong: every other command asks a server elsewhere, while this one almost always asks about the process beside it. As specified, `docker run … -e TRACEPAD_LISTEN=:8080` produced a server that worked and a `HEALTHCHECK` that probed 4318 for ever — a container permanently `unhealthy` while perfectly healthy, which is the failure mode a probe exists to prevent. A flag cannot be read the same way: it lives on another process's command line, so the environment is the only place a server and its probe can both look. |
| 16 | **2026-09-05** — `latest` and `X.Y` move only when the tag being released is the newest of its kind — the highest non-pre-release tag overall, and the highest on its own `X.Y` line, by `git tag --sort=-v:refname`. A back-patch publishes its exact version and whichever of those it is still newest for. The tag-shape check and this computation move into a `check` job that both `release` and `image` depend on | #6 said `latest` moves on a release and not on a push, which answers "what may move it" and not "may it move backwards". Tagging `v0.2.5` after `v0.3.0` would have republished `latest` at the older build — a silent downgrade for everyone tracking it, landing them in exactly the older-binary-meets-newer-schema case `docs/docker.md` says is unsupported. Pre-releases are excluded from the comparison because git's version sort places `v0.3.0-rc.1` after `v0.3.0`. The `check` job exists because the guard was previously in `image`, which is `needs: release` — so a refused tag would already have had its archives published, and a half-published release is worse than a refused one. |
| 17 | **2026-09-26** — **`THIRD_PARTY_NOTICES` travels with every release artifact**: the licence text of each Go module compiled into the server — the union over the six platforms of the release matrix, built as the release builds (`-tags ui`, `CGO_ENABLED=0`) — then the Go distribution's own, then each npm package the web interface's client bundle carries. Two halves, each read where it is known: the interface's build lists its packages (`ui/notices.ts`, a Vite plugin over the modules the bundle *renders*, plus any package a CSS banner `/*! name vX \| … */` names, which is how Tailwind's inlined CSS is visible) into `third-party-notices.txt` beside the bundle; `scripts/notices` (`make notices`) lists the Go modules with `go list -deps` and reads their licence files from the module cache — at each module's root and in every directory between it and a package the binary compiles in, which is where a vendored piece keeps its own (zstd's xxhash, the Snappy reference code) — and writes the file. Both read only what is on disk, sort what they write, and **fail on a module or package with no licence file**; the Go half also fails on a package that did not load or belongs to no module, except the main module's `ui` package before `make ui` has staged the bundle, which changes no dependency. The bundle's list starts with its own count. Where it lands: the archive's root beside `LICENSE` and `NOTICE` (`.goreleaser.yaml`, after `make ui` in `before.hooks`), and `/usr/share/doc/tracepad/THIRD_PARTY_NOTICES` in the image, generated in the build stage from the module cache and the bundle stage's list; `scripts/image-check.sh` asserts it. The file is generated, not committed (`.gitignore`). The gate runs the Go half as a test (`scripts/notices`, about two seconds): every module `go list -deps` names for the binary is in it, and a module without a licence file is an error; the interface's half runs in every UI build (`make ui`, e2e, the image). The bundle's list is also served by the binary at `/third-party-notices.txt`, because the interface is embedded and the list is part of it | Apache-2.0 §4 was honoured for Tracepad's own licence and for what `NOTICE` names, but the binary also carries 22 modules and 32 npm packages under MIT, BSD, ISC and Apache licences, most of which ask for their text to travel with a binary distribution, and no artifact carried any of it. Reading licence files from the module cache and `node_modules` rather than asking a licence service keeps the build offline and its output a function of the lock files. Rendered modules rather than `dependencies` because the SPA's runtime is in `devDependencies` (Svelte, SvelteKit) and a declared dependency can be tree-shaken away. The union over platforms because one file ships in every archive. The image puts it with the licence files of #8 and the table above, where an operator already looks; the archive puts it where `LICENSE` is. Committing the file was the alternative: a reviewable diff when a dependency moves, at the price of a check that needs the interface built, which the gate does not do. |
| 18 | **2026-09-26** — **The application-line ceiling (`UI_BUDGET`) rises from 21,800 to 21,900** | Measured the way spec 041 #20 raised it: `main` stood at 21,745 once PR #88 landed; `ui/notices.ts` and its line in `vite.config.ts` are 68, to 21,813. The plugin is build tooling that the budget counts because it lives beside `vite.config.ts`, where the interface's build can import it; the ceiling is raised once, with room for a review cycle, rather than left as a standing warning. |
| 19 | **2026-09-26** — **The supply chain of CI and the releases.** Amends #5. (a) **Build and publish are separate jobs**, and the one that holds a write scope or an OIDC token builds nothing: `release-server.yml` builds the archives (`archives`: GoReleaser `release --clean --skip=publish`, which runs `make ui`) and the image (`image-build`: buildx to an OCI archive) with `contents: read` alone and hands them over as artifacts; `release` (`contents: write`) attests the archives and runs `gh release create --verify-tag --generate-notes` with them — GoReleaser's changelog is off, GitHub writes the notes — marked `--prerelease` for a pre-release tag and `--latest` only when `check` moves the image's `latest` with it (#16), with notes from the server tag before it in its own history (`--notes-start-tag`: for a stable release the stable one before it, none for the first tag), and `image` (`packages: write`) pushes the archive under every tag `check` decided with `skopeo copy --all --preserve-digests` and attests the digest, logged in through Docker's config file, the one the attestation's push reads. No privileged job checks out the repository, installs a toolchain or runs `npm ci`. `release` waits for both builds, so an image that does not build refuses the tag before anything is public. (b) **Pins**: every `uses:` is a full commit SHA with its version as a comment; GoReleaser is `v2.18.2`, not `~> v2`; the three base images of the `Dockerfile` are pinned by the digest of their multi-arch index, tag kept beside it. (c) **Provenance**: BuildKit's provenance is off (`provenance: false`) and so is the uploaded build record (`DOCKER_BUILD_RECORD_UPLOAD: false`), in the release and in CI's `multiarch`; the release's provenance is `actions/attest-build-provenance` — archives by path, the image by digest, pushed to GHCR beside it. Attestations need a public repository on this plan, so no server release is tagged before the repository opens: a release that would go out without provenance fails instead. (d) Every checkout sets `persist-credentials: false`, the release workflows start from `permissions: {}` and name each job's, and no release job uses a dependency cache. (e) **CI**: the concurrency group is the pull request's number, not its branch (`head_ref`); the gate runs `lockfile-lint` (`--allowed-hosts npm --validate-https --validate-integrity --validate-package-names`) over `ui/` and `sdk/js/`'s lock files, allowing the Node package's one alias; the linter is installed from its own lock in `tools/lockfile-lint/`, in that step alone. (f) `.github/dependabot.yml` covers Go (the root module), npm (`ui`, `sdk/js`, `tools/lockfile-lint`), uv (`sdk/python`), Actions and Docker weekly, minor and patch grouped per ecosystem, with a seven-day cooldown; `sdk-trace-base-v1`, the Node package's OpenTelemetry 1.x test double, is kept off major updates, and the `golang` and `node` base images move by digest only, their versions changing by hand with `go.mod` and the interface. `sdk/go` gets no version updates: the versions a published library's `go.mod` names are the floor of every application that imports it, and they move when the package decides; security updates are GitHub's, switched on when the repository opens. Moved by hand, because they are versions handed to a tool rather than declared to a package manager: GoReleaser (`version:` of `goreleaser-action` in `release-server.yml`), npm 11.20.0 (the publish job of `release-sdk-js.yml`), uv 0.11.12 (`UV_VERSION` in `ci.yml` and `release-sdk-py.yml`) and the Python build backend with what it imports (`[tool.uv]` in `sdk/python/pyproject.toml` and `sdk/python/build-constraints.txt`, regenerated with the command in its header). (g) `SECURITY.md` says how to report a vulnerability — GitHub's private reporting — what to expect without promising a turnaround, and that fixes go into the newest release only. (h) Left as it is: `image` unpacks the OCI archive and checks every blob against the registry once per tag, three times for a stable release; pushing once and copying the rest registry to registry is the optimisation if a release ever needs it | (a) A release job with `id-token: write` or a write scope that also ran `npm ci`, `make ui` or an unpinned installer gave any compromised dependency of the build the power to publish, or to sign what it published. Separating them leaves that power to jobs whose only inputs are artifacts from the same run. (c) BuildKit's `mode=max` provenance on a public repository embeds the event that triggered the build — for a tag push, the pusher's name and address — in a public registry, forever; GitHub's attestation records the workflow, the commit and the run, is signed through Sigstore, and is checked with `gh attestation verify`. The archives had none before and get the same. Left to itself `gh release create` marks every new release Latest, so a release candidate or a back-patch would take the badge from the newest stable one, the downgrade #16 refuses for the image. (e) Two forks may share a branch name, and a group on `head_ref` let one cancel the other's run. `--validate-package-names` catches a lock entry whose URL is another package's; the linter run through `npx` would have resolved its own dependencies from ranges on every run. (f) A pin nobody moves is a vulnerability kept on purpose; grouping keeps it to a handful of pull requests a week, and the cooldown keeps a release that is pulled within days out of them. |
| 20 | **2026-09-26** — **The image sets `TRACEPAD_IN_CONTAINER=1`**, a variable the server knows and nothing else sets. With it, the start's plain-HTTP warning (spec 001 #12) is one `INFO` line naming `docs/docker.md`. Amends #2's environment list. The documented `docker run` and compose examples publish on `127.0.0.1` | Inside a container a wildcard bind is required (#2) and the reach is decided by `-p`, which the process cannot see; warning there would fire on every correctly published deployment. A variable rather than sniffing `/.dockerenv` or cgroups: it says exactly "this is our image", is testable, and a person running the binary in some other container gets the host's behaviour, which is the safe default. What the note does not cover, and the docs say so: `--network host`, a Kubernetes pod, or `-p 4318:4318` expose the port over plain HTTP and print the same `INFO` line as the recommended loopback publish. |
| 21 | **2026-09-26** — The image creates `/data` with mode 0700 (spec 044 #13), and the boot test asserts `/data` 700 and `tracepad.db` 600 on the volume | A named volume inherits the image directory's mode, and 0755 let every account on the host through. See spec 044 #13. |

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
| Volume | `/data` (`TRACEPAD_DATA_DIR`), mode 0700 (Decision 21) |
| Environment set in the image | `TRACEPAD_DATA_DIR=/data`, `TRACEPAD_LISTEN=:4318`, `TRACEPAD_IN_CONTAINER=1` (Decision 20) |
| Health | `HEALTHCHECK` via `/tracepad health` (Decision 4) |
| Labels | `org.opencontainers.image.{source,version,revision,licenses,title,description}` |
| Licence files | `/usr/share/doc/tracepad/{LICENSE,NOTICE,THIRD_PARTY_NOTICES}`, `third_party/` beneath it (Decision 17) |

Every other `TRACEPAD_*` variable (`cmd/tracepad/main.go`'s usage) is the
operator's to pass with `-e`. The CLI inside the image works against the
server with `docker exec … /tracepad <command> --url http://localhost:4318`
— the docs show it for `keys create` (there is no `keys new`; the CLI's
subcommands are `ls`, `create` and `rm`).

## CLI contract

```
tracepad health [--url URL] [--json]
```

Exit `0` and the version on stdout for a `200`; `1` and the reason on
stderr for anything else (connection refused, a non-200, a body without
`version`). `--json`: `{"version": "…", "ok": true}`. Listed in
`docs/cli.md` under *Administration*, covered by the usage parity test.

Where it looks, in order: `--url`, `TRACEPAD_URL`, **`TRACEPAD_LISTEN`**
(a wildcard or empty bind host read as `127.0.0.1`), then
`client.DefaultURL` (#15). The `--listen` flag is not consulted — it is on
another process's command line.

## Workflow contract

`release-server.yml`: `on: push: tags: ['v*']`; `permissions: {}` at the
top. Jobs (Decision 19): `check` (`contents: read`); `archives` (`contents:
read`; Go from `go.mod`, Node from `ui/package.json`, GoReleaser `v2.18.2`
with `release --clean --skip=publish`) and `image-build` (`contents: read`;
buildx to an OCI archive, no provenance), both after `check`; `release`
(`contents: write`, `id-token: write`, `attestations: write`) after both
builds; `image` (`packages: write`, `id-token: write`, `attestations: write`)
after `release`. Nothing in it runs on `main`.

`ci.yml`: job `docker` as Decision 7, alongside `gate`, `e2e`, `smoke`,
`sdk`. Every `uses:` in every workflow is pinned by commit SHA (Decision 19).

---

## Testing

- **Go**: `tracepad health` against a test server (`200` → exit 0 and the
  version; a `500` → exit 1; a refused connection → exit 1 with the
  address in the message; `--json` shape); the usage parity test; the
  precedence of `--url` over `TRACEPAD_URL` over `TRACEPAD_LISTEN`, and
  the listen-address mapping itself (#15).
- **Dispatch parity** (#14), in `cmd/tracepad`: every command
  `cli.Commands()` serves is routed by the binary, the four that were
  unreachable are named by hand as the regression they were, none of them
  collides with `serve`/`mcp`/`version`/`help`, and `splitCommand` hands
  each one over. The CLI's own suite cannot cover this: it calls `cli.Run`,
  which is the path that skips the binary's routing.
- **The `docker` CI job** is the image's test (Decision 7); it also asserts
  the licence files are present in the image and that `docker run --rm
  ghcr… health --url http://127.0.0.1:1` exits 1 (the probe fails
  honestly).
- **Local**: `make image` builds the Dockerfile with `VERSION=$(VERSION)`
  and tags `tracepad:dev`; `make image-check` runs Decision 7's sequence
  locally. Both documented in AGENTS.md's Commands.
- **Mutation** for the Go part; the image contract by the CI job's
  assertions, each of which must fail on the corresponding Dockerfile edit
  (the `USER` line **and** the base image's `nonroot` tag both dropped →
  the uid assertion;
  `HEALTHCHECK` removed → the wait never reaches `healthy`;
  `ENV TRACEPAD_DATA_DIR` removed → the database is not on the volume).
  The PR shows each; #13 records what the first and third of these looked
  like when they were first run, and why they are worded this way.
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
