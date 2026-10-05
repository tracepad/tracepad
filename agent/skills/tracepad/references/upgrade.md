# Upgrading Tracepad

From the installed version to the one asked for, with a backup first and a way
back (migrations run forward only). Anything not healthy: stop, tell the human.

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
- Nothing running: only the binary, the skill and the package change; no binary
  is a setup (`setup.md`). One outside `~/.local/bin` is its package manager's,
  so the human's (`brew upgrade tracepad`).

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

Tell the human what `github.com/tracepad/tracepad/releases/tag/v<version>` lists under *Changed*.

## 3. Your own server

One command; fill in `to` and step 1's `data`. It prints this run's backup
directory, whose `state` (plain `key=value` data) step 5 and the way back take.
Nothing stops before there is room, a count and a verified copy of the running
version; after the stop, any failure starts that version again on the data as it
was. The install script's advice goes to `$bk/install.log`: this block is it.

```sh
to=0.1.0; data=/path/from/step/1
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"; umask 077
pid="$(cat "$data/server.pid")"; listen="$(ps -o args= -p "$pid" | sed -n 's/.*--listen \([^ ]*\).*/\1/p')"
case "$listen" in localhost:[0-9]* | 127.0.0.1:[0-9]*) export TRACEPAD_URL="http://$listen" ;; *) echo "STOP: pid $pid listens on '$listen', not this machine alone"; exit 1 ;; esac
from="$(curl -fsS "$TRACEPAD_URL/health" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
for v in "$from" "$to"; do case "$v" in *[!0-9A-Za-z.-]*) ;; [0-9]*.[0-9]*.[0-9]*) continue ;; esac; echo "STOP: '$v' is not a release's version (empty: $TRACEPAD_URL does not answer)"; exit 1; done
case "$data" in *[!A-Za-z0-9._/@+\ -]*) echo "STOP: run this with a data directory named in plain characters"; exit 1 ;; esac
mkdir -p "$HOME/tracepad-backups" && bk="$(mktemp -d "$HOME/tracepad-backups/$(date +%Y%m%d-%H%M%S)-$from-XXXXXX")" || exit 1
printf 'run=%s\nhow=binary\ndata=%s\nlisten=%s\nfrom=%s\nto=%s\n' "${bk##*/}" "$data" "$listen" "$from" "$to" >"$bk/state" && echo "run: $bk" || exit 1
[ "$(df -Pk "$bk" | awk 'NR == 2 { print $4 }')" -gt "$(($(du -sk "$data" | cut -f 1) + 102400))" ] || { echo "STOP: no room for a backup in $bk"; exit 1; }
curl -fsS -H "Authorization: Bearer $TRACEPAD_API_KEY" "$TRACEPAD_URL/api/v1/system" | sed -n 's/.*"traces":\([0-9]*\).*/\1/p' >"$bk/count"
curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$from" TRACEPAD_INSTALL_DIR="$bk" TRACEPAD_NO_SKILL=1 sh >"$bk/install.log"
[ "$("$bk/tracepad" version 2>/dev/null)" = "$from" ] || { echo "STOP: no copy of $from to go back to; nothing changed"; exit 1; }
curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$to" TRACEPAD_NO_SKILL=1 sh >>"$bk/install.log" || exit 1
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

The same for `tracepad-<project>`. The old container is kept as `<name>-<from>`
with no restart policy; the new one gets its mounts with their modes, ports,
restart policy, and the `TRACEPAD_*` variables the human set, not the image's.
Compose stops here: its file is the human's.

```sh
to=0.1.0; name=tracepad-project
export TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"; umask 077
compose="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$name")" || exit 1; [ -z "$compose" ] || { echo "STOP: Compose runs $name"; exit 1; }
vol="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$name")"; [ -n "$vol" ] || { echo "STOP: no volume at /data"; exit 1; }
listen="$(docker inspect -f '{{range (index .HostConfig.PortBindings "4318/tcp")}}{{.HostIp}}:{{.HostPort}}{{end}}' "$name")"; image="$(docker inspect -f '{{.Image}}' "$name")"
case "$listen" in 127.0.0.1:[0-9]*) export TRACEPAD_URL="http://$listen" ;; *) echo "STOP: $name publishes on '$listen', not this machine alone"; exit 1 ;; esac
from="$(curl -fsS "$TRACEPAD_URL/health" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
for v in "$from" "$to"; do case "$v" in *[!0-9A-Za-z.-]*) ;; [0-9]*.[0-9]*.[0-9]*) continue ;; esac; echo "STOP: '$v' is not a release's version (empty: $TRACEPAD_URL does not answer)"; exit 1; done
mkdir -p "$HOME/tracepad-backups" && bk="$(mktemp -d "$HOME/tracepad-backups/$(date +%Y%m%d-%H%M%S)-$from-XXXXXX")" || exit 1
printf 'run=%s\nhow=docker\nname=%s\nvol=%s\nlisten=%s\nimage=%s\nfrom=%s\nto=%s\n' "${bk##*/}" "$name" "$vol" "$listen" "$image" "$from" "$to" >"$bk/state" && echo "run: $bk" || exit 1
size="$(docker run --rm -v "$vol:/data" busybox du -sk /data | cut -f 1)"; [ -n "$size" ] && [ "$(df -Pk "$bk" | awk 'NR == 2 { print $4 }')" -gt "$size" ] || { echo "STOP: no room for a backup in $bk"; exit 1; }
{ echo --restart; docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$name"
  docker inspect -f '{{range .Mounts}}--mount{{println}}type={{.Type}},src={{if .Name}}{{.Name}}{{else}}{{.Source}}{{end}},dst={{.Destination}}{{if not .RW}},readonly{{end}}{{println}}{{end}}' "$name"
  docker inspect -f '{{range $k, $v := .HostConfig.PortBindings}}{{range $v}}-p{{println}}{{.HostIp}}:{{.HostPort}}:{{$k}}{{println}}{{end}}{{end}}' "$name"; } | grep . >"$bk/args" || exit 1
