# The throwaway Dolt server for manual server-mode smoke tests of this fork's
# remote-only bd: scripts/test-dolt.sh builds, starts and removes it. Data
# lives on tmpfs and the port is bound to 127.0.0.1 only. It is never the
# production server. See README.md.

# Dolt 2.6.2 exactly. Upstream beads requires Dolt 2.6.0 or later
# (scripts/ci/install-dolt.sh, internal/testutil); 2.6.2 adds 2.6.1's DOLT_GC
# and memory fixes and a server-crash fix in generated-column index rows. Same
# digest as everturn's docker/test-dolt; raise it only together with that pin.
# dolthub/dolt-sql-server:2.6.2 (multi-arch index digest)
FROM dolthub/dolt-sql-server@sha256:c8f51ad58aaa585850dea9fd77268aab3de1aaef4cd48ce83900eb103ea5ebf7

COPY init.sql /docker-entrypoint-initdb.d/init.sql
