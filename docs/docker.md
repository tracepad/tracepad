# Docker

`ghcr.io/tracepad/tracepad` is the server, the web interface and the CLI in one
image — the same binary the release archives carry, built from the `Dockerfile`
in this repository. It runs as a non-root user, keeps everything under `/data`,
and listens on `4318`. The same image is on Docker Hub as
[`tracepad/tracepad`](#docker-hub).

```sh
docker run -d --name tracepad \
  -v tracepad:/data \
  -p 127.0.0.1:4318:4318 \
  ghcr.io/tracepad/tracepad
```

That is the whole command: the image already sets `TRACEPAD_DATA_DIR=/data` and
`TRACEPAD_LISTEN=:4318`, so there is nothing to configure to get a running
store. The interface is on <http://localhost:4318/>.

**The port is published on loopback, and every example here does the same.**
`-p 4318:4318` would publish it on every interface of the host, and Tracepad
speaks plain HTTP: the passwords people sign in with, their session cookies and
your applications' keys would cross the network readable by anything on the
path. Keep `127.0.0.1:` for applications on the same machine; to serve anyone
else, put a TLS proxy in front — [Serving over TLS](#serving-over-tls). Inside
the container the server cannot see where its port was published, so it says
this once at every start, as an `INFO` line, whatever `TRACEPAD_URL` says. (The
same binary on a host listens on loopback unless told otherwise, and bound
beyond it makes this a `WARN`.) The note reads the same whatever the
publish is, so it does not tell you when the port *is* exposed: with
`--network host`, in a Kubernetes pod, or with `-p 4318:4318`, other machines
reach the server over plain HTTP and nothing louder is printed.

## The keys are printed once, to the log

The first run creates the database, a project called `default` and a key for
your application, and prints it exactly as it does on a host
([quickstart](quickstart.md)) — which for a container means its stdout:

```sh
docker logs tracepad
```

```
Project "default" created. Connect your app with either:

  # OpenTelemetry SDK
  OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
  OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"
  …
```

**Copy the secret key somewhere.** It is stored hashed, so this is the only
time it is printable, and it is not printed again on later starts. There is
deliberately no file under `/data` holding it: that would put a secret on a
volume outliving the container, and give anyone who can read the volume a way
to learn a key that a host install does not have.

**The log still holds it.** Docker keeps a container's output for as long as
the container exists, and with the default `json-file` driver it never rotates
it: whoever can run `docker logs tracepad` — or read the log file under the
daemon's directory, or receive whatever ships your logs elsewhere — can read
that key next month. That is why it holds the `ingest` scope alone: whoever
finds it can send spans into the project, not read what is in it. A key that
reads — for the CLI, an agent, the eval harness — is minted in the interface
and shown in your browser, never in this log
([quickstart](quickstart.md#3-look-at-them)). Treat the printed key as exposed
once it has been copied out all the same, and rotate onto one that was never
printed: in the web interface, under
**Settings → Project → API keys**, mint a pair — with the `ingest` scope alone
for an application ([api.md](api.md#scopes)) — move your applications onto it,
and revoke the printed one. A project key cannot do this for you — no key
mints or revokes keys — so from a terminal it takes the admin token:

```sh
read -rs TRACEPAD_API_KEY && export TRACEPAD_API_KEY   # paste TRACEPAD_ADMIN_TOKEN
docker exec -e TRACEPAD_API_KEY tracepad /tracepad projects ls --url http://localhost:4318
ID=…   # the project's id from that listing: the token reaches every project
docker exec -e TRACEPAD_API_KEY tracepad /tracepad keys create --project $ID --scope ingest --url http://localhost:4318
# move your applications onto the new pair, then revoke the printed one
docker exec -e TRACEPAD_API_KEY tracepad /tracepad keys rm tp-pk-… --project $ID --url http://localhost:4318
```

Inside the container the admin token is already in the environment, or in the
file `TRACEPAD_ADMIN_TOKEN_FILE` names, and the commands the CLI's help marks
`(admin token)` read it from there when no key is set (the server being on this
machine, which inside the container it is) — so
`docker exec tracepad /tracepad keys ls --url http://localhost:4318` needs no
`-e` and no paste. No other command takes it that way; they ask for a key.

**`docker exec` is for commands that talk to the server, and `skills install`
does not.** It writes files, and in the running container they would land in the
container's own layer — gone with the container, and the image has no shell to
look for them with. Inside a container `skills install` refuses a target that
is not on a mounted volume; run it in a container of its own, onto a directory
you mount, as [agents.md](agents.md#from-the-docker-image) shows.

A key minted this way is shown in your browser or printed to your terminal,
never to the container's log. A deployment that declares its keys in `TRACEPAD_PROJECTS`
from the start never has one printed: the server names the variable where the
secret would go instead of repeating it.

The same log carries the **setup link** — the way into the browser interface,
until this deployment has an owner:

```
  http://localhost:4318/setup#token=…
```

Its token is minted per start and held in memory, never on the volume, and it
works for 24 hours: a `docker restart` prints a new one, and a log kept from
last week opens nothing. A deployment that makes its first owner with the admin
token can set `TRACEPAD_SETUP=off`, and no link is minted or printed at all.
The address in it is the container's own guess; behind a proxy, set
`TRACEPAD_URL` and the link is printed at the address your people use. See
[accounts.md](accounts.md).

There is deliberately no environment variable for the first owner's password:
one in `docker-compose.yml` is the thing accounts exist to stop pasting.

Any command can be run against the server from inside its own container —
`/tracepad` is the same binary, and `--url http://localhost:4318` points it at
the server it is sharing with. Hand it the credential through the environment,
as `-e TRACEPAD_API_KEY` with no value — Docker copies the variable from your
shell — and not as `--key`: an argument is in your shell's history and in the
process list of the host and the container while it runs, where every account
can read it. `read -s` takes the value without echoing it or leaving it in the
history either.

```sh
read -rs TRACEPAD_API_KEY && export TRACEPAD_API_KEY   # paste the key
docker exec -e TRACEPAD_API_KEY tracepad /tracepad traces ls \
  --url http://localhost:4318
```

**If the key is lost**, nothing the key could do is left to do with it — and
minting a new one was never among them. The credentials that mint keys are an
owner's or editor's session and the admin token, which is why a deployment you
cannot afford to lock yourself out of should be started with one:

```sh
# A token of at least 32 characters, generated rather than invented; a shorter
# one stops the server from starting.
( umask 077; printf 'TRACEPAD_ADMIN_TOKEN=%s\n' "$(openssl rand -hex 32)" > tracepad.env )
docker run -d --name tracepad -v tracepad:/data -p 127.0.0.1:4318:4318 \
  --env-file ./tracepad.env \
  ghcr.io/tracepad/tracepad

export TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_ADMIN_TOKEN=//p' tracepad.env)"
docker exec -e TRACEPAD_API_KEY tracepad /tracepad projects ls --url http://localhost:4318
ID=…   # the project's id from that listing
docker exec -e TRACEPAD_API_KEY tracepad /tracepad keys create --project $ID --scope ingest --url http://localhost:4318
```

Without one, an owner or editor signed in to the web interface mints a pair
in the project's settings. `TRACEPAD_PROJECTS` is not a way back in: it creates
the projects it names that do not exist yet and leaves an existing project's
keys as they are. See [admin.md](admin.md) and [cli.md](cli.md).

## What the image is

| | |
|---|---|
| Name | `ghcr.io/tracepad/tracepad`, and `tracepad/tracepad` on [Docker Hub](#docker-hub) |
| Tags | `X.Y.Z`, `X.Y`, `latest`; a pre-release publishes its exact tag alone, on GHCR |
| Platforms | `linux/amd64`, `linux/arm64` |
| User | `nonroot`, uid **65532** |
| Entrypoint | `/tracepad` — arguments are the server's flags |
| Port | `4318` |
| Volume | `/data` |
| Set in the image | `TRACEPAD_DATA_DIR=/data`, `TRACEPAD_LISTEN=:4318`, `TRACEPAD_IN_CONTAINER=1` (turns the plain-HTTP warning into a note, printed at every start whatever `TRACEPAD_URL` is) |
| Health | `HEALTHCHECK` running `tracepad health` |
| Licences | `/usr/share/doc/tracepad/` — `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES` (every Go module and npm package the binary carries) and `third_party/` |

There is no shell in it. The runtime layer is distroless — the binary, CA
roots, zone data and nothing else — so there is no package manager to run, no
`sh` to exec into, and nothing to patch inside the container. Upgrading is
pulling a new image.

Pin a tag in anything you deploy. `latest` is convenient for a laptop and is
the wrong choice for a machine you are not watching; `X.Y` is the pin for
"patches yes, surprises no".

### Docker Hub

Every stable release is also on Docker Hub, as `docker.io/tracepad/tracepad`
(`docker pull tracepad/tracepad`), with the same exact tags (`0.1.0`) as on GHCR:

```sh
docker run -d --name tracepad -v tracepad:/data -p 127.0.0.1:4318:4318 \
  tracepad/tracepad
```

GHCR is where releases are published and what the examples here use; Docker
Hub carries a copy of the same multi-architecture image, byte for byte, so
the digest of `tracepad/tracepad:0.1.0` is the digest of
`ghcr.io/tracepad/tracepad:0.1.0`. Pick the one your network can reach, or
the one your registry mirror already caches. A pre-release (`0.2.0-rc.1`) is
on GHCR only.

The moving tags, `X.Y` and `latest`, follow the same rule on both — only
forward, to the newest release of the line and overall — but each registry
applies it when its own copy is made, so while two releases overlap they can
end up pointing at different releases: GHCR's `latest` stays at whichever
release finished publishing last, Docker Hub's follows the newest tag, and
neither registry corrects the other until the next release that moves it.
Anything you deploy should pin an exact tag, or a digest, which is the same
on both.

The build-provenance attestation lives with GHCR's image and is found by
digest, so it covers the copy: verify it exactly as for the GHCR image, by
name or by the digest you pulled.

```sh
gh attestation verify oci://docker.io/tracepad/tracepad:0.1.0 --repo tracepad/tracepad
```

Without a terminal (a script, CI) a verification that passes prints nothing:
the exit code is the answer. When a log should carry the signer and the commit
too, ask for them (the details are in
[install.md](install.md#verify-what-you-downloaded)):

```sh
gh attestation verify oci://docker.io/tracepad/tracepad:0.1.0 --repo tracepad/tracepad \
  --format json --jq '.[0].verificationResult.signature.certificate
                      | {buildSignerURI, sourceRepositoryDigest}'
```

Do not add `--bundle-from-oci`: that reads the attestation from the registry
you name, and Docker Hub does not hold it.

The copy is made after the GHCR image is published, so for a short while after
a release — longer, if the copy has to be repeated — `0.1.0` can be on GHCR and
not yet on Docker Hub. The GHCR image is never held back by it.

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

The fix is to create the directory for the user that will write it, once,
before the first run — owned by uid 65532 and closed to everyone else:

```sh
sudo install -d -m 0700 -o 65532 -g 65532 ./tracepad-data
docker run -d -v "$PWD/tracepad-data:/data" … ghcr.io/tracepad/tracepad
```

`mkdir -p` and a `chown` would work too, and leave the directory `0755` until
the server's first start closes it ([Who else can read it](#who-else-can-read-it)).

You can also run the container as root with `--user 0:0`, and it will work. Do
not: this store holds every prompt and completion your application ever sent,
and a container that writes `/data` as root is one that hands root-owned files
back to your host and runs a network service as root to do it.

### Who else can read it

The database holds every prompt and completion, the accounts' password hashes
and the key that signs media uploads, so the server keeps it to its owner: the
data directory `0700`, the database, its `-wal` and `-shm`, its lock file
(`tracepad.db.lock`) and the pre-migration backups `0600`. It creates them that way, and it tightens them
**at every start** — an install from before this, whose files were `0644` in a
`0755` directory, is closed by the first start of the new version. The image
creates `/data` as `0700`, so a new named volume starts closed.

The directory is tightened only while it holds nothing but the database's own
files. One you share with other things — `TRACEPAD_DATA_DIR=.`, a home
directory — keeps the mode you gave it, and the log says so at every start, a
`WARN` naming the directory and one of the other files; the database files in
it are `0600` all the same. Give the database a directory of its own.

When a mode cannot be changed — a filesystem without Unix modes, a directory
the server does not own — the server starts anyway and says so in its log, a
`WARN` naming the file and its mode. Close that one yourself; for a host
directory, through `sudo`, since once it is `0700` and owned by uid 65532 your
own shell can no longer list it:

```sh
sudo chmod 0700 ./tracepad-data
sudo find ./tracepad-data -maxdepth 1 -name 'tracepad.db*' -exec chmod 0600 {} \;
```

## Configuration

Every `TRACEPAD_*` variable the binary understands ([configuration.md](configuration.md)
lists them all, and `tracepad help` prints the server's) is passed with `-e`:

```sh
docker run -d --name tracepad \
  -v tracepad:/data -p 127.0.0.1:4318:4318 \
  -e TRACEPAD_STORE_RAW=off \
  -e TRACEPAD_SWEEP_INTERVAL=30m \
  ghcr.io/tracepad/tracepad
```

Three of them deserve a warning.

`TRACEPAD_URL` is not only the CLI's "which server" any more: it is the host of
the setup and invitation links this server prints and hands out. Behind a
reverse proxy the container can only guess, and the guess is its own address,
so set it to the address your people type — see
[Serving over TLS](#serving-over-tls).

`TRACEPAD_ADMIN_TOKEN` is a credential that creates owner accounts, and **no
environment variable keeps it out of `docker inspect`**: `-e`, `--env-file` and
Compose's `env_file` all end up in the container's configuration, which shows
every variable in plain text to anyone who can talk to the Docker daemon. An
env file does keep the value off the command line — out of your shell's
history, the host's process list and a `docker-compose.yml` you commit — so if
you use one, `chmod 600` it and keep it out of version control. What keeps the
token out of the container's configuration is a file mounted into it, named by
`TRACEPAD_ADMIN_TOKEN_FILE`; the configuration then shows the path, not the
token. The server reads the file at start, trims the newline, and refuses to
start when both variables are set:

```sh
( umask 077; openssl rand -hex 32 > admin-token )
# The image runs as uid 65532; the file must be readable by it.
sudo chown 65532 admin-token
docker run -d --name tracepad -v tracepad:/data -p 127.0.0.1:4318:4318 \
  -v "$PWD/admin-token:/run/secrets/tracepad_admin_token:ro" \
  -e TRACEPAD_ADMIN_TOKEN_FILE=/run/secrets/tracepad_admin_token \
  ghcr.io/tracepad/tracepad
```

Compose's `secrets:` mounts the same file at `/run/secrets/<name>` for you (see
[Compose](#compose)). The rest is who has the Docker socket: on that host it is
as good as root, and it reads this token whatever you do. The same goes for
`TRACEPAD_PROJECTS`, whose entries carry secret keys. An admin token shorter
than 32 characters stops the start, and so does a short declared secret for a
project the start would create, each with the command above in the message. A
project that already exists keeps the keys it has — the variable never rotates
them — so a short declared secret that is still its key is a warning at every
start, saying how to replace it.

`TRACEPAD_LISTEN` is already right, and the way to break it is to set it to
`127.0.0.1:4318` — or to `localhost:4318`, the binary's default, which the image
overrides for this reason. Inside a container, loopback is the container's own: the
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
docker run -d -e TRACEPAD_LISTEN=:8080 -p 127.0.0.1:8080:8080 … ghcr.io/tracepad/tracepad
```

### Ingest under load

Two settings size what one export may cost everything else. Neither needs
tuning on a laptop or a small server; both are checked at start, and a value
out of range refuses to start. ([configuration.md](configuration.md) has them
with every other variable.)

| Variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_MAX_SPANS_PER_REQUEST` | `20000` | Spans one export may carry; an export with more is `413`, refused whole. At least `1`. |
| `TRACEPAD_BODY_BUDGET_BYTES` | four times `TRACEPAD_MAX_BODY_BYTES` (80 MiB — up to about 1.6 GiB of heap at the peak, see below) | Request bodies held in memory at once, counted decompressed; a body that does not fit is `429` with `Retry-After`, which exporters retry. At least `TRACEPAD_MAX_BODY_BYTES`. |

The budget counts body bytes, not the heap: while an export is decoded,
mapped and written, its heap peaks at roughly 12 to 18 times its body for
protobuf and 15 to 20 times for JSON
([ingest.md](ingest.md#how-much-one-export-may-carry)), so the default 80 MiB
budget can mean a gigabyte or more at the peak. With a memory limit on the
container (`--memory 1g`), keep the budget times about 20 under it — for 1 GiB,
`TRACEPAD_BODY_BUDGET_BYTES=41943040` (40 MiB) with the body cap at its 20 MiB
default, or lower both. **On a small machine, lower `TRACEPAD_BODY_BUDGET_BYTES`**:
the default is sized for a host with memory to spare, and the budget is what
bounds the heap ingest can take at once.

An OpenTelemetry Collector in front of Tracepad should bound its batches, or
one export after a burst can pass the span limit and be dropped as a `413`:
set the `batch` processor's `send_batch_max_size` — see
[ingest.md](ingest.md#how-much-one-export-may-carry).
[`GET /api/v1/system`](api.md#system) shows the budget in use and how often
each refusal happened.

### Reads under load

Two settings size what reads may cost everything else. Neither needs tuning on
a laptop or a small server; both are checked at start, and a value out of range
refuses to start.

| Variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_READ_TIMEOUT` | `20s` | The deadline of one read, its wait for a slot included. A read it stops is `503` "narrow the time range or the filters". From `1s` to `4m`: the server gives a response five minutes to be written, and the deadline's answer has to come first. |
| `TRACEPAD_READ_CONCURRENCY` | twice the processors, at least `4` | Reads served at once; one that finds no slot before its deadline is `503` "busy" with `Retry-After`. At least `1`. |

Reads are CPU-bound, so more of them at once than there are cores only makes
each slower and holds more memory. The processors are the container's: with a
CPU limit (`--cpus 2`), Go counts the limit, not the host. The database pool
opens at most two connections for each slot and for the system endpoint's own
one, one for the writer, and eight more — background jobs, credential checks
and ingest take no slot and have that headroom. Keep a
proxy's read timeout (`proxy_read_timeout` in nginx, 60 s by default) above
`TRACEPAD_READ_TIMEOUT`, so the server's reason reaches the client rather than
the proxy's. [`GET /api/v1/system`](api.md#system) shows the slots in use and
how often each refusal happened.

## Serving over TLS

Tracepad does not terminate TLS; a reverse proxy in front of it does, and it is
the only way to serve anyone who is not on the same machine without their
passwords, cookies and keys crossing the network in the clear. Keep the
container's port on loopback, so the proxy is the only way in, and give the
proxy the name people will use:

```
# Caddyfile — Caddy obtains and renews the certificate itself
traces.example.com {
	reverse_proxy 127.0.0.1:4318
}
```

Any proxy will do if it does four things, which Caddy does by default and
nginx needs told:

```nginx
location / {
    proxy_pass http://127.0.0.1:4318;
    proxy_set_header Host              $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host  $host;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    client_max_body_size 20m;   # an OTLP batch may be up to 20 MiB
}
```

- **`X-Forwarded-Proto: https`** is how the server knows the browser is on
  https, when a trusted proxy (below) sends it, and it then marks the session cookie `Secure`
  ([accounts.md](accounts.md#signing-in)) and refuses writes from an
  `http://` page on the same name. Configure the proxy to set
  `X-Forwarded-Proto`, not append to it, as nginx's `proxy_set_header` does:
  behind more than one proxy the first value counts, and it should never be
  one a client sent.
- **`Host` or `X-Forwarded-Host`** carries the name the browser typed. Writes
  from the interface are refused unless their `Origin` is one of this server's
  hosts, and behind a proxy that rewrites `Host` the forwarded one, from a
  trusted proxy, is how it recognises its own.
- **`X-Forwarded-For`, appended to** (`$proxy_add_x_forwarded_for` in nginx;
  Caddy writes it on its own). The server reads it from the right, and only
  when the connection comes from a proxy it trusts, to learn the client's
  address. It uses that address to limit how many passwords one client may
  check, and to list where each session signed in. A proxy on the same host is
  trusted by default. A container's proxy is not: to the container, the host
  is the bridge's gateway (`172.17.0.1` on Docker's default network), so name
  it with `TRACEPAD_TRUSTED_PROXIES`. Otherwise every client counts as one, and
  the log says so once an hour. The same list governs `X-Forwarded-Proto` and
  `X-Forwarded-Host`: from any other peer they are ignored, so behind an
  untrusted proxy the cookie is not marked `Secure`, and only `Host` and
  `TRACEPAD_URL` name the server's address.
- **A body limit at least `TRACEPAD_MAX_BODY_BYTES`** (20 MiB by default).
  nginx refuses anything over 1 MiB unless told otherwise, and an exporter's
  large batch is then lost at the proxy with a `413` the server never sees.

The proxy should not add a `Content-Security-Policy` of its own unless it is
meant to narrow the page. The server already sends one with the interface
([ui.md](ui.md#what-the-page-may-load)), the browser enforces every policy it
is given, and a proxy that *replaces* the header owns what the interface may
load from then on. The same goes for `X-Frame-Options` and `Referrer-Policy`,
which the server sets too ([api.md](api.md#response-headers)).

Then tell the server where people reach it, and it prints its setup and
invitation links there. The note about plain HTTP stays: the direct listener
is still reachable without the proxy, which is why the port stays on loopback,
and the server cannot tell from inside the container that it is:

```sh
docker run -d --name tracepad -v tracepad:/data -p 127.0.0.1:4318:4318 \
  -e TRACEPAD_URL=https://traces.example.com \
  -e TRACEPAD_TRUSTED_PROXIES=172.17.0.1 \
  ghcr.io/tracepad/tracepad
```

`TRACEPAD_TRUSTED_PROXIES` takes addresses and CIDR ranges, comma-separated
(`172.17.0.1`, `10.0.0.0/24`, `loopback`), or `none`. On Docker Desktop the
gateway is another address. To check, ask `GET /api/v1/system` through the
proxy with a project key that holds `read` (`curl -H "Authorization: Bearer
tp-sk-…" https://traces.example.com/api/v1/system`) and read `source`: it
should be your own address, not the gateway's. Trust only
the proxy, not the network it sits on. A trusted address can name any client
it likes, so trusting the whole bridge (`172.17.0.0/16`) lets every other
container on it pick its own source. A range wider than one network's
proxies could be — shorter than `/8` for IPv4 or `/16` for IPv6, `0.0.0.0/0`
above all — would let clients name their own sources and switch the limit off,
and the server does not start with one.

Loopback is trusted by default because a proxy on the same host is what the
docs recommend, but not everything that connects over loopback is a proxy
that appends. A TCP relay on loopback passes the client's own
`X-Forwarded-For` through untouched, and the client then picks its source.
Examples are a service-mesh sidecar, `ssh -L`, `socat`, stunnel and
`kubectl port-forward`. If one sits in front of the server, set
`TRACEPAD_TRUSTED_PROXIES=none`, or name only the real proxy's address.

Applications then export to `https://traces.example.com/v1/traces`, and the
CLI takes the same address as `--url`. The server believes
`X-Forwarded-Proto` from whoever sends it, which is one more reason for the
loopback publish: only the proxy should be able to.

## Health

The image declares a `HEALTHCHECK`, so Docker knows whether the server is up:

```sh
docker inspect --format '{{.State.Health.Status}}' tracepad   # healthy; starting until the first check passes
```

It runs [`tracepad health`](cli.md#health), which needs no key — the same
command works from a load balancer, a systemd unit or your own script:

```sh
tracepad health --url http://tracepad.internal:4318
```

A bug in one write or one background pass does not stop the server. The
writer recovers a panic in a job it is applying, and the retention sweeper, the
statistics rollup and the erasure worker recover one in a project's, an hour's
or a run's work: an `ERROR` line, `a panic was recovered`, says where and
carries the stack; the write that hit it is answered with an error and refused
alone, and the others in its window commit; the next tick tries the project
again, and gives up on one that panics three passes in a row until the next
start, which [`GET /api/v1/system`](api.md#system) shows the admin token or an owner in `worker_panics`. The line carries the panic's own words only when it is a runtime fault (an
index out of range, a nil pointer), never a value the code chose to panic with,
so the stack is what to report. A panic anywhere else — the writer's own loop, the WAL checkpoint —
ends the process, as a Go program's does; run the server under something that
restarts it, such as `restart: unless-stopped` below.

## Compose

The image is published under three kinds of tag: an exact release (`0.1.0`),
its minor line (`0.1`, which moves with each patch) and `latest` (the newest
stable release). A pre-release such as `0.1.0-rc.1` has its exact tag and
nothing else. A deployment that wants patches without surprises pins the minor
line; one that wants to choose the day pins the exact version.

```yaml
services:
  tracepad:
    image: ghcr.io/tracepad/tracepad:0.1    # a minor line: it moves with its patches; X.Y.Z pins one release
    restart: unless-stopped
    ports:
      - "127.0.0.1:4318:4318"
    volumes:
      - tracepad:/data
    environment:
      TRACEPAD_ADMIN_TOKEN_FILE: /run/secrets/tracepad_admin_token
      # Behind a TLS proxy, its address as this container sees it (below).
      # TRACEPAD_TRUSTED_PROXIES: 172.18.0.1
    secrets: [tracepad_admin_token]

secrets:
  tracepad_admin_token:
    file: ./admin-token     # not in git; made as below

volumes:
  tracepad:
```

Behind a [TLS proxy](#serving-over-tls), name it in `TRACEPAD_TRUSTED_PROXIES`,
or the server ignores its `X-Forwarded-*` headers: session cookies lose
`Secure`, and the log says so once an hour. Compose puts the stack on a network
of its own, so a proxy on the host reaches the container from that network's
gateway rather than `172.17.0.1`: `docker network inspect <project>_default`
shows it. A proxy in another service of the same stack has an address Compose
picks at each start; give the network a fixed `subnet` and the proxy a fixed
`ipv4_address`, and name that address.

Compose mounts the file as it is on the host, owner and mode included, so make
it the way the `docker run` example above does — readable by the image's user
and by nobody else:

```sh
( umask 077; openssl rand -hex 32 > admin-token )
sudo chown 65532 admin-token
```

`docker compose logs tracepad` is where the first run's keys are — and where
they stay, which is why [the first key is worth rotating](#the-keys-are-printed-once-to-the-log).

## Stopping

A `docker stop` gives the server ten seconds: it drains open requests for five,
waits up to three more for the handlers it interrupted, closes its database
and exits. An MCP client's open stream is ended as the stop begins, so it does
not hold the drain. A second signal while it stops kills the process at once. Those windows are fixed and fit inside the default, so a longer
`--stop-timeout` (Compose: `stop_grace_period`) changes nothing: a response
still being written after five seconds is cut off either way.

## Upgrading, and backing up first

Schema migrations run on start, as they do on a host: a new image opens the
existing `/data` and brings it forward. **A downgrade is not supported** — an
older binary meeting a newer schema is not a case anything here handles — so
the upgrade is only as safe as the copy you took before it.

**A container named `tracepad-<project>`, publishing on `127.0.0.1` only** —
the one a coding agent starts in a setup — is upgraded by
[`tracepad upgrade`](cli.md#upgrade) on the host (`--container NAME` for any
other name), or by the agent, given
`Update Tracepad to the latest release: follow https://tracepad.github.io/tracepad/agent-upgrade.md`.
It pulls the new tag, stops the container, archives the volume into
`~/tracepad-backups/<run>/` as below, renames the old container
`<name>-before-<run>` with its restart policy set to `no`, and runs the new
image with the same mounts, ports, restart policy and labels, and the
variables you set (not the old image's own), through an env file. A new
version that exits, crash-loops or answers as another version goes back at
once (one that runs but stays silent is left for you to decide): the archive is restored into a
new volume, `<volume>-<run>`, and the old image runs on it; the volume the new
version migrated is left as it was. Nothing is removed — not a container, not
a volume — and the report gives the commands for what is set aside. Like any
backup, the run directory holds every prompt and completion and the
container's variables until you delete it, and an erasure does not reach it.
Compose, a container open beyond this machine, and anything the command cannot
reproduce exactly (privileges, devices, a network of its own, resource
limits) are yours, as follows.

Back the volume up by tarring it from a throwaway container:

```sh
docker stop tracepad
docker run --rm -v tracepad:/data -v "$PWD:/backup" busybox \
  sh -c 'umask 077 && tar czf /backup/tracepad-$(date +%F).tar.gz -C /data .'
docker pull ghcr.io/tracepad/tracepad:X.Y.Z        # the release you are moving to
docker rm -f tracepad && docker run -d --name tracepad … ghcr.io/tracepad/tracepad:X.Y.Z
```

Stopping first matters: SQLite's write-ahead log is part of the database, and a
tar of a live one is a copy of a file mid-write. `umask 077` makes the archive
readable by its owner alone; it is the whole database, and without it the file
lands in your directory as readable as that directory lets it be. Restoring is
the same command with the arguments swapped, into a stopped container's volume.

**One server per volume.** The server holds a lock on `tracepad.db.lock`, beside the
database, for as long as it runs, and a second one started on the same
directory exits at once, telling you a server is already running there and the
pid that last recorded itself in the file, which may be stale (a crash leaves
its pid behind, and two containers sharing a volume are both pid 1). That is
what a `docker run` that overlaps the container it replaces, or a second
`tracepad serve` on the same `--data-dir`, meets: stop the first, or give the
second a directory of its own. A server that was killed leaves the database
free at once; the file it leaves behind holds nothing that matters and is safe
to delete or to include in a backup. On a file system that cannot lock at all
(NFS without a lock daemon, some FUSE volumes) the server starts anyway and
says in a `WARN` that nothing guards the directory: do not run two.

**The server keeps a copy of its own, too.** Before every start that applies a
migration it writes `tracepad.db.pre-<NNNN>_<name>.bak` beside the database — a
full copy as it stood before the upgrade, readable by its owner only, for
rolling that upgrade back by swapping the file in. Once the upgrade's
migrations have committed, the server removes the backups of earlier upgrades,
naming each in its log; the sweeper removes the newest seven days after it was
written. That week is the window for a rollback — and the copy holds
everything erased or swept since, as does any tar of the volume taken in it.
Once the upgrade has proved itself you can remove it sooner. This removes
every backup the server wrote, names each one, and does nothing on a volume
that has none:

```sh
docker run --rm -v tracepad:/data busybox \
  find /data -maxdepth 1 -name 'tracepad.db.pre-[0-9][0-9][0-9][0-9]_*.bak' -print -exec rm {} \;
```

The server deletes only files named exactly that way — `pre-`, the four-digit
number of a migration, an underscore and its name — so a copy you take yourself
is safe from it under any other name. Better still, keep it outside the data
directory (the tar above does); if it must sit beside the database, call it
anything but `pre-<NNNN>_<name>.bak`: `tracepad.db.pre-upgrade-<sha>.bak` is
fine, `tracepad.db.pre-0035_mine.bak` would be taken for the server's own.

A data-subject erasure does not rewrite this file; while it exists, the
erasure's answer names it and the day it goes — see
[retention.md](retention.md#what-this-means-for-a-data-subject-request).

## Building it yourself

The `Dockerfile` is the whole recipe — it builds the interface and the binary
from the sources in the checkout, so a fork or a patched tree produces the same
kind of artifact the releases do:

```sh
docker build --build-arg VERSION=0.1.0-mine -t tracepad:mine .
```

Without `--build-arg VERSION`, the image reports `dev`. `make image` and `make
image-check` are the same build and the boot test CI runs against it.