docker image inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$image" >"$bk/image-env" || exit 1
docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$name" | grep '^TRACEPAD_' | grep -vxF -f "$bk/image-env" >"$bk/env"
curl -fsS -H "Authorization: Bearer $TRACEPAD_API_KEY" "$TRACEPAD_URL/api/v1/system" | sed -n 's/.*"traces":\([0-9]*\).*/\1/p' >"$bk/count"
docker pull -q "ghcr.io/tracepad/tracepad:$to" >/dev/null && docker stop "$name" >/dev/null || exit 1
back() { echo "STOP: $1. $name starts again as it was"; docker start "$name" >/dev/null; exit 1; }
docker run --rm -v "$vol:/data" -v "$bk:/backup" busybox sh -c "umask 077 && tar czf /backup/data.tar.gz -C /data . && chown $(id -u):$(id -g) /backup/data.tar.gz" || back "the archive failed"
tar -tzf "$bk/data.tar.gz" >"$bk/list" && grep -qx './tracepad.db' "$bk/list" || back "the archive does not read back whole"
echo archived=yes >>"$bk/state"; docker rename "$name" "$name-$from" || back "the rename failed"
docker update --restart no "$name-$from" >/dev/null; set --; while IFS= read -r a; do set -- "$@" "$a"; done <"$bk/args"
docker run -d --name "$name" "$@" --env-file "$bk/env" "ghcr.io/tracepad/tracepad:$to" serve >/dev/null && echo "started $to; check with bk=$bk" || {
  docker rename "$name" "$name-failed-${bk##*/}" 2>/dev/null && docker update --restart no "$name-failed-${bk##*/}" >/dev/null
  docker rename "$name-$from" "$name" && docker update --restart "$(sed -n 2p "$bk/args")" "$name" >/dev/null; back "the new container did not start"; }
