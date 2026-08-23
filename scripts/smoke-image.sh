#!/usr/bin/env bash
#
# Smoke-check a built Veritix image by running it and talking to it.
#
#   make docker-smoke                 # build the image, then check it
#   IMAGE=veritix:dev scripts/smoke-image.sh
#
# The Dockerfile asserts what it can about the binary during the build -- that
# the web interface is embedded, for one -- but it cannot assert anything about
# the image that ships, because distroless has no shell. These are the checks
# that need the container running.
#
# The one this exists for is the time zone. A schedule names an IANA zone, and
# `internal/schedule` imports `time/tzdata` so the binary carries its own copy
# of the database. Nothing in `go test` can see whether that import is still
# there: this machine and every CI runner have a system zoneinfo, which
# time.LoadLocation reads first, so every zone test passes either way. The
# second phase below takes the system database away from the running container
# and asks it to accept a zone anyway, which is the situation the import is for
# -- a Windows desktop, where Go has no system zone source at all, or an image
# whose base does not ship one.

set -euo pipefail

IMAGE=${IMAGE:-veritix:dev}
PORT=${PORT:-18080}
DATASET=${DATASET:-testdata/dirty-retail}
CONTAINER=veritix-smoke-$$

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

for tool in docker curl jq; do
    command -v "$tool" >/dev/null || { echo "smoke: $tool is not installed" >&2; exit 1; }
done

fail() {
    echo >&2
    echo "FAIL: $*" >&2
    echo "--- container log ---" >&2
    docker logs "$CONTAINER" >&2 2>&1 || true
    exit 1
}

ok() { printf '  ok   %s\n' "$*"; }

body=$(mktemp)
# The empty directory phase two masks the system zone database with. 0755
# because the server runs as nonroot: a directory it cannot read fails with
# "permission denied", which would pass the check for the wrong reason.
nozones=$(mktemp -d)
chmod 755 "$nozones"

cleanup() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    rm -rf "$body" "$nozones"
}
trap cleanup EXIT

# An image bound to 0.0.0.0 refuses to serve without a token, and checking that
# an unauthenticated request is refused needs a real one to compare against. No
# openssl dependency: this is not a secret that outlives the next thirty
# seconds.
token=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
auth="Authorization: Bearer $token"
base="http://127.0.0.1:$PORT/api/v1"

# start runs the image with whatever extra arguments a phase needs.
# --read-only is readOnlyRootFilesystem from deploy/kubernetes, checked here
# rather than taken on trust; /data is a volume and stays writable, and the
# dataset is mounted read-only, the way data somebody audits should be.
start() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker run -d --rm --name "$CONTAINER" \
        -p "127.0.0.1:$PORT:8080" \
        -e VERITIX_AUTH_TOKEN="$token" \
        -v "$repo/$DATASET:/exports:ro" \
        --read-only --tmpfs /tmp \
        "$@" \
        "$IMAGE" serve >/dev/null

    for _ in $(seq 60); do
        if curl -fsS "$base/health" >/dev/null 2>&1; then return; fi
        sleep 1
    done
    fail "the server never answered /health"
}

# register returns the id of the dataset at /exports, which a schedule needs:
# an uploaded dataset cannot be scheduled at all.
register() {
    local out
    out=$(curl -fsS -XPOST "$base/datasets" -H "$auth" \
        -H 'Content-Type: application/json' -d '{"path":"/exports"}') \
        || fail "the dataset could not be registered"
    jq -r .id <<<"$out"
}

put_schedule() {
    curl -s -o "$body" -w '%{http_code}' \
        -XPUT "$base/datasets/$1/schedule" -H "$auth" \
        -H 'Content-Type: application/json' \
        -d "{\"kind\":\"daily\",\"at\":\"02:00\",\"timezone\":\"$2\"}"
}

echo "smoke: $IMAGE on port $PORT"
echo "phase 1: the image as it ships"

start

health=$(curl -fsS "$base/health")
[ "$(jq -r .status <<<"$health")" = ok ] || fail "/health did not say ok: $health"
ok "serving, and /health answers unauthenticated"

code=$(curl -s -o /dev/null -w '%{http_code}' "$base/runs")
[ "$code" = 401 ] || fail "an unauthenticated /runs returned $code, not 401"
ok "the API is behind the token"

csp=$(curl -fsS -D - -o /dev/null "http://127.0.0.1:$PORT/" | tr -d '\r' \
    | sed -n 's/^[Cc]ontent-[Ss]ecurity-[Pp]olicy: //p')
case $csp in
    *"connect-src 'self'"*) ok "the interface is served under its CSP" ;;
    "") fail "the interface was served with no Content-Security-Policy" ;;
    *) fail "the CSP does not confine the page to its own origin: $csp" ;;
esac

id=$(register)
[ -n "$id" ] && [ "$id" != null ] || fail "no dataset id came back"
ok "a dataset registered by path"

# Asia/Kolkata rather than somewhere closer to home because its offset is
# +05:30 and it does not observe summer time, so the wall clock time landing in
# the right place is visible in every season. Europe/London is +00:00 for half
# the year, where a zone accepted and then ignored would look identical.
code=$(put_schedule "$id" Asia/Kolkata)
[ "$code" = 200 ] || fail "a schedule in Asia/Kolkata was refused ($code): $(cat "$body")"
due=$(jq -r .next_due_at "$body")
case $due in
    *T02:00:00+05:30) ok "a schedule in a named zone fires at 02:00 in that zone" ;;
    *) fail "next_due_at is $due, not 02:00+05:30: the zone was accepted and its offset was not applied" ;;
esac

# ── phase 2 ────────────────────────────────────────────────────────────────
#
# The same image with an empty directory bind-mounted over every zone source Go
# looks in, so nothing but the embedded database can answer. This is the only
# check in the repo that fails when time/tzdata is not imported: it has been
# run against an image built without it, and the PUT below comes back 400.
echo "phase 2: the same image with no system zone database"

start \
    -v "$nozones:/usr/share/zoneinfo:ro" \
    -v "$nozones:/usr/share/lib/zoneinfo:ro" \
    -v "$nozones:/usr/lib/locale/TZ:ro"

id=$(register)
code=$(put_schedule "$id" Europe/London)
[ "$code" = 200 ] || fail "with no system zone database, a schedule in Europe/London was refused ($code): $(cat "$body")
this is what a Windows desktop looks like to Go: check that internal/schedule still imports time/tzdata"
zone=$(curl -fsS "$base/datasets/$id/schedule" -H "$auth" | jq -r .timezone)
[ "$zone" = Europe/London ] || fail "the schedule came back in zone $zone"
ok "the binary carries its own zone database"

echo
echo "smoke: $IMAGE looks like an image somebody could deploy"
