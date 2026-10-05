# Setting Tracepad up for a project

From nothing to a trace you have read back: a server, a key, the application
connected, and a report to the human. Each step says what a healthy answer
looks like. Anything else is a reason to stop and tell the human, not to improvise.

## 1. Is there a server?

```sh
tracepad version
tracepad health
```

- `version` prints one, such as `0.1.0`. *command not found* means the binary is
  missing, or `~/.local/bin` is not on `PATH` (try `~/.local/bin/tracepad`). A
  commit hash, or a version older than `0.1.0`, is another build. In both cases
  install, then call the new binary by its full path:
  `curl -fsSL https://tracepad.github.io/tracepad/install.sh | sh`.
- `health` prints `{"version":"…","ok":true}`: a server is running and you did
  not start it. Ask the human for its URL and a key (`ingest` for the
  application, `read` as well to check the result), skip step 3, and start
  no second server.
- *cannot reach … connection refused*: there is no server. You start one in step 3.
- Any other answer means another program holds the port: pick a free one for
  step 3's `port=`.

## 2. Look at the project

Sections 1 and 2 of `tracepad skills show instrumenting.md` find what already
traces the service, and so its path: environment-only OpenTelemetry, the
Langfuse bridge, or the `tracepad` package for its language. The path decides
the lines `.env` gets next. A variable `.env` already sets is replaced, not
repeated. If it pointed elsewhere (Langfuse Cloud, another collector), ask
first, because the application stops sending there.

## 3. Start a server, with a key you declare

This is the one place you create a key. All three must hold: you start the
server now, it listens on loopback only (`localhost`), and its data directory
has no database yet. On any other server the key is the human's to give
(SKILL.md, *Connecting*). Check, and check that `.env` can hold a secret:

```sh
data="${TRACEPAD_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/tracepad}"
test -e "$data/tracepad.db" && echo "NOT FRESH"
git ls-files --error-unmatch .env >/dev/null 2>&1 && echo "TRACKED"
git check-ignore -q .env || echo "NOT IGNORED"
```

- `NOT FRESH`: an earlier install. Ask whether to start that one, whose key is
  the human's to give, or to give this project a new data directory.
- `TRACKED`: write no key into `.env`; ask where it should go.
- `NOT IGNORED`: add `.env` to `.gitignore`, creating it if missing, before
  writing `.env`, and say so in the report. (Outside a git repository it prints too.)

Then run one command, so that the secret goes from the generator into `.env`
and the server and is never printed. Of the two `printf` lines marked *only*,
keep the one for step 2's path; the `tracepad` package needs neither:

```sh
port=4318
data="${TRACEPAD_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/tracepad}"
project="$(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)" | tr -c 'A-Za-z0-9._\n-' '-' | sed 's/^[^A-Za-z0-9]*//')"
pk="tp-pk-$(openssl rand -hex 16)"; sk="tp-sk-$(openssl rand -hex 32)"
mkdir -p "$data" && chmod 700 "$data"
printf 'TRACEPAD_URL=http://localhost:%s\nTRACEPAD_API_KEY=%s\n' "$port" "$sk" >>.env
# only on the OpenTelemetry path:
printf 'OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf\nOTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:%s/v1/traces\nOTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer %s"\n' "$port" "$sk" >>.env
# only on the Langfuse path:
printf 'LANGFUSE_BASE_URL=http://localhost:%s\nLANGFUSE_HOST=http://localhost:%s\nLANGFUSE_PUBLIC_KEY=%s\nLANGFUSE_SECRET_KEY=%s\n' "$port" "$port" "$pk" "$sk" >>.env
TRACEPAD_PROJECTS="${project:-app}:$pk:$sk" nohup tracepad serve --listen "localhost:$port" --data-dir "$data" >>"$data/server.log" 2>&1 &
echo $! >"$data/server.pid"
```

