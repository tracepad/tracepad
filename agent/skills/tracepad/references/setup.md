# Setting Tracepad up for a project

From nothing to a trace you have read back: a server, a key, the application
connected, and a report to the human. Each step says what a healthy answer
looks like. Anything else is a reason to stop and tell the human, not to improvise.

## 1. Is there a server?

```sh
tracepad version
tracepad health
```

- `version` prints a release (`X.Y.Z`) or a candidate (`X.Y.Z-rc.N`): installed.
  `dev` is a build from a checkout: it serves, but has no image for Docker.
  *command not found* (try `~/.local/bin/tracepad` first, often off `PATH`):
  install with `curl -fsSL https://tracepad.github.io/tracepad/install.sh | sh`.
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
server now, it listens on loopback only (`localhost`), and it holds no data
yet. On any other server the key is the human's to give (SKILL.md,
*Connecting*). Check, and check that `.env` can hold a secret:

```sh
data="${TRACEPAD_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/tracepad}"
test -e "$data/tracepad.db" && echo "NOT FRESH"
git ls-files --error-unmatch .env >/dev/null 2>&1 && echo "TRACKED"
git check-ignore -q .env || echo "NOT IGNORED"
```

- `NOT FRESH` (for Docker: `docker volume inspect tracepad-<project>`
  succeeds): an earlier install. Ask whether to start that one, whose key is
  the human's to give, or to give this project a new data directory.
- `TRACKED`: write no key into `.env`; ask where it should go.
- `NOT IGNORED`: add `.env` to `.gitignore`, creating it if missing, before
  writing `.env`, and say so in the report. (Outside a git repository it prints too.)

