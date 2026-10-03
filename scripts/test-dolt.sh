#!/usr/bin/env bash
# Throwaway Dolt sql-server for manual server-mode smoke tests of a remote-only
# bd (docker/test-dolt). It is never the production server.
#
#   scripts/test-dolt.sh build   build the image
#   scripts/test-dolt.sh start   build if needed, start, wait for a beads login
#   scripts/test-dolt.sh stop    remove the container
#   scripts/test-dolt.sh env     print the exports that point bd at it
#
# start and stop are idempotent. Use it with: eval "$(scripts/test-dolt.sh env)"

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CONTEXT="$REPO_ROOT/docker/test-dolt"

CONTAINER="bd-test-dolt"
HOST="127.0.0.1"
PORT=3317
DB_USER="beads"
DB_PASSWORD="bd-test" # test-only; must match docker/test-dolt/init.sql
TMPFS_SIZE="2g"
READY_TIMEOUT_SECONDS=120

die() {
    echo "test-dolt: $*" >&2
    exit 1
}

# The image tag follows the build context, so an edited Dockerfile or init.sql
# builds a new image.
image_tag() {
    local hash
    hash="$(cat "$CONTEXT/Dockerfile" "$CONTEXT/init.sql" | sha256sum | cut -c1-12)"
    echo "$CONTAINER:$hash"
}

container_status() {
    docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true
}

cmd_build() {
    local tag
    tag="$(image_tag)"
    docker build -q -t "$tag" "$CONTEXT" >/dev/null || die "could not build $tag"
    echo "$tag"
}

# A beads login over TCP that can read mysql.user: init.sql grants last, so
# this succeeds only once initialisation is done.
login_works() {
    docker exec "$CONTAINER" dolt --host 127.0.0.1 --port 3306 --no-tls \
        --user "$DB_USER" --password "$DB_PASSWORD" \
        sql -q "SELECT 1 FROM mysql.user LIMIT 1" >/dev/null 2>&1
}

# The host side of the binding, so a ready server is one bd can reach.
port_open() {
    (exec 3<>"/dev/tcp/$HOST/$PORT") 2>/dev/null
}

cmd_start() {
    command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
    local tag status
    tag="$(image_tag)"
    status="$(container_status)"
    if [[ "$status" != "running" ]]; then
        if [[ -n "$status" ]]; then
            docker rm -f "$CONTAINER" >/dev/null
        fi
        if ! docker image inspect "$tag" >/dev/null 2>&1; then
            cmd_build >/dev/null
        fi
        docker run -d --rm --name "$CONTAINER" \
            --tmpfs "/var/lib/dolt:size=$TMPFS_SIZE" \
            -p "$HOST:$PORT:3306" "$tag" >/dev/null ||
            die "could not start $CONTAINER on $HOST:$PORT"
    fi

    local deadline=$((SECONDS + READY_TIMEOUT_SECONDS))
    while ((SECONDS < deadline)); do
        if [[ "$(container_status)" != "running" ]]; then
            die "$CONTAINER exited while starting"
        fi
        if port_open && login_works; then
            echo "$CONTAINER ready on $HOST:$PORT"
            return 0
        fi
        sleep 0.5
    done
    docker logs --tail 20 "$CONTAINER" >&2 || true
    die "$CONTAINER was not ready within ${READY_TIMEOUT_SECONDS}s"
}

cmd_stop() {
    if [[ -n "$(container_status)" ]]; then
        docker rm -f "$CONTAINER" >/dev/null
    fi
}

cmd_env() {
    echo "export BEADS_DOLT_SERVER_HOST=$HOST BEADS_DOLT_SERVER_PORT=$PORT BEADS_DOLT_SERVER_USER=$DB_USER BEADS_DOLT_PASSWORD=$DB_PASSWORD"
}

case "${1:-}" in
    build) cmd_build ;;
    start) cmd_start ;;
    stop) cmd_stop ;;
    env) cmd_env ;;
    *)
        echo "usage: $0 build|start|stop|env" >&2
        exit 2
        ;;
esac
