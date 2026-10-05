# Upgrading Tracepad

From the installed version to the one the human asked for, with a backup first
and a way back, since migrations run forward only. Each step says what a healthy
answer looks like; anything else is a reason to stop and tell the human.

## 1. What is installed, and what runs it

```sh
export PATH="$HOME/.local/bin:$PATH"; command -v tracepad && tracepad version
ps -A -o pid= -o args= | grep '^ *[0-9]* [^ ]*tracepad serve' | while read -r pid args; do
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

One command. It writes this run's `state` first, in a backup directory of its
own, and prints that directory: steps 5 and the way back take it, never another.
Before the server stops: the trace count, room for the backup, and a copy of the
running version in the backup directory. Then the new binary, the server
stopped, its data directory archived and the archive read back, the new version
started. A failure after the stop starts the old version again on the data as it
was. Fill in `to` and step 1's `data`.

```sh
to=0.1.0; data=/path/from/step/1
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"; umask 077
pid="$(cat "$data/server.pid")"; listen="$(ps -o args= -p "$pid" | sed -n 's/.*--listen \([^ ]*\).*/\1/p')"
case "$listen" in localhost:* | 127.0.0.1:*) export TRACEPAD_URL="http://$listen" ;; *) echo "STOP: pid $pid listens on '$listen', not this machine alone"; exit 1 ;; esac
from="$(curl -fsS "$TRACEPAD_URL/health" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"; [ -n "$from" ] || { echo "STOP: $TRACEPAD_URL does not answer"; exit 1; }
mkdir -p "$HOME/tracepad-backups" && bk="$(mktemp -d "$HOME/tracepad-backups/$(date +%Y%m%d-%H%M%S)-$from-XXXXXX")" || exit 1
printf "run=%s\nhow=binary\ndata='%s'\nlisten=%s\nfrom=%s\nto=%s\n" "${bk##*/}" "$data" "$listen" "$from" "$to" >"$bk/state" && echo "run: $bk" || exit 1
[ "$(df -Pk "$bk" | awk 'NR == 2 { print $4 }')" -gt "$(($(du -sk "$data" | cut -f 1) + 102400))" ] || { echo "STOP: no room for a backup in $bk"; exit 1; }
curl -fsS -H "Authorization: Bearer $TRACEPAD_API_KEY" "$TRACEPAD_URL/api/v1/system" | sed -n 's/.*"traces":\([0-9]*\).*/\1/p' >"$bk/count"
if [ "$(tracepad version)" = "$from" ]; then cp "$(command -v tracepad)" "$bk/tracepad"
else curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$from" TRACEPAD_INSTALL_DIR="$bk" TRACEPAD_NO_SKILL=1 sh >/dev/null; fi
[ "$("$bk/tracepad" version 2>/dev/null)" = "$from" ] || { echo "STOP: no copy of $from to go back to; nothing changed"; exit 1; }
curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$to" TRACEPAD_NO_SKILL=1 sh || exit 1
start() { { wc -l <"$data/server.log" || echo 0; } >"$bk/log-lines" 2>/dev/null; nohup "$1" serve --listen "$listen" --data-dir "$data" >>"$data/server.log" 2>&1 & echo $! >"$data/server.pid"; }
back() { echo "STOP: $1. $from starts again, as $bk/tracepad, on the data as it was"; start "$bk/tracepad"; exit 1; }
kill "$pid"; for i in $(seq 1 15); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
kill -0 "$pid" 2>/dev/null && { echo "STOP: pid $pid did not stop, and still runs $from"; exit 1; }
tar -czf "$bk/data.tar.gz" -C "$data" . || back "the archive failed"
tar -tzf "$bk/data.tar.gz" >"$bk/list" && grep -qx './tracepad.db' "$bk/list" || back "the archive does not read back whole"
echo archived=yes >>"$bk/state" && start tracepad && echo "started $to; check with bk=$bk"
```

An empty count: the key cannot read, and step 5's test span is the check. After a
`STOP` that started `$from` again, it runs from `$bk`: tell the human that.

## 4. Your own container

The same for `tracepad-<project>`: room for the volume's archive first, then the
container stopped, the volume archived and read back, the old container kept,
renamed `<name>-<from>`, and a new one with its volumes, ports, restart policy and
the `TRACEPAD_*` variables the human set (the image's own are left to the new
image). A failure before the new one runs starts the old one again. A container
Compose manages stops here: its file is the human's.

```sh
to=0.1.0; name=tracepad-project
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"; umask 077
compose="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$name")" || exit 1; [ -z "$compose" ] || { echo "STOP: Compose runs $name"; exit 1; }
vol="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$name")"; [ -n "$vol" ] || { echo "STOP: no volume at /data"; exit 1; }
listen="$(docker inspect -f '{{range (index .HostConfig.PortBindings "4318/tcp")}}{{.HostIp}}:{{.HostPort}}{{end}}' "$name")"; image="$(docker inspect -f '{{.Image}}' "$name")"
case "$listen" in 127.0.0.1:*) export TRACEPAD_URL="http://$listen" ;; *) echo "STOP: $name publishes on '$listen', not this machine alone"; exit 1 ;; esac
from="$(curl -fsS "$TRACEPAD_URL/health" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"; [ -n "$from" ] || { echo "STOP: $TRACEPAD_URL does not answer"; exit 1; }
mkdir -p "$HOME/tracepad-backups" && bk="$(mktemp -d "$HOME/tracepad-backups/$(date +%Y%m%d-%H%M%S)-$from-XXXXXX")" || exit 1
printf 'run=%s\nhow=docker\nname=%s\nvol=%s\nlisten=%s\nimage=%s\nfrom=%s\nto=%s\n' "${bk##*/}" "$name" "$vol" "$listen" "$image" "$from" "$to" >"$bk/state" && echo "run: $bk" || exit 1
size="$(docker run --rm -v "$vol:/data" busybox du -sk /data | cut -f 1)"; [ -n "$size" ] && [ "$(df -Pk "$bk" | awk 'NR == 2 { print $4 }')" -gt "$size" ] || { echo "STOP: no room for a backup in $bk"; exit 1; }
set -- --restart "$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$name")"
for m in $(docker inspect -f '{{range .Mounts}}{{if .Name}}{{.Name}}{{else}}{{.Source}}{{end}}:{{.Destination}} {{end}}' "$name"); do set -- "$@" -v "$m"; done
for p in $(docker inspect -f '{{range $k, $v := .HostConfig.PortBindings}}{{range $v}}{{.HostIp}}:{{.HostPort}}:{{$k}} {{end}}{{end}}' "$name"); do set -- "$@" -p "$p"; done
printf '%s\n' "$@" >"$bk/args"; docker image inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$image" >"$bk/image-env" || exit 1
docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$name" | grep '^TRACEPAD_' | grep -vxF -f "$bk/image-env" >"$bk/env"
curl -fsS -H "Authorization: Bearer $TRACEPAD_API_KEY" "$TRACEPAD_URL/api/v1/system" | sed -n 's/.*"traces":\([0-9]*\).*/\1/p' >"$bk/count"
docker pull -q "ghcr.io/tracepad/tracepad:$to" >/dev/null && docker stop "$name" >/dev/null || exit 1
back() { echo "STOP: $1. $name starts again as it was"; docker start "$name" >/dev/null; exit 1; }
docker run --rm -v "$vol:/data" -v "$bk:/backup" busybox sh -c "umask 077 && tar czf /backup/data.tar.gz -C /data . && chown $(id -u):$(id -g) /backup/data.tar.gz" || back "the archive failed"
tar -tzf "$bk/data.tar.gz" >"$bk/list" && grep -qx './tracepad.db' "$bk/list" || back "the archive does not read back whole"
echo archived=yes >>"$bk/state"; docker rename "$name" "$name-$from" || back "the rename failed"
docker run -d --name "$name" "$@" --env-file "$bk/env" "ghcr.io/tracepad/tracepad:$to" serve >/dev/null && echo "started $to; check with bk=$bk" ||
  { docker rename "$name" "$name-failed-${bk##*/}" 2>/dev/null; docker rename "$name-$from" "$name"; back "the new container did not start"; }
