# Agent setup

This page is an instruction to a coding agent (Claude Code, Codex, Cursor or
any other) that a person has asked to set Tracepad up for the project in front
of it. A person reading it learns what the agent will do and what it will ask
of them. The [quickstart](quickstart.md) covers the same ground by hand.

**The goal**: a Tracepad server running on this machine, the project's
application sending its traces to it, a trace you have read back yourself,
and a report to the person. Work in order. Each step says what a healthy
answer looks like. Anything else is a reason to stop and tell the person, not
to improvise.

## 1. The binary

```sh
tracepad version
```

A release (`0.1.0`) or a release candidate (`0.1.0-rc.1`) means it is
installed: go to step 2. *command not found*, or a commit hash (a development
build), means installing the binary and the agent skill, together, verified,
with no `sudo`:

```sh
curl -fsSL https://tracepad.github.io/tracepad/install.sh | sh
```

Healthy output ends like this:

```
installed tracepad 0.1.0 at /home/you/.local/bin/tracepad
  verified  sha256 matches checksums.txt; attestation not checked: gh is not installed
  skill     installed 0.1.0 to /home/you/.claude/skills/tracepad

Next, paste this into your coding agent (Claude Code, Codex, Cursor, …):

  Set up Tracepad for this project: follow https://tracepad.github.io/tracepad/agent-setup.md
```

- *there is no stable release of Tracepad yet*: the message names the newest
  release candidate and the line that installs it, with `TRACEPAD_VERSION`.
  Run that line.
- *`~/.local/bin` is not on your PATH*: call the binary as
  `~/.local/bin/tracepad` for the rest of this session. Adding the directory
  to the person's shell profile is theirs to do.
- *Another tracepad comes first on your PATH*: an older binary shadows the new
  one. Call the new one by its full path for the rest of this session, and
  report the other.
- Any other line starting `tracepad install:` is a failure, with its reason. Nothing
  was installed. Report it rather than working around it: a checksum or an
  attestation that does not match is never retried with the check turned off.

