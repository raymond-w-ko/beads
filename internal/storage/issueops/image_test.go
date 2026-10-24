package issueops

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	publicops "github.com/steveyegge/beads/issueops"
)

func imageFixture(reverse bool) publicops.Image {
	labels := []map[string]any{
		{"issue_id": "bd-1", "label": "a"},
		{"issue_id": "bd-1", "label": "b"},
	}
	events := []map[string]any{
		{"id": "e1", "issue_id": "bd-1", "event_type": "created", "comment": nil},
		{"id": "e2", "issue_id": "bd-1", "event_type": "closed", "comment": nil},
	}
	row := map[string]any{}
	keys := []string{"id", "title", "metadata", "priority", "closed_at"}
	vals := []any{"bd-1", "T <&>", `{"k": 1}`, int64(2), nil}
	if reverse {
		labels[0], labels[1] = labels[1], labels[0]
		events[0], events[1] = events[1], events[0]
		for i := len(keys) - 1; i >= 0; i-- {
			row[keys[i]] = vals[i]
		}
	} else {
		for i := range keys {
			row[keys[i]] = vals[i]
		}
	}
	return publicops.Image{ID: "bd-1", Table: "issues", Row: row, Labels: labels, Events: events}
}

func TestCanonicalImageJSONStableAndOrderIndependent(t *testing.T) {
	a, err := CanonicalImageJSON(imageFixture(false))
	if err != nil {
		t.Fatal(err)
	}
	again, err := CanonicalImageJSON(imageFixture(false))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, again) {
		t.Fatalf("two calls differ:\n%s\n%s", a, again)
	}
	for i := 0; i < 20; i++ {
		b, err := CanonicalImageJSON(imageFixture(true))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("row or key order changed the canonical form:\n%s\n%s", a, b)
		}
	}
	want := `{"comments":[],"dependencies":[],"events":[{"comment":null,"event_type":"created","id":"e1","issue_id":"bd-1"},{"comment":null,"event_type":"closed","id":"e2","issue_id":"bd-1"}],` +
		`"id":"bd-1","labels":[{"issue_id":"bd-1","label":"a"},{"issue_id":"bd-1","label":"b"}],` +
		`"row":{"closed_at":null,"id":"bd-1","metadata":"{\"k\": 1}","priority":2,"title":"T <&>"},"table":"issues"}`
	if string(a) != want {
		t.Fatalf("canonical form\n got %s\nwant %s", a, want)
	}

	// The caller's slices are not reordered.
	img := imageFixture(true)
	if _, err := CanonicalImageJSON(img); err != nil {
		t.Fatal(err)
	}
	if img.Labels[0]["label"] != "b" {
		t.Fatalf("CanonicalImageJSON reordered the caller's labels: %v", img.Labels)
	}
}

func TestComputeImageRevisionIgnoresRevisionField(t *testing.T) {
	img := imageFixture(false)
	rev := ComputeImageRevision(img)
	if len(rev) != 64 {
		t.Fatalf("revision %q is not sha256 hex", rev)
	}
	img.Revision = "stale"
	if got := ComputeImageRevision(img); got != rev {
		t.Fatalf("revision depends on the Revision field: %s vs %s", got, rev)
	}
	withRev, err := CanonicalImageJSON(img)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(withRev, []byte(`"revision":"stale"`)) {
		t.Fatalf("canonical form drops a set revision: %s", withRev)
	}
	img.Revision = ""
	img.Labels = append(img.Labels, map[string]any{"issue_id": "bd-1", "label": "c"})
	if ComputeImageRevision(img) == rev {
		t.Fatal("a label add did not change the revision")
	}
}