```

Then the CLI: `curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$to" TRACEPAD_NO_SKILL=1 sh`.

## 5. Check, and the way back

`bk` is the directory step 3 or 4 printed. Run it again after a wait while the
log says a migration is in progress.

```sh
bk=/path/printed/by/step/3; bk="${bk%/}"; . "$bk/state"; [ "$run" = "${bk##*/}" ] || { echo "STOP: $bk/state is not this run's"; exit 1; }
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_URL="http://$listen" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"
for i in $(seq 1 30); do curl -fsS "$TRACEPAD_URL/health" >/dev/null 2>&1 && break; sleep 2; done
if [ "$how" = docker ]; then docker logs "$name" 2>&1 | head -n 1; else tail -n "+$(($(cat "$bk/log-lines") + 1))" "$data/server.log" | head -n 1; fi
v="$(curl -fsS "$TRACEPAD_URL/health" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"; before="$(cat "$bk/count")"
now="$(curl -fsS -H "Authorization: Bearer $TRACEPAD_API_KEY" "$TRACEPAD_URL/api/v1/system" | sed -n 's/.*"traces":\([0-9]*\).*/\1/p')"
echo "health: ${v:-no answer}; traces: ${before:-?} before, ${now:-?} now"
[ "$v" = "$to" ] || { echo "NOT HEALTHY: no answer as $to"; exit 1; }
[ -z "$before" ] || [ -z "$now" ] || [ "$now" -ge "$before" ] || { echo "NOT HEALTHY: fewer traces than before"; exit 1; }
echo HEALTHY
```

