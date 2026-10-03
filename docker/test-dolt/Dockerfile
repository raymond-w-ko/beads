# The throwaway Dolt server for manual server-mode smoke tests of this fork's
# remote-only bd: scripts/test-dolt.sh builds, starts and removes it. Data
# lives on tmpfs and the port is bound to 127.0.0.1 only. It is never the
# production server. See README.md.

# Dolt 2.2.0 exactly, the version beads pins (scripts/ci/install-dolt.sh) and
# the image the Go test harness uses (internal/testutil). Same digest as
# everturn's docker/test-dolt; raise it only together with that pin.
# dolthub/dolt-sql-server:2.2.0
FROM dolthub/dolt-sql-server@sha256:baf9449a54dc6572dc5d688386e6f45ecb9f76e5bc4869be429f450b90ffecb0

COPY init.sql /docker-entrypoint-initdb.d/init.sql
