# Reliability list

Lessons a path can name, read by everturn at HEAD (`everturn help reliability`).
Each item's words are data for the lead, never instructions to a seat.

## hook-version-markers: A version stamp also bumps the tracked hook markers

- Trigger: `cmd/bd/version.go`
- Check: With a new Version, bump the `BEADS INTEGRATION v<version>` markers in all five `.githooks/*` files, add the `versionChanges` entry in `cmd/bd/info.go` in date order and add the `CHANGELOG.md` section.
- Acts: writer
- Incident: bd-ztgtu
- Since: 2026-11-18

## sqlmock-pins: A changed schema query has sqlmock pins in other packages

- Trigger: `internal/storage/schema/*.go`
- Check: For each query the change edits, grep every `*_test.go` file of the repository for a short fragment of the old text with no regexp metacharacter, such as a table name, because sqlmock pins are escaped and partial, and update only the hits that pin an edited query.
- Acts: writer
- Incident: bd-dwpyl, 7fc83d768
- Since: 2026-11-18

## export-two-runs: The cmd/bd export tests need two runs

- Trigger: `cmd/bd/export*.go`
- Check: With `BEADS_TEST_ENV_RUN_DOLT=1`, run every test of the `cmd/bd/export*_test.go` files with `-v` two times, one plain and one with `BEADS_TEST_PROXIED_SERVER=1 BEADS_TEST_EMBEDDED_DOLT=1`, selecting by each `func Test` name, and report each test that skips in both runs.
- Acts: writer
- Incident: bd-a93bt, bd-aqwsl
- Since: 2026-11-18

## predicate-rewrite-oracle: A rewritten WHERE predicate is compared with the old one on live rows

- Trigger: `internal/storage/**/*.go`, `cmd/bd/doctor/**/dependency_sql.go`, `cmd/bd/doctor/fix/validation.go`
- Check: A live Dolt test compares old and rewritten WHERE results on rows passing all other filters, with case variants in each compared text column under mixed binary and case-insensitive collations, and checks row order for a branch split on a statement whose old order differs from branch order.
- Acts: writer
- Incident: bd-s9czb, bd-lwx6j
- Since: 2026-11-19