A few seconds later, `tracepad health --url "http://localhost:$port"` prints
`{"version":"…","ok":true}`. Without `--url` it asks `localhost:4318`, which on
another port is someone else. The project is named after the repository. The
key holds all three scopes, and its secret is in no log (the public key is).
The log holds the setup link for the human (step 7).

**Fallback**, when the human would rather you created no key: start the same
server without `TRACEPAD_PROJECTS`. The first start prints an `ingest` key;
move it into `.env` unseen:

```sh
sed -n 's/^ *OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer \(tp-sk-[^"]*\)"$/TRACEPAD_API_KEY=\1/p' "$data/server.log" >>.env
```

That key sends but cannot read. Step 6 waits for a `read` key the human mints
(Settings → Project → API keys) and hands you; until then step 4's `200` is
the only check, and the report says so.

## 4. A test span

The CLI and `curl` take the two variables from `.env`, in every new shell (a
shell without them asks `localhost:4318` with no key), never typed out:

```sh
export TRACEPAD_URL="$(sed -n 's/^TRACEPAD_URL=//p' .env)" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"
now="$(date +%s)000000000"
curl -sS -o /dev/null -w '%{http_code}\n' "$TRACEPAD_URL/v1/traces" \
  -H "Authorization: Bearer $TRACEPAD_API_KEY" -H 'Content-Type: application/json' \
  -d '{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"'"$(openssl rand -hex 16)"'","spanId":"'"$(openssl rand -hex 8)"'","name":"tracepad-setup-check","kind":1,"startTimeUnixNano":"'"$now"'","endTimeUnixNano":"'"$now"'"}]}]}]}'
```

`200`: committed. `401`: the key is not this server's. `403`: it lacks
`ingest`. *Connection refused*: the server stopped (read `$data/server.log`).

## 5. Connect the application, and run it once

Section 3 of `instrumenting.md`, and the package's own page for the shapes.
Add the package with the project's own tool, so it lands in its manifest.
Until 0.1.0 the packages are candidates as well: `tracepad==0.1.0rc1` (pip,
uv), `tracepad@next` (npm), `github.com/tracepad/tracepad/sdk/go@v0.1.0-rc.1`.
The packages and the OpenTelemetry SDKs read the process's environment, not
`.env`: run through the project's loader (dotenv, `node --env-file=.env`), or
prefix the command with `set -a; . ./.env; set +a;`. Run the smallest path
that calls a model. The provider's key (`OPENAI_API_KEY` and its kin) is the
human's: when it is not set, ask, and do not look for one. A short-lived
process flushes before it exits.

## 6. Read both back

With step 4's `export` line first, in the same command:

```sh
tracepad traces ls --since 15m --limit 5
tracepad traces last --since 15m --full
```

Healthy: `tracepad-setup-check` and the application's trace are both listed,
and its generation carries a model and token usage. With only the test span
after half a minute, work through section 4 of `instrumenting.md`.

## 7. Report to the human

- **The interface**: the setup link, `grep -o 'http[^ ]*/setup#token=[^ ]*' "$data/server.log" | tail -n 1`.
  They open it and choose an email and a password; it works for 24 hours or
  until a restart. You never open it. After that, the `TRACEPAD_URL` in `.env`.
- **The key**: `.env`'s `TRACEPAD_API_KEY` (and the lines you added), for the
  project you named. It reads too; before production, each application gets
  an `ingest`-only key of its own (Settings → Project → API keys).
- **The server**: you started it; PID in `$data/server.pid`, log in
  `$data/server.log`, stopped by `kill "$(cat "$data/server.pid")"`. It does
  not outlive a reboot: a service or a container keeps it
  (https://tracepad.github.io/tracepad/install/).
- **What changed**: every file you edited, `.gitignore` included, and the
  test trace `tracepad-setup-check`.
- **What is theirs**: the account, a provider key, keeping the server running,
  opening its port beyond this machine, and anything that deletes (SKILL.md).
