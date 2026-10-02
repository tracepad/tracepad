# The Tracepad image (spec 020). Multi-stage and self-contained: `docker build .`
# in a checkout produces the artifact the docs describe, with no release
# pipeline and no prebuilt binary involved (#1). The two builder stages are
# `make ui` and `go build -tags ui` (Makefile:18-19) — the same recipe a host
# build follows, so a binary from either path answers /health with the same
# version, even though the bytes differ because the build stamps do.

# Every base image is pinned by the digest of its multi-arch index, the tag
# kept beside it for the reader and for Dependabot, which moves both (spec 020
# #19): a tag is a name the publisher can point elsewhere, a digest is the bytes.

# The version the binary reports. `dev` for anyone building from a checkout;
# the release workflow passes the tag without its `v` (#6).
ARG VERSION=dev
# The commit the image was built from, for the OCI revision label. Empty by
# default, which is the honest answer: a build context carries no commit, and
# only the workflow that has one should claim it (#12).
ARG REVISION=

# --- The web interface --------------------------------------------------------
#
# Node 24, the one `ui/package.json` declares (spec 006 #16). The bundle is
# static files and carries no architecture, so this stage is pinned to the
# platform doing the building: under `--platform linux/amd64,linux/arm64` it
# then runs once instead of once per architecture, and never under emulation
# (#10).
FROM --platform=$BUILDPLATFORM node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS ui

WORKDIR /src/ui
# The manifest and the lock file first, on their own layer: editing a component
# should not reinstall the toolchain.
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# --- The binary ---------------------------------------------------------------
#
# On the build platform for the same reason, and cross-compiled from there:
# `CGO_ENABLED=0` makes that exact — the SQLite driver is modernc's, pure Go
# (.goreleaser.yaml:17) — and it is the difference between a Go build that takes
# a minute and an emulated arm64 one that takes fifteen (#10).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# The bundle from the stage above, staged where the `ui` build tag embeds it
# (internal/ui/embedded.go). It arrives from that stage and never from the
# context, so what the image serves cannot depend on whether the person
# building it happened to have run `make ui` (#11). Without it this build does
# not compile, which is the intended failure: an official artifact that quietly
# served the stub page is the one thing spec 006 #9 rules out.
COPY --from=ui /src/ui/dist ./internal/ui/dist

# The licences of the Go modules compiled in and of the npm packages the bundle
# above carries, from the module cache `go mod download` filled and the list
# the bundle's build wrote beside it; no network (spec 020 #17).
RUN mkdir -p /out && go run ./scripts/notices -ui internal/ui/dist/third-party-notices.txt -o /out/THIRD_PARTY_NOTICES

ARG VERSION
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags ui -ldflags "-s -w -X main.version=$VERSION" \
    -o /out/tracepad ./cmd/tracepad

# /data is created here because the runtime stage has no shell to create it
# with: a distroless image can only receive directories, and the one the server
# writes to has to arrive owned by the user that will write to it (#2). A named
# volume inherits that ownership when Docker seeds it; a host directory bind
# mounted over it does not, which is what docs/docker.md's `install` line is for.
RUN mkdir -p /out/data

# --- The image ----------------------------------------------------------------
#
# Distroless static: no shell, no package manager, `nonroot` (65532) as the
# user, and the two things a static Go program still reads from an OS — zone
# data and CA roots (#2).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG VERSION
ARG REVISION
LABEL org.opencontainers.image.title="Tracepad" \
      org.opencontainers.image.description="Lightweight, self-hosted, OTLP-native store and viewer for LLM and agent traces" \
      org.opencontainers.image.source="https://github.com/tracepad/tracepad" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/tracepad /tracepad
# 0700: the directory is the guard of everything the server writes into it,
# and a named volume inherits this mode when Docker seeds it (spec 044 #13).
# Set here, once, where the directory is copied in.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/data /data

# Apache-2.0 §4(d) applies to a container image as it does to an archive, so
# the NOTICE and the third-party licences travel in it (.goreleaser.yaml says
# the same of the release archives).
COPY LICENSE NOTICE /usr/share/doc/tracepad/
COPY --from=build /out/THIRD_PARTY_NOTICES /usr/share/doc/tracepad/
COPY third_party /usr/share/doc/tracepad/third_party

# The two defaults that make `docker run -v tracepad:/data -p 127.0.0.1:4318:4318 …`
# the whole command. TRACEPAD_LISTEN=:4318 rather than 127.0.0.1:4318 on
# purpose — the publish, not the bind, keeps it on loopback: a server bound to
# loopback inside a container is a server nothing outside it can reach.
# TRACEPAD_IN_CONTAINER turns the plain-HTTP warning at start into one INFO
# line: here the wildcard bind is the design, and where the port is published —
# which the process cannot see — decides who reaches it (spec 020 #20).
ENV TRACEPAD_DATA_DIR=/data \
    TRACEPAD_LISTEN=:4318 \
    TRACEPAD_IN_CONTAINER=1

EXPOSE 4318
VOLUME /data
USER nonroot:nonroot

# The probe is the binary, because the image has nothing else to probe with: no
# shell, no curl, no wget. `/health` is the one route that authenticates
# nothing, so the probe needs no key either (#4).
#
# It finds the server through TRACEPAD_LISTEN, so moving the port with
# `-e TRACEPAD_LISTEN=:8080` moves the probe with it. A `--listen` flag appended
# to `docker run` does not: a flag on the server's command line is not something
# a second process can read, and the container would run correctly while
# reporting itself unhealthy for ever (#15).
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/tracepad", "health"]

# Arguments are the server's flags: `docker run … tracepad --data-dir /data/x`
# reaches `serve`, because a leading flag belongs to the default command
# (spec 001 #1). The port is the exception above — set it in the environment.
ENTRYPOINT ["/tracepad"]
CMD []