```

Then the CLI: `curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION="$to" TRACEPAD_NO_SKILL=1 sh >/dev/null`.

## 5. Check, and the way back

`bk` is the directory step 3 or 4 printed; a value in its `state` of another
shape stops it. Run it again after a wait while the log reports a migration.

```sh
bk=/path/printed/by/step/3; bk="${bk%/}"
get() { v="$(sed -n "s/^$1=//p" "$bk/state" | tail -n 1)"; case "$v" in "" | *[!$2]*) echo "STOP: $1 in $bk/state is not this procedure's" >&2; return 1 ;; esac; echo "$v"; }
run="$(get run A-Za-z0-9._-)" && how="$(get how a-z)" && listen="$(get listen a-z0-9.:)" && to="$(get to 0-9A-Za-z.-)" || exit 1
[ "$run" = "${bk##*/}" ] || { echo "STOP: $bk/state is not this run's"; exit 1; }
if [ "$how" = docker ]; then name="$(get name A-Za-z0-9_.-)"; else data="$(get data 'A-Za-z0-9._/@+ -')"; fi || exit 1
export PATH="$HOME/.local/bin:$PATH" TRACEPAD_URL="http://$listen" TRACEPAD_API_KEY="$(sed -n 's/^TRACEPAD_API_KEY=//p' .env)"
for i in $(seq 1 30); do curl -fsS "$TRACEPAD_URL/health" >/dev/null 2>&1 && break; sleep 2; done
if [ "$how" = docker ]; then docker logs "$name" 2>&1 | head -n 1; else tail -n "+$(($(cat "$bk/log-lines") + 1))" "$data/server.log" | head -n 1; fi
v="$(curl -fsS "$TRACEPAD_URL/health" 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"; before="$(cat "$bk/count")"
now="$(curl -fsS -H "Authorization: Bearer $TRACEPAD_API_KEY" "$TRACEPAD_URL/api/v1/system" 2>/dev/null | sed -n 's/.*"traces":\([0-9]*\).*/\1/p')"
echo "health: ${v:-no answer}; traces: ${before:-?} before, ${now:-?} now"
[ "$v" = "$to" ] || { echo "NOT HEALTHY: no answer as $to"; exit 1; }
[ -z "$before" ] || { [ -n "$now" ] && [ "$now" -ge "$before" ]; } || { echo "NOT HEALTHY: the traces are fewer, or cannot be counted"; exit 1; }
echo HEALTHY
```

Healthy: *HEALTHY*, after a log line naming the version and commit (*tracepad
0.1.0 (a1b2c3d)*); then `setup.md`'s step 4 span (`200`) and step 6 read-back.

**The way back**, at once on *NOT HEALTHY* (otherwise ask first), deletes
nothing: it sets aside `<data>.after-<run>`, or the container `<name>-after-<run>`
and the volume, and restores into a new place. It stops only a PID that is still
this server; every step is checked, and a failure starts nothing.

```sh
bk=/path/printed/by/step/3; bk="${bk%/}"; umask 077
get() { v="$(sed -n "s/^$1=//p" "$bk/state" | tail -n 1)"; case "$v" in "" | *[!$2]*) echo "STOP: $1 in $bk/state is not this procedure's" >&2; return 1 ;; esac; echo "$v"; }
run="$(get run A-Za-z0-9._-)" && how="$(get how a-z)" && listen="$(get listen a-z0-9.:)" && from="$(get from 0-9A-Za-z.-)" && get archived yes >/dev/null || exit 1
[ "$run" = "${bk##*/}" ] || { echo "STOP: $bk/state is not this run's"; exit 1; }
tar -tzf "$bk/data.tar.gz" >/dev/null || { echo "STOP: $bk/data.tar.gz does not read"; exit 1; }
if [ "$how" = docker ]; then
  name="$(get name A-Za-z0-9_.-)" && vol="$(get vol A-Za-z0-9_.-)" && image="$(get image a-z0-9:)" || exit 1
  docker inspect "$name-$from" >/dev/null || { echo "STOP: no container $name-$from to go back to"; exit 1; }
  docker volume create "$vol-$run" >/dev/null && docker run --rm -v "$vol-$run:/data" -v "$vol:/old:ro" -v "$bk:/backup:ro" busybox \
    sh -c 'tar xzf /backup/data.tar.gz -C /data && chown "$(stat -c %u:%g /old)" /data && test -s /data/tracepad.db' || { echo "STOP: the restore into $vol-$run failed; nothing changed"; exit 1; }
  docker stop "$name" >/dev/null; docker rename "$name" "$name-after-$run" && docker update --restart no "$name-after-$run" >/dev/null || { echo "STOP: $name could not be set aside"; exit 1; }
  set --; while IFS= read -r a; do [ "$a" != "type=volume,src=$vol,dst=/data" ] || a="type=volume,src=$vol-$run,dst=/data"; set -- "$@" "$a"; done <"$bk/args"
  docker run -d --name "$name" "$@" --env-file "$bk/env" "$image" serve >/dev/null || { echo "STOP: $from did not start on $vol-$run"; exit 1; }
else
  data="$(get data 'A-Za-z0-9._/@+ -')" || exit 1; [ "$("$bk/tracepad" version)" = "$from" ] || { echo "STOP: $bk/tracepad is not $from"; exit 1; }
  pid="$(cat "$data/server.pid")"; case "$(ps -o args= -p "$pid")" in
    "") ;; *"tracepad serve "*"--data-dir $data"*) kill "$pid"; for i in $(seq 1 15); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done ;;
    *) echo "STOP: pid $pid is no longer this server; nothing stopped"; exit 1 ;; esac
  kill -0 "$pid" 2>/dev/null && { echo "STOP: pid $pid did not stop"; exit 1; }
  mv "$data" "$data.after-$run" || { echo "STOP: $data could not be set aside"; exit 1; }
  mkdir "$data" && tar -xzf "$bk/data.tar.gz" -C "$data" && [ -s "$data/tracepad.db" ] || { echo "STOP: the restore into $data failed; what the upgrade left is $data.after-$run; nothing started"; exit 1; }
  ! command -v sqlite3 >/dev/null || [ "$(sqlite3 "$data/tracepad.db" 'PRAGMA integrity_check')" = ok ] || { echo "STOP: the restored database fails its integrity check; nothing started"; exit 1; }
  cp "$bk/tracepad" "$HOME/.local/bin/.tracepad.old" && mv -f "$HOME/.local/bin/.tracepad.old" "$HOME/.local/bin/tracepad" || { echo "STOP: $from could not be put back in ~/.local/bin; nothing started"; exit 1; }
  wc -l <"$data/server.log" >"$bk/log-lines"; TRACEPAD_URL="http://$listen" nohup "$HOME/.local/bin/tracepad" serve --listen "$listen" --data-dir "$data" >>"$data/server.log" 2>&1 &
  echo $! >"$data/server.pid"
