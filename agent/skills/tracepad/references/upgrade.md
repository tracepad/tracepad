# Upgrading Tracepad

From the installed version to the one the human asked for, with a backup first
and a way back, since migrations run forward only. Each step says what a healthy
answer looks like; anything else is a reason to stop and tell the human.

## 1. What is installed, and what runs it

```sh
export PATH="$HOME/.local/bin:$PATH"; command -v tracepad && tracepad version
ps -A -o pid= -o args= | grep '[t]racepad serve' | while read -r pid args; do
  d="$(printf '%s\n' "$args" | sed -n 's/.*--data-dir \([^ ]*\).*/\1/p')"
  if [ -n "$d" ] && [ "$(cat "$d/server.pid" 2>/dev/null)" = "$pid" ]; then echo "MINE pid=$pid data=$d $args"; else echo "OTHER pid=$pid $args"; fi
done
docker ps -a --format '{{.Names}} {{.Image}} {{.Ports}}' 2>/dev/null | grep 'tracepad/tracepad'
```

- `MINE`: a server `setup.md` started, its PID in its data directory's
  `server.pid`. Yours to upgrade (step 3) when its `--listen` is `localhost`.
- A container named `tracepad-<project>`, this repository's name, publishing on
  `127.0.0.1` only: the one `setup.md` starts, yours (step 4).
- `OTHER`, any other container, Compose, a port open beyond this machine: the
  human's. It keeps running, and step 7 says what to hand them.
- Nothing running: only the binary, the skill and the package change. No
  binary at all: this is a setup, `setup.md`.
- `tracepad` outside `~/.local/bin` (Homebrew, a package): its manager
  upgrades it (`brew upgrade tracepad`), and that is the human's.

## 2. The version to go to

"The latest release" is the newest stable one; GitHub names it:

```sh
curl -fsSL https://api.github.com/repos/tracepad/tracepad/releases/latest | grep -o '"tag_name": *"v[^"]*"' | sed 's/.*"v//; s/"$//'
```

- *404*, nothing printed: no stable release yet. Say so, name the newest
  candidate (`…/releases?per_page=1`), and ask before taking it.
- A version the human named (`0.1.0-rc.2`, without the `v`) is taken as said.
- The same version as `tracepad version` and every server's `health`:
  nothing to upgrade; step 6 still checks the skill and the package.
