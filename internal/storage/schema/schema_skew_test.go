package schema

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// -- checkSchemaSkew unit tests (mock DB) --

func TestCheckSchemaSkew_FreshDB_NoError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expectCursorProbe(mock, "schema_migrations", true)
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(0))

	if err := checkSchemaSkew(context.Background(), db); err != nil {
		t.Fatalf("checkSchemaSkew = %v, want nil for fresh DB (version=0)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestCheckSchemaSkew_EqualVersion_NoError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expectCursorProbe(mock, "schema_migrations", true)
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(LatestVersion()))

	if err := checkSchemaSkew(context.Background(), db); err != nil {
		t.Fatalf("checkSchemaSkew = %v, want nil when DB at binary version", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestCheckSchemaSkew_OneAhead_ReturnsSchemaSkewError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	dbVersion := LatestVersion() + 1
	expectCursorProbe(mock, "schema_migrations", true)
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(dbVersion))

	got := checkSchemaSkew(context.Background(), db)
	if got == nil {
		t.Fatal("checkSchemaSkew = nil, want error when DB one migration ahead")
	}
	var skewErr *SchemaSkewError
	if !errors.As(got, &skewErr) {
		t.Fatalf("error type = %T (%v), want *SchemaSkewError", got, got)
	}
	if skewErr.DBVersion != dbVersion {
		t.Errorf("DBVersion = %d, want %d", skewErr.DBVersion, dbVersion)
	}
	if skewErr.BinaryVersion != LatestVersion() {
		t.Errorf("BinaryVersion = %d, want %d", skewErr.BinaryVersion, LatestVersion())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestCheckSchemaSkew_ThreeAhead_ReturnsSchemaSkewError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	dbVersion := LatestVersion() + 3
	expectCursorProbe(mock, "schema_migrations", true)
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(dbVersion))

	got := checkSchemaSkew(context.Background(), db)
	if got == nil {
		t.Fatal("checkSchemaSkew = nil, want error when DB three migrations ahead")
	}
	var skewErr *SchemaSkewError
	if !errors.As(got, &skewErr) {
		t.Fatalf("error type = %T (%v), want *SchemaSkewError", got, got)
	}
	if skewErr.DBVersion != dbVersion {
		t.Errorf("DBVersion = %d, want %d", skewErr.DBVersion, dbVersion)
	}
	if skewErr.BinaryVersion != LatestVersion() {
		t.Errorf("BinaryVersion = %d, want %d", skewErr.BinaryVersion, LatestVersion())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestCheckSchemaSkew_EscapeHatch_ReturnsNilAndWarns(t *testing.T) {
	t.Setenv("BD_IGNORE_SCHEMA_SKEW", "1")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	dbVersion := LatestVersion() + 3
	expectCursorProbe(mock, "schema_migrations", true)
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(dbVersion))

	// Capture stderr to verify warning is emitted.
	origStderr := os.Stderr
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	os.Stderr = w

	gotErr := checkSchemaSkew(context.Background(), db)

	_ = w.Close()
	os.Stderr = origStderr

	var buf bytes.Buffer
	if _, copyErr := io.Copy(&buf, r); copyErr != nil {
		t.Fatal(copyErr)
	}
	_ = r.Close()

	if gotErr != nil {
		t.Fatalf("checkSchemaSkew = %v, want nil when BD_IGNORE_SCHEMA_SKEW=1", gotErr)
	}

	wantWarning := fmt.Sprintf(
		"Warning: schema skew ignored — database (v%d) is ahead of binary (v%d); some queries may fail",
		dbVersion, LatestVersion(),
	)
	if !strings.Contains(buf.String(), wantWarning) {
		t.Errorf("stderr = %q\nwant to contain warning:\n  %q", buf.String(), wantWarning)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestCheckSchemaSkew_MissingTable_NoError covers the writable open path, where
// CheckForwardDrift runs before initSchema creates schema_migrations on a fresh
// database. A table-not-exist error must be treated as version 0 (no forward
// drift), not surfaced as a skew-check failure.
func TestCheckSchemaSkew_MissingTable_NoError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// With the be-bv7x probe in front, an absent cursor table is reported by
	// the probe returning 0 rather than by the cursor read erroring out.
	expectCursorProbe(mock, "schema_migrations", false)

	if err := checkSchemaSkew(context.Background(), db); err != nil {
		t.Fatalf("checkSchemaSkew = %v, want nil when schema_migrations is absent (fresh DB)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestCheckForwardDrift_Conn_Ahead confirms the exported guard accepts a DBConn
// (so the embedded writable path can pass a pinned *sql.Conn) and still reports
// forward drift.
func TestCheckForwardDrift_Conn_Ahead(t *testing.T) {
	ctx := context.Background()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	dbVersion := LatestVersion() + 2
	expectCursorProbe(mock, "schema_migrations", true)
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(dbVersion))

	got := CheckForwardDrift(ctx, conn)
	if !IsSchemaSkewError(got) {
		t.Fatalf("CheckForwardDrift = %v (%T), want *SchemaSkewError", got, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// -- SchemaSkewError message copy tests --

func TestSchemaSkewError_Error_Singular(t *testing.T) {
	e := &SchemaSkewError{DBVersion: 43, BinaryVersion: 42}
	want := "schema version mismatch: database is at v43, binary knows up to v42 (1 migration ahead)"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q\nwant:   %q", got, want)
	}
}

func TestSchemaSkewError_Error_Plural(t *testing.T) {
	e := &SchemaSkewError{DBVersion: 45, BinaryVersion: 42}
	want := "schema version mismatch: database is at v45, binary knows up to v42 (3 migrations ahead)"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q\nwant:   %q", got, want)
	}
}

func TestSchemaSkewError_UserMessage_ExactCopy(t *testing.T) {
	e := &SchemaSkewError{DBVersion: 45, BinaryVersion: 42}
	want := "schema version mismatch: database is at v45, binary knows up to v42 (3 migrations ahead)\n" +
		"\n" +
		"  Your bd binary is stale. Queries for dropped or renamed columns will fail\n" +
		"  with cryptic SQL errors (e.g. \"column X could not be found in any table in scope\").\n" +
		"\n" +
		"  Rebuild from main:\n" +
		"    CGO_ENABLED=0 go build -tags gms_pure_go ./cmd/bd\n" +
		"\n" +
		"  Or install the latest release:\n" +
		"    CGO_ENABLED=0 go install -tags gms_pure_go github.com/steveyegge/beads/cmd/bd@latest\n" +
		"\n" +
		"  To proceed despite the risk (some read commands may still work):\n" +
		"    BD_IGNORE_SCHEMA_SKEW=1 bd <command>\n" +
		"    bd --ignore-schema-skew <command>\n"
	if got := e.UserMessage(); got != want {
		t.Errorf("UserMessage() mismatch.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestSchemaSkewError_EscapeHint_ExactCopy(t *testing.T) {
	e := &SchemaSkewError{DBVersion: 45, BinaryVersion: 42}
	want := "BD_IGNORE_SCHEMA_SKEW=1 bd <command>  or  bd --ignore-schema-skew <command>"
	if got := e.EscapeHint(); got != want {
		t.Errorf("EscapeHint() = %q\nwant:         %q", got, want)
	}
}

// -- IsSchemaSkewError tests --

func TestIsSchemaSkewError_DirectError(t *testing.T) {
	err := &SchemaSkewError{DBVersion: 45, BinaryVersion: 42}
	if !IsSchemaSkewError(err) {
		t.Error("IsSchemaSkewError(*SchemaSkewError) = false, want true")
	}
}

func TestIsSchemaSkewError_WrappedError(t *testing.T) {
	skew := &SchemaSkewError{DBVersion: 45, BinaryVersion: 42}
	wrapped := fmt.Errorf("schema version check: %w", skew)
	doubleWrapped := fmt.Errorf("failed to open database: %w", wrapped)
	if !IsSchemaSkewError(doubleWrapped) {
		t.Error("IsSchemaSkewError(wrapped *SchemaSkewError) = false, want true")
	}
}

func TestIsSchemaSkewError_OtherError(t *testing.T) {
	err := errors.New("some unrelated error")
	if IsSchemaSkewError(err) {
		t.Error("IsSchemaSkewError(non-SchemaSkewError) = true, want false")
	}
}

// queryRecordingConn records every statement sent through it, so a test can
// prove a statement was NOT sent: sqlmock alone forgets an unexpected query
// when the caller swallows the error.
type queryRecordingConn struct {
	DBConn
	queries []string
}

func (c *queryRecordingConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.queries = append(c.queries, query)
	return c.DBConn.ExecContext(ctx, query, args...)
}

func (c *queryRecordingConn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.queries = append(c.queries, query)
	return c.DBConn.QueryContext(ctx, query, args...)
}

func (c *queryRecordingConn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.queries = append(c.queries, query)
	return c.DBConn.QueryRowContext(ctx, query, args...)
}

// TestCurrentVersionProbesWithShowTables pins the open-time cursor probe to
// SHOW TABLES LIKE. An INFORMATION_SCHEMA.TABLES scan is not pushed down to
// the current database on Dolt: it walks every database on the server
// (seconds on a busy one) and fails with 1105 "no root value found in
// session" when another session drops a database mid-scan.
func TestCurrentVersionProbesWithShowTables(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present bool
		want    int
	}{
		{"cursor table present", true, LatestVersion()},
		{"cursor table absent", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()

			expectCursorProbe(mock, "schema_migrations", tc.present)
			if tc.present {
				expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", tc.want)
			}

			conn := &queryRecordingConn{DBConn: db}
			got, err := CurrentVersion(context.Background(), conn)
			if err != nil {
				t.Fatalf("CurrentVersion: %v", err)
			}
			if got != tc.want {
				t.Fatalf("CurrentVersion = %d, want %d", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
			if len(conn.queries) == 0 || !strings.HasPrefix(conn.queries[0], "SHOW TABLES LIKE ") {
				t.Fatalf("first statement = %q, want a SHOW TABLES LIKE probe", conn.queries)
			}
			for _, q := range conn.queries {
				if strings.Contains(strings.ToLower(q), "information_schema") {
					t.Fatalf("CurrentVersion issued an information_schema query: %q", q)
				}
			}
		})
	}
}

// TestSchemaProbesRejectNonIdentifierNames: the SHOW probes splice the name
// into the statement text, so anything but [A-Za-z0-9_] is refused before a
// statement is sent.
func TestSchemaProbesRejectNonIdentifierNames(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	conn := &queryRecordingConn{DBConn: db}
	if _, err := schemaTableExists(ctx, conn, "issues' OR '1"); err == nil {
		t.Error("schemaTableExists accepted a quoted table name")
	}
	if _, err := schemaColumnExists(ctx, conn, "issues`", "id"); err == nil {
		t.Error("schemaColumnExists accepted a backquoted table name")
	}
	if _, err := schemaColumnExists(ctx, conn, "issues", "id%"); err == nil {
		t.Error("schemaColumnExists accepted a LIKE pattern as a column name")
	}
	if len(conn.queries) != 0 {
		t.Fatalf("rejected names still reached the database: %q", conn.queries)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