The variables the script reads are in
[configuration.md](configuration.md#the-install-script).

## 2. The procedure

The rest of the steps, with their commands and their healthy answers, are in
the agent skill the binary carries. Its version matches the binary's, so the
commands are the ones this version has. Print it and follow it:

```sh
tracepad skills show setup.md
```

If your agent loaded the `tracepad` skill, this is the same file as its
`references/setup.md`. A binary older than this page (`0.1.0-rc.1`) answers
*the skill has no file "setup.md"*. Read the file from the repository
instead, and install the skill again once a newer binary is in place:

```sh
curl -fsSL https://raw.githubusercontent.com/tracepad/tracepad/main/agent/skills/tracepad/references/setup.md
```

In outline:

1. **Is there a server?** `tracepad health`. A server you did not start means
   asking the person for its URL and a key, and starting no second one.
2. **Look at the project**, and choose how it will send (the table below).
3. **Start a server and declare its key** (on `4318`, or a free port when
   something else holds it), but only when you start it now,
   on loopback, on a data directory with no database yet. The key goes from
   the generator into `.env`, never onto the screen, after `.env` is
   confirmed ignored by git. On any other server the key comes from the
   person.
4. **Send a test span** with `curl`. `200` means it is stored.
5. **Connect the application** and run one path that calls a model.
6. **Read both back** with `tracepad traces ls` and `tracepad traces last`.
7. **Report** (below).

## Choosing the path

What the project already has decides how it sends. One exporter per process,
so look before you add (`tracepad skills show instrumenting.md`, sections 1
and 2):

| The project has | It sends through | The shapes are in |
|---|---|---|
| OpenTelemetry already, in any language | its own exporter, configured by environment alone (`OTEL_EXPORTER_OTLP_*`) | [ingest.md](ingest.md) |
| The Langfuse SDK | the same SDK, pointed at Tracepad (`LANGFUSE_*`) | [langfuse-sdk.md](langfuse-sdk.md) |
| Neither, in Python (`pyproject.toml`, `requirements*.txt`) | the `tracepad` package | [sdk-python.md](sdk-python.md) |
| Neither, in Node (`package.json`, Node 22 or newer) | the `tracepad` package | [sdk-js.md](sdk-js.md) |
| Neither, in Go (`go.mod`) | the `tracepad` module | [sdk-go.md](sdk-go.md) |
| Neither, in another language | that language's OpenTelemetry SDK over OTLP/HTTP | [ingest.md](ingest.md) |

## With Docker instead of the binary

When the person asks for Docker, or the binary cannot run here, run the
server as a container. You still install the binary for its CLI and its
skill. The conditions of step 3 apply the same way: "fresh" means the volume
does not exist yet (`docker volume inspect tracepad` fails), and the port is
published on loopback only. This command takes the place of step 3's start
command, with the same `port` and `declare`. The image is named by the
binary's version, because before 0.1.0 an untagged image does not exist:

```sh
port=4318; declare=yes
umask 077; put() { { grep -v "^$1=" .env 2>/dev/null; printf '%s=%s\n' "$1" "$2"; } >.env.new && mv .env.new .env; }
project="$(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)" | tr -c 'A-Za-z0-9._\n-' '-' | sed 's/^[^A-Za-z0-9]*//')"
decl=""; [ "$declare" = yes ] && sk="tp-sk-$(openssl rand -hex 32)" && decl="${project:-app}:tp-pk-$(openssl rand -hex 16):$sk"
docker run -d --name tracepad -v tracepad:/data -p "127.0.0.1:$port:4318" -e TRACEPAD_PROJECTS="$decl" \
  "ghcr.io/tracepad/tracepad:$(tracepad version)" >/dev/null \
  || { echo "docker run failed: port $port is taken, or a container named tracepad exists"; exit 1; }
sleep 3; tracepad health --url "http://localhost:$port" || { docker logs tracepad 2>&1 | tail -n 5; exit 1; }
[ "$declare" = yes ] || sk="$(docker logs tracepad 2>&1 | sed -n 's/^ *OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer \(tp-sk-[^"]*\)"$/\1/p' | tail -n 1)"
put TRACEPAD_URL "http://localhost:$port"; put TRACEPAD_API_KEY "$sk"
```

After it, the log, with the setup link and the public key, is
`docker logs tracepad 2>&1`: read the Langfuse public key from there in the
block that writes the path's lines. `docker stop tracepad` stops the server.
A declared key is part of the container's configuration, so whoever can run
`docker inspect` on this machine can read it. Say so in the report.
[docker.md](docker.md) covers the volume and upgrades.

## What needs the person

You ask for these and do not do them yourself:

- A model provider's API key (`OPENAI_API_KEY` and its kin), for the run of
  step 5. You do not look for one in files or history.
- A Tracepad key for a server you did not start.
- Making the server reachable from other machines (`--listen :4318`, a proxy,
  TLS): it speaks plain HTTP ([docker.md](docker.md#serving-over-tls)).
- Creating the account in the browser, through the setup link.
- Anything that deletes: traces, a project, a data directory, a volume.
- Keeping the server running past this session: a service
  ([install.md](install.md#as-a-service)) or a container with a restart
  policy.
- Replacing a variable that pointed somewhere else (Langfuse Cloud, another
  collector), because the application stops sending there.

## The report

The person reads this, not your transcript. It says:

- **Where to look**: the setup link from the server's log, which they open to
  choose an email and a password. After that, the server's address, the
  `TRACEPAD_URL` in `.env`.
- **Where the key is**: `.env`, as `TRACEPAD_API_KEY` and the lines you
  added, for which project. Before production each application gets its own
  `ingest`-only key (Settings → Project → API keys).
- **The server**: that you started it, its PID file and log, how to stop it,
  and that it does not outlive a reboot.
- **What you changed**: every file, `.gitignore` included, and the test trace
  `tracepad-setup-check`.
- **What is left for them**: the items above that apply, and anything that
  did not pass, with the answer you got.
