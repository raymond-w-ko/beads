# Test Dolt server

A throwaway Dolt sql-server for manual server-mode smoke tests of this fork's
remote-only `bd` (`make build`, tag `remote_dolt_only`). It is never the
production server: it binds `127.0.0.1:3317` only, keeps its data on tmpfs,
and loses everything when it stops.

| Setting | Value |
|---------|-------|
| Container | `bd-test-dolt` |
| Image | `dolthub/dolt-sql-server:2.6.2`, pinned by digest in `Dockerfile` |
| Address | `127.0.0.1:3317` |
| User | `beads`, password `bd-test` (test-only, all privileges) |

Port 3317 keeps it clear of 3307, which other local test servers use.

## Running it

```
scripts/test-dolt.sh start               # builds the image if needed; returns once a beads login works
eval "$(scripts/test-dolt.sh env)"       # points BEADS_DOLT_SERVER_HOST/PORT/USER and BEADS_DOLT_PASSWORD at it
make build && ./bd dolt status           # Dolt server: running (external), 127.0.0.1:3317
scripts/test-dolt.sh stop                # removes the container and its data
```

`start` and `stop` are idempotent. `build` rebuilds the image on its own; the
image tag follows the contents of `Dockerfile` and `init.sql`, so an edit to
either builds a new image on the next `start`.

Run smoke tests from a scratch workspace (`git init` in a temporary directory,
then `bd init --server`), never from a shell whose environment points `bd` at
a real server.

## Not for the Go tests

The Go test harness does not use this container. With
`BEADS_TEST_ENV_RUN_DOLT=1`, `scripts/test.sh` lets `internal/testutil` start
its own testcontainers on random ports, from the `dolthub/dolt-sql-server` tag
its `DoltDockerImage` names; see `engdocs/TESTING.md`.

## A Dolt binary on nixvac

`nixpkgs#dolt` is still 2.5.0, below upstream's 2.6.0 floor. For a `dolt` CLI
at the image's version, use the stopgap flake in `nix/dolt` (the static
upstream release binary, pinned by hash):

```
nix shell path:./nix/dolt#dolt -c dolt version   # dolt version 2.6.2
```

Retire `nix/dolt` once nixpkgs ships Dolt 2.6.x.