- An **older** version: refuse. A migrated database is not opened by an
  older binary; the way back is the backup taken before that upgrade
  (https://tracepad.github.io/tracepad/install/#upgrading). A release is
  later than its candidates, `0.1.0` > `0.1.0-rc.2` > `0.1.0-rc.1`.

Read `https://github.com/tracepad/tracepad/releases/tag/v<version>` and tell
the human anything under *Changed* before going on.

## 3. Your own server

One command: the trace count, the old binary copied (unless the install script
already replaced it), the server stopped, its data directory archived, the new
binary started on the same port and directory. Fill in `to` and step 1's
`data`. With no room for the backup it stops, changing nothing.

```sh
to=0.1.0; data=/path/from/step/1
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"; umask 077
pid="$(cat "$data/server.pid")"; listen="$(ps -o args= -p "$pid" | sed -n 's/.*--listen \([^ ]*\).*/\1/p')"
case "$listen" in localhost:* | 127.0.0.1:*) export TRACEPAD_URL="http://$listen" ;; *) echo "STOP: pid $pid listens on '$listen', not this machine alone"; exit 1 ;; esac
from="$(tracepad health | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"; [ -n "$from" ] || { echo "STOP: $TRACEPAD_URL does not answer"; exit 1; }
bk="$HOME/tracepad-backups/$(date +%Y%m%d-%H%M%S)-$from"; mkdir -p "$bk" || exit 1
[ "$(df -Pk "$bk" | awk 'NR == 2 { print $4 }')" -gt "$(du -sk "$data" | cut -f 1)" ] || { echo "STOP: no room for a backup in $bk"; exit 1; }
tracepad system | sed -n 's/.*"traces":\([0-9]*\).*/\1/p' >"$bk/count"
[ "$(tracepad version)" != "$from" ] || cp "$(command -v tracepad)" "$bk/tracepad" || exit 1
printf "how=binary\ndata='%s'\nlisten=%s\nfrom=%s\nto=%s\n" "$data" "$listen" "$from" "$to" >"$bk/state" || exit 1
curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$to" TRACEPAD_NO_SKILL=1 sh || exit 1
kill "$pid"; for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
kill -0 "$pid" 2>/dev/null && { echo "STOP: pid $pid did not stop"; exit 1; }
tar -czf "$bk/data.tar.gz" -C "$data" . && wc -l <"$data/server.log" >"$bk/log-lines" || exit 1
nohup tracepad serve --listen "$listen" --data-dir "$data" >>"$data/server.log" 2>&1 &
echo $! >"$data/server.pid"; echo "backup: $bk"
```

An empty count means the key cannot read: the test span of step 5 is the check.

## 4. Your own container

The same for `tracepad-<project>`: the volume archived from the stopped
container, which is kept, renamed, for the way back; the new one gets its
volumes, ports, restart policy and `TRACEPAD_*` variables. A container Compose
manages stops here: its file is the human's.

```sh
to=0.1.0; name=tracepad-project
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_URL="$(sed -n 's/^TRACEPAD_URL=//p' .env)" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"; umask 077
compose="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$name")" || exit 1; [ -z "$compose" ] || { echo "STOP: Compose runs $name"; exit 1; }
vol="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$name")"; [ -n "$vol" ] || { echo "STOP: no volume at /data"; exit 1; }
from="$(tracepad health | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"; [ -n "$from" ] || { echo "STOP: $TRACEPAD_URL does not answer"; exit 1; }
bk="$HOME/tracepad-backups/$(date +%Y%m%d-%H%M%S)-$from"; mkdir -p "$bk" || exit 1
set -- --restart "$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$name")"
for m in $(docker inspect -f '{{range .Mounts}}{{if .Name}}{{.Name}}{{else}}{{.Source}}{{end}}:{{.Destination}} {{end}}' "$name"); do set -- "$@" -v "$m"; done
for p in $(docker inspect -f '{{range $k, $v := .HostConfig.PortBindings}}{{range $v}}{{.HostIp}}:{{.HostPort}}:{{$k}} {{end}}{{end}}' "$name"); do set -- "$@" -p "$p"; done
docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$name" | grep '^TRACEPAD_' >"$bk/env"
tracepad system | sed -n 's/.*"traces":\([0-9]*\).*/\1/p' >"$bk/count"
printf 'how=docker\nname=%s\nvol=%s\nfrom=%s\nto=%s\n' "$name" "$vol" "$from" "$to" >"$bk/state"
docker pull -q "ghcr.io/tracepad/tracepad:$to" && docker stop "$name" >/dev/null || exit 1
docker run --rm -v "$vol:/data" -v "$bk:/backup" busybox sh -c 'umask 077 && tar czf /backup/data.tar.gz -C /data .' || exit 1
docker rename "$name" "$name-$from" && docker run -d --name "$name" "$@" --env-file "$bk/env" "ghcr.io/tracepad/tracepad:$to" serve >/dev/null || exit 1
echo "backup: $bk"
```

Then the CLI: `curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$to" TRACEPAD_NO_SKILL=1 sh`.

## 5. Check, and the way back

Run it again after a wait while the log says a migration is in progress.

```sh
bk="$(dirname "$(ls "$HOME"/tracepad-backups/*/state | tail -n 1)")"; . "$bk/state"
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_URL="$(sed -n 's/^TRACEPAD_URL=//p' .env)" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"
for i in $(seq 1 30); do tracepad health >/dev/null 2>&1 && break; sleep 2; done
if [ "$how" = docker ]; then docker logs "$name" 2>&1 | head -n 1; else tail -n "+$(($(cat "$bk/log-lines") + 1))" "$data/server.log" | head -n 1; fi
v="$(tracepad health | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"; echo "health: ${v:-no answer}; traces: $(cat "$bk/count") before, $(tracepad system | sed -n 's/.*"traces":\([0-9]*\).*/\1/p') now"
[ "$v" = "$to" ] || { echo "NOT HEALTHY: no answer as $to"; exit 1; }
```

Healthy: the log's first line names the new version and its commit (*tracepad
0.1.0 (a1b2c3d)*), `health` says it, and the count has not dropped. Then
`setup.md`'s step 4 test span answers `200`, and its step 6 reads it back.