Then one command starts it and writes `.env`, each line replaced in place,
the secret never printed. Its first line holds four choices. `port` is step
1's; the server is told it, so its printed links carry it. `declare=no`, for a
human who would rather you made no key, takes the `ingest` key the first start
prints instead: it cannot read, so step 6 waits for a `read` key the human
mints (Settings → Project → API keys), and until then step 4's `200` is the
only check. `how=docker` when the human asks for a container (the image is the
binary's release; `dev` has none). `via` is step 2's path: `package`, `otel`, `langfuse`.

```sh
port=4318; declare=yes; how=binary; via=package
export PATH="$HOME/.local/bin:$PATH"; umask 077; url="http://localhost:$port"; tag="$(tracepad version)"
data="${TRACEPAD_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/tracepad}"; mkdir -p "$data"; n="$(cat "$data/server.log" 2>/dev/null | wc -l)"
put() { { grep -v "^$1=" .env 2>/dev/null; printf '%s=%s\n' "$1" "$2"; } >.env.new && mv .env.new .env; }
project="$(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)" | tr -c 'A-Za-z0-9._\n-' '-' | sed 's/^[^A-Za-z0-9]*//')"; name="tracepad-${project:-app}"
decl=""; [ "$declare" = yes ] && sk="tp-sk-$(openssl rand -hex 32)" && pk="tp-pk-$(openssl rand -hex 16)" && decl="${project:-app}:$pk:$sk"
if [ "$how" = docker ]; then
  case "$tag" in [0-9]*.[0-9]*.[0-9]*) ;; *) echo "STOP: the image needs a release, and tracepad version says '$tag'"; exit 1 ;; esac
  docker container inspect "$name" >/dev/null 2>&1 && { echo "STOP: a container named $name is there already (docker ps -a): the human's to keep or remove"; exit 1; }
  made=; docker volume inspect "$name" >/dev/null 2>&1 || made=yes; logs() { docker logs "$name" 2>&1; }; alive() { true; }
  stop() { docker rm -f "$name" >/dev/null 2>&1; [ -z "$made" ] || docker volume rm "$name" >/dev/null; }
  docker run -d --name "$name" -v "$name:/data" -p "127.0.0.1:$port:4318" -e TRACEPAD_URL="$url" -e TRACEPAD_PROJECTS="$decl" "ghcr.io/tracepad/tracepad:$tag" serve >/dev/null || { stop; exit 1; }
else
  TRACEPAD_URL="$url" TRACEPAD_PROJECTS="$decl" nohup tracepad serve --listen "localhost:$port" --data-dir "$data" >>"$data/server.log" 2>&1 &
  echo $! >"$data/server.pid.new"; logs() { cat "$data/server.log"; }
  alive() { kill -0 "$(cat "$data/server.pid.new")" && tail -n "+$((n + 1))" "$data/server.log" | grep -q 'listening addr'; }; stop() { kill "$(cat "$data/server.pid.new")"; rm "$data/server.pid.new"; }
fi
for i in 1 2 3 4 5 6 7 8 9 10; do sleep 1; tracepad health --url "$url" >/dev/null 2>&1 && break; done
alive && tracepad health --url "$url" || { logs | tail -n 5; stop; exit 1; }
[ "$how" = docker ] || mv "$data/server.pid.new" "$data/server.pid"
[ "$declare" = yes ] || { sk="$(logs | sed -n 's/^ *OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer \(tp-sk-[^"]*\)"$/\1/p' | tail -n 1)"; pk="$(logs | sed -n 's/^ *LANGFUSE_PUBLIC_KEY=\(tp-pk-[^ ]*\)$/\1/p' | tail -n 1)"; }
[ -n "$sk" ] && [ -n "$pk" ] || { echo "STOP: no key in this server's log"; exit 1; }
put TRACEPAD_URL "$url"; put TRACEPAD_API_KEY "$sk"; [ "$via" != langfuse ] || put LANGFUSE_PUBLIC_KEY "$pk"
```

Healthy: `{"version":"…","ok":true}`. Otherwise its log's last lines say why
(*address already in use*: another port; Docker says its own), the server is
stopped — a container that failed is removed, with the volume it made, so the
next try takes the same name — and `.env` untouched. The project is named after the repository.

Then the lines of `via`, from `.env`. On a server you did not start, the human
puts `TRACEPAD_URL`, the key and, for Langfuse, `LANGFUSE_PUBLIC_KEY` there:

```sh
via=package; umask 077
put() { { grep -v "^$1=" .env 2>/dev/null; printf '%s=%s\n' "$1" "$2"; } >.env.new && mv .env.new .env; }
url="$(sed -n 's/^TRACEPAD_URL=//p' .env)"; sk="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"; pk="$(sed -n 's/^LANGFUSE_PUBLIC_KEY=//p' .env)"
[ "$via" != otel ] || { put OTEL_EXPORTER_OTLP_PROTOCOL http/protobuf; put OTEL_EXPORTER_OTLP_TRACES_ENDPOINT "$url/v1/traces"; put OTEL_EXPORTER_OTLP_HEADERS "\"authorization=Bearer $sk\""; }
[ "$via" != langfuse ] || { [ -n "$pk" ] || { echo "STOP: no LANGFUSE_PUBLIC_KEY in .env"; exit 1; }; put LANGFUSE_BASE_URL "$url"; put LANGFUSE_HOST "$url"; put LANGFUSE_SECRET_KEY "$sk"; }
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

Section 3 of `instrumenting.md`; the package's own page has the shapes. Add
the package with the project's own tool, so it is in the manifest; a candidate
binary's only when pinned: `tracepad==X.Y.ZrcN`, `tracepad@X.Y.Z-rc.N`, `…/sdk/go@vX.Y.Z-rc.N`.
The SDKs read the process's environment, not `.env`: run through the
project's loader (dotenv, `node --env-file=.env`) or after `set -a; . ./.env;
set +a;`. Run the smallest path that calls a model; the provider's key is the
human's to give, never to look for. A short-lived process flushes first.

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

- **The interface**: the setup link, `grep -o 'http[^ ]*/setup#token=[^ ]*'`
  over the log. They open it and choose an email and a password (24 hours, or
  until a restart); you never do. After that, `.env`'s `TRACEPAD_URL`.
- **The key**: `.env`'s `TRACEPAD_API_KEY` (and the lines you added), for the
  project you named. It reads too; before production, each application gets
  an `ingest`-only key of its own (Settings → Project → API keys).
- **The server**: you started it. Log `$data/server.log`, stopped by `kill
  "$(cat "$data/server.pid")"`; a container's are `docker logs` and `docker stop`
  `tracepad-<project>`. It does not outlive a reboot; a service does
  (https://tracepad.github.io/tracepad/latest/install/).
- **What changed**: every file you edited, `.gitignore` included, and the
  test trace `tracepad-setup-check`.
- **What is theirs**: the account, a provider key, keeping the server running,
  opening its port beyond this machine, and anything that deletes (SKILL.md).
