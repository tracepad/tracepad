#!/usr/bin/env bash
# The image's test (spec 020 #7). A Dockerfile that only ever gets built proves
# that it builds; what an operator needs proved is that the thing boots, writes
# where it was told to, and says who it is — and an image that exits on start
# because /data is not writable is exactly the failure a release would
# otherwise be the first to find.
#
#   scripts/image-check.sh [image]        (default: tracepad:dev)
#
# The image must have been built with VERSION set to what EXPECT_VERSION says,
# which is how the run proves the build argument reached the binary rather than
# merely reached the build. Both defaults are the Makefile's, so that
# `make image && scripts/image-check.sh` agrees with itself; `make image-check`
# passes whatever VERSION is in force, and CI passes `ci`.
#
# Every assertion here is one the Dockerfile can fail: the mutations named in
# the spec's Testing section (drop USER, drop HEALTHCHECK, drop
# ENV TRACEPAD_DATA_DIR) each turn exactly one of them red.
set -euo pipefail

image="${1:-tracepad:dev}"
expect_version="${EXPECT_VERSION:-dev}"

# Distroless has no shell, so nothing here may `docker exec sh`. Everything is
# asked of the daemon (`inspect`, `top`, `cp`) or of the binary itself, which
# is the only executable in the image.
container="tracepad-image-check-$$"
volume="tracepad-image-check-$$"

cleanup() {
    docker rm -f "$container" >/dev/null 2>&1 || true
    docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
    echo "FAIL: $*" >&2
    echo "--- container logs ---" >&2
    docker logs "$container" >&2 2>&1 || true
    exit 1
}

echo "==> booting $image on an ephemeral volume"
docker volume create "$volume" >/dev/null
# Port 0 lets the host pick: a check must not fail because something else on
# the machine happened to hold 4318.
docker run -d --name "$container" -v "$volume:/data" -p 127.0.0.1:0:4318 \
    "$image" >/dev/null

# The wait reads the container's own HEALTHCHECK rather than curling from here,
# which is what tests `tracepad health` in the place it will actually be used
# (#7). `starting` is the start period; anything else is decided.
echo "==> waiting for the container's own HEALTHCHECK to pass"
deadline=$((SECONDS + 90))
while :; do
    status="$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || echo gone)"
    [ "$status" = "running" ] || fail "the container is $status before it ever became healthy"

    # `.State.Health` is nil on an image that declares no HEALTHCHECK, and
    # reaching through it would make the template error rather than the check
    # — which is a probe that reports the wrong reason and then waits out its
    # whole timeout to do it.
    health="$(docker inspect \
        --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \
        "$container")"
    case "$health" in
        healthy) break ;;
        none) fail "the image declares no HEALTHCHECK, so nothing in it says whether the server is up" ;;
        unhealthy) fail "the container went unhealthy: its own probe says the server is not answering" ;;
    esac
    [ "$SECONDS" -lt "$deadline" ] || fail "still $health after 90s"
    sleep 1
done
echo "    healthy"

echo "==> the version the binary reports is the version the build was given"
address="$(docker port "$container" 4318/tcp | head -1)"
[ -n "$address" ] || fail "4318 is not published; the image's EXPOSE or the run's -p is wrong"
# Over the published port on purpose: a server bound to 127.0.0.1 inside the
# container would answer its own HEALTHCHECK and nothing else, which is the
# mistake TRACEPAD_LISTEN=:4318 exists to prevent (edge cases).
body="$(curl -fsS --max-time 10 "http://$address/health")" ||
    fail "GET /health over the published port $address answered nothing"
case "$body" in
    *"\"version\":\"$expect_version\""*) ;;
    *) fail "/health says $body, want version $expect_version" ;;
esac
echo "    $body"

echo "==> the log opens with the version (spec 001 #25)"
# `docker logs` is where a report starts. The whole log is captured before it
# is cut, so a `head` closing the pipe cannot turn a good run into a SIGPIPE.
logs="$(docker logs "$container" 2>&1)"
first="${logs%%$'\n'*}"
case "$first" in
    *"tracepad $expect_version"*) ;;
    *) fail "the first line of the log is '$first', want it to name 'tracepad $expect_version'" ;;
esac
echo "    $first"

echo "==> the server runs as uid 65532"
# `docker top` asks the daemon, which is the only party that can see the
# process: there is no shell in the image to ask `id` of. The column is found
# by its header rather than by position, because the daemon chooses the `ps`
# arguments and a fixed index is a guess about someone else's default.
#
# An absent column is its own failure rather than a default: awk reads an unset
# variable as 0, so `$column` would silently become `$0` and the check would
# report the whole process line as the uid — a wrong answer wearing the costume
# of a real regression.
uid="$(docker top "$container" | awk '
    NR == 1 { for (i = 1; i <= NF; i++) if ($i == "UID") column = i; next }
    NR == 2 { if (column) print $column; else print "no-UID-column" }')"
case "$uid" in
    65532) ;;
    no-UID-column)
        fail "docker top printed no UID column, so this check cannot answer; its header was: $(docker top "$container" | head -1)" ;;
    *)
        fail "the server runs as uid ${uid:-unknown}, want 65532 — a container that writes /data as root is the bind-mount permission problem every operator hits once" ;;
esac
echo "    uid $uid"

echo "==> the data directory is the volume"
# The other half of TRACEPAD_DATA_DIR: not merely that the server started, but
# that what it wrote landed on the volume the operator mounted rather than in
# the container's writable layer, which a `docker rm` would take with it.
listing="$(docker run --rm -v "$volume:/data" busybox:latest ls /data)"
case "$listing" in
    *tracepad.db*) ;;
    *) fail "the volume holds [$listing], not the database: the server wrote somewhere else" ;;
esac
echo "    tracepad.db is on the volume"

echo "==> the volume is its owner's alone"
# The database holds every prompt, the password hashes and the media signing
# key; the directory and the file are closed to other accounts (spec 044 #13).
modes="$(docker run --rm -v "$volume:/data" busybox:latest stat -c '%a %n' /data /data/tracepad.db)"
case "$modes" in
    "700 /data"*"600 /data/tracepad.db") ;;
    *) fail "the modes are [$modes], want /data 700 and tracepad.db 600" ;;
esac
echo "    /data 700, tracepad.db 600"

echo "==> Apache-2.0 §4(d): the licences travel in the image"
for file in LICENSE NOTICE THIRD_PARTY_NOTICES third_party/langfuse/LICENSE; do
    docker cp "$container:/usr/share/doc/tracepad/$file" - >/dev/null 2>&1 ||
        fail "/usr/share/doc/tracepad/$file is not in the image"
done
echo "    LICENSE, NOTICE, THIRD_PARTY_NOTICES and third_party are present"

echo "==> the probe fails honestly"
# A probe that cannot tell a dead server from a live one is worse than none:
# this is the same binary, pointed at a port nothing listens on.
if docker run --rm "$image" health --url http://127.0.0.1:1 >/dev/null 2>&1; then
    fail "health exited 0 against a port nothing listens on"
fi
echo "    health exits 1 when nothing answers"

echo "OK: $image"
