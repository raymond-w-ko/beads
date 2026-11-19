package dolt

import (
	"slices"
	"testing"
)

// TestCustomTypesRepeatedNameStoredOnce (bd-w2ji5): `bd config set
// types.custom "gone away,gone away"` failed with a raw 1062 (duplicate
// primary key) because the sync inserted the repeated name twice into
// custom_types. It now succeeds and stores the name once. Names that differ
// only in case stay two rows: the table's key compares them byte-wise.
func TestCustomTypesRepeatedNameStoredOnce(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	names := func() []string {
		t.Helper()
		rows, err := store.db.QueryContext(ctx, "SELECT name FROM custom_types ORDER BY name")
		if err != nil {
			t.Fatalf("read custom_types: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan custom_types: %v", err)
			}
			out = append(out, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read custom_types: %v", err)
		}
		return out
	}

	if err := store.SetConfig(ctx, "types.custom", "gone away,gone away"); err != nil {
		t.Fatalf("SetConfig(types.custom, \"gone away,gone away\"): %v", err)
	}
	if got := names(); !slices.Equal(got, []string{"gone away"}) {
		t.Errorf("custom_types after a repeated name = %q, want one row [\"gone away\"]", got)
	}

	if err := store.SetConfig(ctx, "types.custom", "Broken Pipe,broken pipe"); err != nil {
		t.Fatalf("SetConfig(types.custom, \"Broken Pipe,broken pipe\"): %v", err)
	}
	if got := names(); len(got) != 2 || !slices.Contains(got, "Broken Pipe") || !slices.Contains(got, "broken pipe") {
		t.Errorf("custom_types after two case spellings = %q, want two rows, \"Broken Pipe\" and \"broken pipe\"", got)
	}
}