func TestImageInTxMissingIDsNamedTogether(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM wisps LIMIT 1")).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM issues WHERE id IN (?,?,?)")).
		WithArgs("bd-1", "bd-x", "bd-y").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("bd-1"))

	images, err := ImageInTx(context.Background(), db, []string{"bd-1", "bd-x", "bd-1", "bd-y"})
	if images != nil {
		t.Fatalf("partial result returned: %v", images)
	}
	if !errors.Is(err, publicops.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var nf *publicops.NotFoundError
	if !errors.As(err, &nf) || len(nf.IDs) != 2 || nf.IDs[0] != "bd-x" || nf.IDs[1] != "bd-y" {
		t.Fatalf("err = %#v, want both missing ids", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestImageInTxValidation(t *testing.T) {
	for _, ids := range [][]string{nil, {"bd-1", " "}} {
		if _, err := ImageInTx(context.Background(), nil, ids); !errors.Is(err, publicops.ErrValidation) {
			t.Fatalf("ids %q: err = %v, want ErrValidation", ids, err)
		}
	}
}

// showColumns answers SHOW COLUMNS; each row is field, type, extra.
func showColumns(rows ...[3]string) *sqlmock.Rows {
	out := sqlmock.NewRows([]string{"Field", "Type", "Null", "Key", "Default", "Extra"})
	for _, r := range rows {
		out.AddRow(r[0], r[1], "YES", "", nil, r[2])
	}
	return out
}

// expectColumns expects one table's discovery: SHOW CREATE TABLE, whose ddl
// is the column definition lines, then SHOW COLUMNS.
func expectColumns(mock sqlmock.Sqlmock, table string, ddl []string, cols ...[3]string) {
	create := "CREATE TABLE `" + table + "` (\n  " + strings.Join(ddl, ",\n  ") + "\n) ENGINE=InnoDB"
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE " + table)).
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow(table, create))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW COLUMNS FROM " + table)).WillReturnRows(showColumns(cols...))
}

// TestImageInTxRawRowsFromTextProtocol drives one durable image through the
// text-protocol shape (every value []byte): a generated column is neither
// selected nor imaged whether only the table definition says so (Dolt 2.2.0)
// or only Extra does, an expression default is not mistaken for one, integers
// and timestamps normalize by declared type, and a missing wisp edge table
// means no wisp edges.
func TestImageInTxRawRowsFromTextProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM wisps LIMIT 1")).WillReturnError(missingTableError("wisps"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM issues WHERE id IN (?)")).
		WithArgs("bd-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("bd-1"))
	expectColumns(mock, "issues",
		[]string{
			"`id` varchar(255) NOT NULL",
			"`priority` int NOT NULL DEFAULT '2'",
			"`metadata` json DEFAULT (json_object())",
			"`created_at` datetime(6) DEFAULT CURRENT_TIMESTAMP(6)",
			"`closed_at` datetime",
			"`everturn_action` varchar(255) GENERATED ALWAYS AS (LEFT(json_unquote(json_extract(`metadata`, '$.everturn.action')), 255)) STORED",
			"`v` int",
		},
		[3]string{"id", "varchar(255)", ""},
		[3]string{"priority", "int", ""},
		[3]string{"metadata", "json", "DEFAULT_GENERATED"},
		[3]string{"created_at", "datetime(6)", "DEFAULT_GENERATED on update CURRENT_TIMESTAMP(6)"},
		[3]string{"closed_at", "datetime", ""},
		[3]string{"everturn_action", "varchar(255)", ""},
		[3]string{"v", "int", "VIRTUAL GENERATED"},
	)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `id`, `priority`, `metadata`, `created_at`, `closed_at` FROM issues WHERE `id` = ?")).
		WithArgs("bd-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "priority", "metadata", "created_at", "closed_at"}).
			AddRow([]byte("bd-1"), []byte("2"), []byte(`{"everturn": {"action": "x"}}`), []byte("2026-10-24 17:05:44.120000"), nil))
	expectColumns(mock, "labels", nil, [3]string{"issue_id", "varchar(255)", ""}, [3]string{"label", "varchar(255)", ""})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `issue_id`, `label` FROM labels WHERE `issue_id` = ?")).
		WithArgs("bd-1").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id", "label"}).AddRow([]byte("bd-1"), []byte("z")).AddRow([]byte("bd-1"), []byte("a")))
	expectColumns(mock, "comments", nil, [3]string{"id", "char(36)", ""}, [3]string{"issue_id", "varchar(255)", ""})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `id`, `issue_id` FROM comments WHERE `issue_id` = ?")).
		WithArgs("bd-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "issue_id"}))
	expectColumns(mock, "events", nil, [3]string{"id", "char(36)", ""}, [3]string{"issue_id", "varchar(255)", ""})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `id`, `issue_id` FROM events WHERE `issue_id` = ?")).
		WithArgs("bd-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "issue_id"}))
	expectColumns(mock, "dependencies", nil,
		[3]string{"id", "char(36)", ""},
		[3]string{"issue_id", "varchar(255)", ""},
		[3]string{"depends_on_issue_id", "varchar(255)", ""},
		[3]string{"depends_on_wisp_id", "varchar(255)", ""},
		[3]string{"depends_on_external", "varchar(255)", ""},
	)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `id`, `issue_id`, `depends_on_issue_id`, `depends_on_wisp_id`, `depends_on_external` FROM dependencies WHERE `issue_id` = ? OR `depends_on_issue_id` = ? OR `depends_on_wisp_id` = ?")).
		WithArgs("bd-1", "bd-1", "bd-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "issue_id", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}).
			AddRow([]byte("d1"), []byte("bd-2"), []byte("bd-1"), nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE wisp_dependencies")).
		WillReturnError(missingTableError("wisp_dependencies"))

	images, err := ImageInTx(context.Background(), db, []string{"bd-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 {
		t.Fatalf("got %d images", len(images))
	}
	img := images[0]
	if img.Table != "issues" || img.ID != "bd-1" {
		t.Fatalf("image header = %q %q", img.ID, img.Table)
	}
	for _, col := range []string{"everturn_action", "v"} {
		if _, ok := img.Row[col]; ok {
			t.Fatalf("generated column %s imaged: %v", col, img.Row)
		}
	}
	if img.Row["priority"] != int64(2) {
		t.Fatalf("priority = %#v, want int64(2)", img.Row["priority"])
	}
	if img.Row["metadata"] != `{"everturn": {"action": "x"}}` {
		t.Fatalf("metadata = %#v, want the stored text", img.Row["metadata"])
	}
	if img.Row["created_at"] != "2026-10-24T17:05:44.120000Z" {
		t.Fatalf("created_at = %#v", img.Row["created_at"])
	}
	if v, ok := img.Row["closed_at"]; !ok || v != nil {
		t.Fatalf("closed_at = %#v (present %v), want null", v, ok)
	}
	if len(img.Labels) != 2 || img.Labels[0]["label"] != "a" {
		t.Fatalf("labels not sorted by key: %v", img.Labels)
	}
	if img.Comments == nil || img.Events == nil || len(img.Dependencies) != 1 {
		t.Fatalf("lists = %v %v %v", img.Comments, img.Events, img.Dependencies)
	}
	if img.Revision != ComputeImageRevision(img) {
		t.Fatal("Revision is not ComputeImageRevision of the image")
	}
}

func TestNormalizeImageValueDriverShapesAgree(t *testing.T) {
	cases := []struct {
		typ  string
		a, b any
	}{
		{"int", []byte("7"), int64(7)},
		{"bigint unsigned", []byte("7"), uint64(7)},
		{"tinyint(1)", "1", int64(1)},
		{"double", []byte("1.5"), 1.5},
		{"datetime", []byte("2026-01-02 03:04:05"), "2026-01-02T03:04:05Z"},
		{"varchar(255)", []byte("x"), "x"},
	}
	for _, c := range cases {
		a, err := normalizeImageValue(c.typ, c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := normalizeImageValue(c.typ, c.b)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("%s: %#v vs %#v", c.typ, a, b)
		}
	}
}