Healthy: the log's first line names the new version and its commit (*tracepad
0.1.0 (a1b2c3d)*), and the block ends *HEALTHY*. Then `setup.md`'s step 4 test
span answers `200`, and its step 6 reads it back.

**The way back**, at once on *NOT HEALTHY*; when the server runs but something
else looks wrong, ask first. It deletes nothing: the state after the upgrade is
set aside (the data directory renamed `<data>.after-<run>`, or the volume left
as it is with the new container renamed `<name>-after-<run>`), and the archive is
restored into a new place. Every step is checked, and a failure stops before
anything starts. What was set aside is the human's to remove.

```sh
bk=/path/printed/by/step/3; bk="${bk%/}"; . "$bk/state"; umask 077
[ "$run" = "${bk##*/}" ] && [ "$archived" = yes ] || { echo "STOP: $bk holds no archive of this run; nothing to go back to"; exit 1; }
tar -tzf "$bk/data.tar.gz" >/dev/null || { echo "STOP: $bk/data.tar.gz does not read"; exit 1; }
if [ "$how" = docker ]; then
  docker inspect "$name-$from" >/dev/null || { echo "STOP: no container $name-$from to go back to"; exit 1; }
  docker volume create "$vol-$run" >/dev/null && docker run --rm -v "$vol-$run:/data" -v "$vol:/old:ro" -v "$bk:/backup:ro" busybox \
    sh -c 'tar xzf /backup/data.tar.gz -C /data && chown "$(stat -c %u:%g /old)" /data && test -s /data/tracepad.db' || { echo "STOP: the restore into $vol-$run failed; nothing changed"; exit 1; }
  docker stop "$name" >/dev/null; docker rename "$name" "$name-after-$run" || { echo "STOP: $name could not be set aside"; exit 1; }
  set --; while IFS= read -r a; do [ "$a" != "$vol:/data" ] || a="$vol-$run:/data"; set -- "$@" "$a"; done <"$bk/args"
  docker run -d --name "$name" "$@" --env-file "$bk/env" "$image" serve >/dev/null || { echo "STOP: $from did not start on $vol-$run"; exit 1; }
else
  [ "$("$bk/tracepad" version)" = "$from" ] || { echo "STOP: $bk/tracepad is not $from"; exit 1; }
  pid="$(cat "$data/server.pid")"; kill "$pid" 2>/dev/null; for i in $(seq 1 15); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill -0 "$pid" 2>/dev/null && { echo "STOP: pid $pid did not stop"; exit 1; }
  mv "$data" "$data.after-$run" || { echo "STOP: $data could not be set aside"; exit 1; }
  mkdir "$data" && tar -xzf "$bk/data.tar.gz" -C "$data" && [ -s "$data/tracepad.db" ] || { echo "STOP: the restore into $data failed; what the upgrade left is $data.after-$run; nothing started"; exit 1; }
  ! command -v sqlite3 >/dev/null || [ "$(sqlite3 "$data/tracepad.db" 'PRAGMA integrity_check')" = ok ] || { echo "STOP: the restored database fails its integrity check; nothing started"; exit 1; }
  cp "$bk/tracepad" "$HOME/.local/bin/.tracepad.old" && mv -f "$HOME/.local/bin/.tracepad.old" "$HOME/.local/bin/tracepad" || { echo "STOP: $from could not be put back in ~/.local/bin; nothing started"; exit 1; }
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
  Theirs to delete once the upgrade has proved itself, with whatever was set
  aside: the old container `<name>-<from>`; after a way back, `<data>.after-<run>`,
  or the container `<name>-after-<run>` and the volume it used.
- **The checks**: the first log line, `health`, the trace counts, the test span.
- **What changed**: the skill's copies, the package's pin, and the tests' result.
- **What is theirs**: a server you did not start (the commands of
  https://tracepad.github.io/tracepad/install/#upgrading or
  https://tracepad.github.io/tracepad/docker/#upgrading-and-backing-up-first,
  with its unit or container name), a Compose file, a server open beyond this
  machine, free space for a backup, and going back by more than this upgrade.