fi
echo "to=$from" >>"$bk/state"
```

Then step 5's check again, expecting `from` now (before rc.2, no version line).

## 6. The skill and the package

The skill is the binary's own: install it again wherever `tracepad/.version` marks a copy:

```sh
export PATH="$HOME/.local/bin:$PATH"
for d in "$HOME/.claude/skills" "$HOME/.agents/skills"; do [ ! -f "$d/tracepad/.version" ] || tracepad skills install --dir "$d"; done
[ ! -f .claude/skills/tracepad/.version ] || tracepad skills install --project
```

With no binary on the host, the image installs it:
`docker run --rm --user "$(id -u):$(id -g)" -v "$HOME/.claude/skills:/skills" ghcr.io/tracepad/tracepad:<to> skills install --dir /skills`.

A pinned `tracepad` package goes to the same version, in the project's tool:
`tracepad==0.1.0rc2` (PyPI's spelling), `tracepad@0.1.0-rc.2` (npm), Go's
`…/sdk/go@v0.1.0-rc.2`. Unpublished: keep the pin, say so. Run the tests.

## 7. Report to the human

- **From and to**: both versions, of the binary and of each server.
- **The backup, said plainly**: `$bk` is a full copy of the database, every
  prompt and completion, kept until someone deletes it; erasing traces or a
  user never reaches it, nor what a way back set aside. Offer to remove it once
  the upgrade has run a week, and give the commands, which are theirs to run:
  `rm -r <bk>`, and as they apply `rm -r <data>.after-<run>`, `docker rm
  <name>-<from> <name>-after-<run>` and `docker volume rm` of the volume set aside.
- **The checks**: the first log line, `health`, the trace counts, the test span.
- **What changed**: the skill's copies, the package's pin, and the tests' result.
- **What is theirs**: a server you did not start (install.md's or docker.md's
  *Upgrading*, with its unit or container name), a Compose file, a server open
  beyond this machine, room for a backup, going back by more than this upgrade.