**The way back**, at once on *NOT HEALTHY*. When the server runs but a check
looks wrong, ask first: going back drops what arrived since. The server's own
`tracepad.db.pre-<NNNN>_<name>.bak` beside the database holds the same state.

```sh
bk="$(dirname "$(ls "$HOME"/tracepad-backups/*/state | tail -n 1)")"; . "$bk/state"; umask 077
[ -s "$bk/data.tar.gz" ] || { echo "STOP: no archive in $bk, so nothing to go back to"; exit 1; }
if [ "$how" = docker ]; then
  docker rm -f "$name" >/dev/null; docker run --rm -v "$vol:/data" -v "$bk:/backup" busybox sh -c 'find /data -mindepth 1 -delete && tar xzf /backup/data.tar.gz -C /data'
  docker rename "$name-$from" "$name" && docker start "$name"
else
  pid="$(cat "$data/server.pid")"; kill "$pid" 2>/dev/null; for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill -0 "$pid" 2>/dev/null && { echo "STOP: pid $pid did not stop"; exit 1; }
  mkdir -p "$bk/restore" && tar -xzf "$bk/data.tar.gz" -C "$bk/restore" || exit 1
  for f in tracepad.db tracepad.db-wal tracepad.db-shm; do rm -f "$data/$f"; [ ! -f "$bk/restore/$f" ] || cp "$bk/restore/$f" "$data/"; done
  if [ -f "$bk/tracepad" ]; then cp "$bk/tracepad" "$HOME/.local/bin/.tracepad.old" && mv -f "$HOME/.local/bin/.tracepad.old" "$HOME/.local/bin/tracepad"
  else curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$from" TRACEPAD_NO_SKILL=1 sh; fi
  wc -l <"$data/server.log" >"$bk/log-lines"; TRACEPAD_URL="http://$listen" nohup "$HOME/.local/bin/tracepad" serve --listen "$listen" --data-dir "$data" >>"$data/server.log" 2>&1 &
  echo $! >"$data/server.pid"
fi
printf 'to=%s\n' "$from" >>"$bk/state"
```

Then step 5's check again, which now expects `from` (a build before
`0.1.0-rc.2` writes no version line first), and the log's last lines.

## 6. The skill and the package

The skill is the binary's own: install it again wherever it is
(`tracepad/.version` marks a copy), the way it went in:

```sh
export PATH="$HOME/.local/bin:$PATH"
for d in "$HOME/.claude/skills" "$HOME/.agents/skills"; do [ ! -f "$d/tracepad/.version" ] || tracepad skills install --dir "$d"; done
[ ! -f .claude/skills/tracepad/.version ] || tracepad skills install --project
```

With no binary on the host, the image installs it:
`docker run --rm --user "$(id -u):$(id -g)" -v "$HOME/.claude/skills:/skills" ghcr.io/tracepad/tracepad:<to> skills install --dir /skills`.

A pinned `tracepad` package goes to the same version with the project's own
tool: Python `tracepad==0.1.0rc2` (PyPI's spelling), npm `tracepad@0.1.0-rc.2`
(`next` is the newest candidate), Go `…/sdk/go@v0.1.0-rc.2`. A version not
published leaves the pin, and the report says so. Run the project's tests.

## 7. Report to the human

- **From and to**: both versions, of the binary and of each server.
- **The backup**: `$bk`, the whole database as private as the data directory.
  Theirs to delete once the upgrade has proved itself, with the old container
  (`docker rm tracepad-<project>-<from>`).
- **The checks**: the first log line, `health`, the trace counts, the test span.
- **What changed**: the skill's copies, the package's pin, and the tests' result.
- **What is theirs**: a server you did not start (the commands of
  https://tracepad.github.io/tracepad/install/#upgrading or
  https://tracepad.github.io/tracepad/docker/#upgrading-and-backing-up-first,
  with its unit or container name), a Compose file, a server open beyond this
  machine, free space for a backup, and going back by more than this upgrade.
