# Docker

`ghcr.io/tracepad/tracepad` is the server, the web interface and the CLI in one
image — the same binary the release archives carry, built from the `Dockerfile`
in this repository. It runs as a non-root user, keeps everything under `/data`,
and listens on `4318`.

```sh
docker run -d --name tracepad \
  -v tracepad:/data \
  -p 4318:4318 \
  ghcr.io/tracepad/tracepad
```

That is the whole command: the image already sets `TRACEPAD_DATA_DIR=/data` and
`TRACEPAD_LISTEN=:4318`, so there is nothing to configure to get a running
store. The interface is on <http://localhost:4318/>.

## The keys are printed once, to the log

The first run creates the database, a project called `default` and its key
pair, and prints them exactly as it does on a host
([quickstart](quickstart.md)) — which for a container means its stdout:

```sh
docker logs tracepad
```

```
Project "default" created. Connect your app with either:

  # OpenTelemetry SDK
  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
  OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"
  …
```

**Copy the secret key somewhere.** It is stored hashed, so this is the only
time it is printable, and it is not printed again on later starts. There is
deliberately no file under `/data` holding it: that would put a secret on a
volume outliving the container, and give anyone who can read the volume a way
to learn a key that a host install does not have.

The same log carries the **setup link** — the way into the browser interface,
until this deployment has an owner:

```
  http://localhost:4318/setup#token=…
```

Its token is minted per start and held in memory, never on the volume, so a
`docker restart` prints a new one and a log kept from last week opens nothing.
The address in it is the container's own guess; behind a proxy, set
`TRACEPAD_URL` and the link is printed at the address your people use. See
[accounts.md](accounts.md).

There is deliberately no environment variable for the first owner's password:
one in `docker-compose.yml` is the thing accounts exist to stop pasting.

Any command can be run against the server from inside its own container —
`/tracepad` is the same binary, and `--url http://localhost:4318` points it at
the server it is sharing with:

```sh
docker exec tracepad /tracepad keys create --url http://localhost:4318 \
  --key tp-sk-…            # mint a second pair, to rotate onto
```

**If the key is lost rather than being rotated**, that command has nothing to
authenticate with — every CLI command but `health` needs a credential, and the
one you would pass is the one you do not have. The credential that still works
is the admin token, which is why a deployment you cannot afford to lock
yourself out of should be started with one:

```sh
docker run -d --name tracepad -v tracepad:/data -p 4318:4318 \
  --env-file ./tracepad.env \        # TRACEPAD_ADMIN_TOKEN=…
  ghcr.io/tracepad/tracepad

docker exec tracepad /tracepad keys create \
  --url http://localhost:4318 --key "$TRACEPAD_ADMIN_TOKEN"
```

Without one, the remaining route is the declarative bootstrap: set
`TRACEPAD_PROJECTS` and restart, which re-adds the keys it names. See
[admin.md](admin.md) and [cli.md](cli.md).

## What the image is

| | |
|---|---|
| Name | `ghcr.io/tracepad/tracepad` |
| Tags | `X.Y.Z`, `X.Y`, `latest`; a pre-release publishes its exact tag alone |
| Platforms | `linux/amd64`, `linux/arm64` |
| User | `nonroot`, uid **65532** |
| Entrypoint | `/tracepad` — arguments are the server's flags |
| Port | `4318` |
| Volume | `/data` |
| Set in the image | `TRACEPAD_DATA_DIR=/data`, `TRACEPAD_LISTEN=:4318` |
| Health | `HEALTHCHECK` running `tracepad health` |
| Licences | `/usr/share/doc/tracepad/` — `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES` (every Go module and npm package the binary carries) and `third_party/` |

There is no shell in it. The runtime layer is distroless — the binary, CA
roots, zone data and nothing else — so there is no package manager to run, no
`sh` to exec into, and nothing to patch inside the container. Upgrading is
pulling a new image.

Pin a tag in anything you deploy. `latest` is convenient for a laptop and is
the wrong choice for a machine you are not watching; `X.Y` is the pin for
"patches yes, surprises no".

## The volume, and who owns it

The server runs as uid 65532 and writes the database as that user. **A named
volume is the path of least resistance**: Docker creates it with the ownership
the image declares, and nothing else is needed.

```sh
docker run -d -v tracepad:/data … ghcr.io/tracepad/tracepad
```

A **host directory** bind-mounted over `/data` keeps the host's ownership
instead, and if that is root the server cannot create its database and exits
saying so:

