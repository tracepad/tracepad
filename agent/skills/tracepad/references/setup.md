# Setting Tracepad up for a project

From nothing to a trace you have read back: a server, a key, the application
connected, and a report to the human. Each step says what a healthy answer
looks like. Anything else is a reason to stop and tell the human, not to improvise.

## 1. Is there a server?

```sh
tracepad version
tracepad health
```

- `version` prints a release (`0.1.0`) or a candidate (`0.1.0-rc.1`): installed.
  *command not found* (try `~/.local/bin/tracepad` first, often off `PATH`) or
  a commit hash (a development build): install with
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

Then one command starts it with `port` from step 1, and writes `TRACEPAD_URL`
and `TRACEPAD_API_KEY` into `.env`: each line replaced where it is, never
repeated, and the secret never printed. `declare=yes` declares the key;
`declare=no` is for a human who would rather you did not, and moves the
`ingest` key the first start prints instead. That key sends but cannot read,
so step 6 waits for a `read` key the human mints (Settings → Project → API
keys), and until then the report says step 4's `200` is the only check.

```sh
port=4318; declare=yes
export PATH="$HOME/.local/bin:$PATH"; umask 077
data="${TRACEPAD_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/tracepad}"; mkdir -p "$data"
put() { { grep -v "^$1=" .env 2>/dev/null; printf '%s=%s\n' "$1" "$2"; } >.env.new && mv .env.new .env; }
project="$(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)" | tr -c 'A-Za-z0-9._\n-' '-' | sed 's/^[^A-Za-z0-9]*//')"
decl=""; [ "$declare" = yes ] && sk="tp-sk-$(openssl rand -hex 32)" && decl="${project:-app}:tp-pk-$(openssl rand -hex 16):$sk"
TRACEPAD_PROJECTS="$decl" nohup tracepad serve --listen "localhost:$port" --data-dir "$data" >>"$data/server.log" 2>&1 &
echo $! >"$data/server.pid.new"; sleep 3
kill -0 "$(cat "$data/server.pid.new")" && tracepad health --url "http://localhost:$port" || { rm "$data/server.pid.new"; tail -n 5 "$data/server.log"; exit 1; }
mv "$data/server.pid.new" "$data/server.pid"; [ "$declare" = yes ] || sk="$(sed -n 's/^ *OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer \(tp-sk-[^"]*\)"$/\1/p' "$data/server.log" | tail -n 1)"
put TRACEPAD_URL "http://localhost:$port"; put TRACEPAD_API_KEY "$sk"
```

Healthy: `{"version":"…","ok":true}`. Otherwise the log's last lines say why
(*address already in use*: another port), and `.env` is untouched. The project
is named after the repository; the log has its public key and the setup link.

Then the lines of step 2's path, from `.env` (the `tracepad` package needs
none: keep only the line of yours). On a server you did not start, the human
puts `TRACEPAD_URL` and the key into `.env`, and this block does the rest:

```sh
put() { { grep -v "^$1=" .env 2>/dev/null; printf '%s=%s\n' "$1" "$2"; } >.env.new && mv .env.new .env; }
url="$(sed -n 's/^TRACEPAD_URL=//p' .env)"; sk="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"; umask 077
pk="$(sed -n 's/^ *LANGFUSE_PUBLIC_KEY=\(tp-pk-[^ ]*\)$/\1/p' "${TRACEPAD_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/tracepad}/server.log" | tail -n 1)"
put OTEL_EXPORTER_OTLP_PROTOCOL http/protobuf; put OTEL_EXPORTER_OTLP_TRACES_ENDPOINT "$url/v1/traces"; put OTEL_EXPORTER_OTLP_HEADERS "\"authorization=Bearer $sk\""
put LANGFUSE_BASE_URL "$url"; put LANGFUSE_HOST "$url"; put LANGFUSE_PUBLIC_KEY "$pk"; put LANGFUSE_SECRET_KEY "$sk"
```

## 4. A test span

The CLI and `curl` take the two variables from `.env`, in every new shell (a
shell without them asks `localhost:4318` with no key), never typed out:

```sh
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_URL="$(sed -n 's/^TRACEPAD_URL=//p' .env)" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"
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