```
ERROR fatal err="open /data/tracepad.db: … unable to open database file (14)"
```

The fix is to give the directory to the user that will write it, once, before
the first run:

```sh
mkdir -p ./tracepad-data
sudo chown 65532:65532 ./tracepad-data
docker run -d -v "$PWD/tracepad-data:/data" … ghcr.io/tracepad/tracepad
```

You can also run the container as root with `--user 0:0`, and it will work. Do
not: this store holds every prompt and completion your application ever sent,
and a container that writes `/data` as root is one that hands root-owned files
back to your host and runs a network service as root to do it.

## Configuration

Every `TRACEPAD_*` variable the binary understands ([quickstart](quickstart.md)
and `tracepad help`) is passed with `-e`:

```sh
docker run -d --name tracepad \
  -v tracepad:/data -p 4318:4318 \
  -e TRACEPAD_STORE_RAW=off \
  -e TRACEPAD_SWEEP_INTERVAL=30m \
  ghcr.io/tracepad/tracepad
```

Three of them deserve a warning.

`TRACEPAD_URL` is not only the CLI's "which server" any more: it is the host of
the setup and invitation links this server prints and hands out. Behind a
reverse proxy the container can only guess, and the guess is its own address,
so set it to the address your people type.

`TRACEPAD_ADMIN_TOKEN` is a credential, and anything passed with `-e` is
readable ever after in `docker inspect` and in the daemon's logs. Use
`--env-file`, or your orchestrator's secret mechanism, as you would for any
other password.

`TRACEPAD_LISTEN` is already right, and the way to break it is to set it to
`127.0.0.1:4318`. Inside a container, loopback is the container's own: the
server would answer its own health check and nothing else, and `-p` would
publish a port nothing accepts on. Bind to `:4318` — the container **is** the
isolation boundary — and control who can reach it with `-p 127.0.0.1:4318:4318`
on the host side instead.

**To change the port, set `TRACEPAD_LISTEN`; do not pass `--listen`.** Both
move the server, but only the variable moves the health check with it: the
probe reads `TRACEPAD_LISTEN` (and `TRACEPAD_URL` ahead of it) and cannot see a
flag you appended to the command line, so a container started with `--listen
:8080` runs correctly and is marked `unhealthy` for ever.

```sh
docker run -d -e TRACEPAD_LISTEN=:8080 -p 8080:8080 … ghcr.io/tracepad/tracepad
```

## Health

The image declares a `HEALTHCHECK`, so Docker knows whether the server is up:

```sh
docker inspect --format '{{.State.Health.Status}}' tracepad
```

It runs [`tracepad health`](cli.md#health), which needs no key — the same
command works from a load balancer, a systemd unit or your own script:

```sh
tracepad health --url http://tracepad.internal:4318
```

## Compose

```yaml
services:
  tracepad:
    image: ghcr.io/tracepad/tracepad:0.2
    restart: unless-stopped
    ports:
      - "127.0.0.1:4318:4318"
    volumes:
      - tracepad:/data
    env_file: [.env]        # TRACEPAD_ADMIN_TOKEN and anything else secret

volumes:
  tracepad:
```

`docker compose logs tracepad` is where the first run's keys are.

## Upgrading, and backing up first

Schema migrations run on start, as they do on a host: a new image opens the
existing `/data` and brings it forward. **A downgrade is not supported** — an
older binary meeting a newer schema is not a case anything here handles — so
the upgrade is only as safe as the copy you took before it.

Back the volume up by tarring it from a throwaway container:

```sh
docker stop tracepad
docker run --rm -v tracepad:/data -v "$PWD:/backup" busybox \
  tar czf /backup/tracepad-$(date +%F).tar.gz -C /data .
docker pull ghcr.io/tracepad/tracepad:0.3
docker rm -f tracepad && docker run -d --name tracepad … ghcr.io/tracepad/tracepad:0.3
```

Stopping first matters: SQLite's write-ahead log is part of the database, and a
tar of a live one is a copy of a file mid-write. Restoring is the same command
with the arguments swapped, into a stopped container's volume.

## Building it yourself

The `Dockerfile` is the whole recipe — it builds the interface and the binary
from the sources in the checkout, so a fork or a patched tree produces the same
kind of artifact the releases do:

```sh
docker build --build-arg VERSION=0.2.0-mine -t tracepad:mine .
```

Without `--build-arg VERSION`, the image reports `dev`. `make image` and `make
image-check` are the same build and the boot test CI runs against it.
