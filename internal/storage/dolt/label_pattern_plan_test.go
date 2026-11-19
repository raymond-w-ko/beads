package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// TestLabelPatternPlan (bd-pqgvg, from bd-ypak8) pins the plan of the ready
// read every seat runs on each wake, `bd ready --label-pattern 'class:*'`:
// over 20,000 issues and 30,000 labels, one issue in 7 with a class: label,
// after ANALYZE TABLE labels, the label subquery must read labels through
// its label index over a bounded range (the engine turns a bare
// `LIKE 'class:%'` into one), not probe labels once per open issue. Today's
// `LIKE ? ESCAPE '|'` keeps the LIKE a filter, which is the plan it replaces.
//
// Both plans are logged with dolt_version(). The investigator measured the
// seek on Dolt 2.6.2 only; on a server that does not seek even for the bare
// form, the test asserts only that the new plan has the old one's shape.
func TestLabelPatternPlan(t *testing.T) {
	primary, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	var version string
	if err := primary.db.QueryRowContext(ctx, "SELECT dolt_version()").Scan(&version); err != nil {
		t.Fatalf("dolt_version: %v", err)
	}
	start := time.Now()
	counts := seedLabelPatternPlanCorpus(t, ctx, primary.db, 20000)
	if counts["issues"] != 20000 || counts["labels"] != 30000 || counts["class"]*7 < 20000 {
		t.Fatalf("seeded %v, want 20000 issues, 30000 labels, a class label on 1 issue in 7", counts)
	}
	if _, err := primary.db.ExecContext(ctx, "ANALYZE TABLE labels"); err != nil {
		t.Fatalf("ANALYZE TABLE labels: %v", err)
	}
	t.Logf("dolt_version() = %s; seeded %v and analyzed labels in %v", version, counts, time.Since(start).Round(time.Millisecond))

	capture := &stmtCapture{}
	store := openCaptureStoreOnBranch(t, ctx, primary, capture)
	const glob = "class:*"
	for _, read := range []struct {
		name string
		run  func() (int, error)
	}{
		{"GetReadyWork", func() (int, error) {
			got, err := store.GetReadyWork(ctx, types.WorkFilter{LabelPattern: glob, Limit: 100})
			return len(got), err
		}},
		{"GetReadyWorkWithCounts", func() (int, error) {
			got, err := store.GetReadyWorkWithCounts(ctx, types.WorkFilter{LabelPattern: glob, Limit: 100})
			return len(got), err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			capture.take(true)
			rows, err := read.run()
			stmts := capture.take(false)
			if err != nil {
				t.Fatalf("%s: %v", read.name, err)
			}
			if rows == 0 {
				t.Fatalf("%s returned no rows for %q", read.name, glob)
			}
			var ready []capturedQuery
			for _, st := range stmts {
				if strings.Contains(st.query, "FROM labels WHERE label LIKE ?") {
					ready = append(ready, st)
				}
			}
			if len(ready) == 0 {
				t.Fatalf("%s sent no statement with the labels pattern clause; sent:\n%s", read.name, capturedText(stmts))
			}
			for _, st := range ready {
				if strings.Contains(st.query, "LIKE ? ESCAPE") {
					t.Fatalf("%s sent today's ESCAPE form for %q, want the bare LIKE:\n%s", read.name, glob, st.query)
				}
				newSQL := literalizeParams(st.query, lpoLiterals(t, st.args)...)
				oldQuery, idxs := (&lpoRewriter{}).rewriteQuery(st.query)
				oldArgs := append([]driver.NamedValue(nil), st.args...)
				for _, n := range idxs {
					oldArgs[n].Value = lpoOracleLikePattern(glob)
				}
				oldSQL := literalizeParams(oldQuery, lpoLiterals(t, oldArgs)...)

				newPlan := explainPlan(t, ctx, primary.db, newSQL)
				oldPlan := explainPlan(t, ctx, primary.db, oldSQL)
				t.Logf("dolt %s %s ready statement:\n%s", version, read.name, newSQL)
				t.Logf("dolt %s plan, old clause (LIKE ? ESCAPE '|'):\n%s", version, oldPlan)
				t.Logf("dolt %s plan, new clause (bare LIKE):\n%s", version, newPlan)

				if !looksLikeDoltPlan(newPlan) {
					t.Fatalf("EXPLAIN output not in a recognized Dolt plan format:\n%s", newPlan)
				}
				if seek, ok := labelIndexRangeSeek(newPlan); ok {
					t.Logf("dolt %s: label subquery seeks %s", version, seek)
					continue
				}
				newShape, oldShape := planShape(newPlan), planShape(oldPlan)
				t.Logf("no label seek on Dolt %s: new plan shape %v, old %v", version, newShape, oldShape)
				if strings.Join(newShape, "\n") != strings.Join(oldShape, "\n") {
					t.Fatalf("no label seek on Dolt %s, and the new plan's shape differs from the old one's:\nnew %v\nold %v", version, newShape, oldShape)
				}
			}
		})
	}
}

// seedLabelPatternPlanCorpus bulk-inserts n issues (35% open, 10%
// in_progress, 5% deferred to 2099, the rest closed) and 1.5n labels: area:*
// on every issue, class:* on every 7th and tier:* on enough others to reach
// the total.
func seedLabelPatternPlanCorpus(t *testing.T, ctx context.Context, db *sql.DB, n int) map[string]int {
	t.Helper()
	classes := []string{"investigator", "implementer", "lead", "reviewer", "pilot", "writer", "tester"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var issues, labels []string
	class, tier := 0, 0
	tiers := n*3/2 - n - (n+6)/7
	for i := 0; i < n; i++ {
		status, closedAt, deferUntil := "closed", fmt.Sprintf("'%s'", base.Add(time.Duration(i)*time.Minute).Format("2006-01-02 15:04:05")), "NULL"
		switch i % 20 {
		case 0, 1, 2, 3, 4, 5, 6:
			status, closedAt = "open", "NULL"
		case 7, 8:
			status, closedAt = "in_progress", "NULL"
		case 9:
			status, closedAt, deferUntil = "deferred", "NULL", "'2099-01-01 00:00:00'"
		}
		created := base.Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
		id := fmt.Sprintf("lpp-%06d", i)
		issues = append(issues, fmt.Sprintf("('%s','label plan %d','','','','','%s',%d,'task','%s','%s',%s,%s)", id, i, status, i%5, created, created, closedAt, deferUntil))
		labels = append(labels, fmt.Sprintf("('%s','area:%02d')", id, i%40))
		if i%7 == 0 {
			labels = append(labels, fmt.Sprintf("('%s','class:%s')", id, classes[(i/7)%len(classes)]))
			class++
		} else if tier < tiers {
			labels = append(labels, fmt.Sprintf("('%s','tier:%d')", id, i%4))
			tier++
		}
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %.120s: %v", q, err)
		}
	}
	bulk := func(head string, rows []string) {
		t.Helper()
		for s := 0; s < len(rows); s += 1000 {
			exec(head + strings.Join(rows[s:min(s+1000, len(rows))], ","))
		}
	}
	exec("SET FOREIGN_KEY_CHECKS=0")
	defer exec("SET FOREIGN_KEY_CHECKS=1")
	bulk("INSERT INTO issues (id,title,description,design,acceptance_criteria,notes,status,priority,issue_type,created_at,updated_at,closed_at,defer_until) VALUES ", issues)
	bulk("INSERT INTO labels (issue_id,label) VALUES ", labels)
	return map[string]int{"issues": len(issues), "labels": len(labels), "class": class}
}

// labelIndexRangeSeek finds an IndexedTableAccess of labels through an index
// that leads with labels.label, over a range bounded above, and returns its
// index and filter lines.
func labelIndexRangeSeek(plan string) (string, bool) {
	lines := strings.Split(plan, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "IndexedTableAccess(labels)") {
			continue
		}
		var index, filters string
		for _, next := range lines[i+1:] {
			if strings.Contains(next, "TableAccess(") {
				break
			}
			if s := strings.TrimSpace(strings.TrimLeft(next, "│├└─ ")); strings.HasPrefix(s, "index: [") {
				index = s
			} else if strings.HasPrefix(s, "filters: [") {
				filters = s
			}
		}
		if strings.HasPrefix(index, "index: [labels.label") && filters != "" && !strings.Contains(filters, "∞") && strings.Contains(filters, "class:") {
			return index + " " + filters, true
		}
	}
	return "", false
}

var planNodeRE = regexp.MustCompile(`^[A-Z][A-Za-z]*(\([a-z_]+\))?`)

// planShape lists a plan's node names (IndexedTableAccess(labels),
// SemiLookupJoin, ...) in order, without their expressions or ranges.
func planShape(plan string) []string {
	var out []string
	for _, line := range strings.Split(plan, "\n") {
		s := strings.TrimSpace(strings.TrimLeft(line, "│├└─ "))
		if m := planNodeRE.FindString(s); m != "" && (len(s) == len(m) || s[len(m)] == ' ' || s[len(m)] == '(') {
			out = append(out, m)
		}
	}
	return out
}

func capturedText(stmts []capturedQuery) string {
	var b strings.Builder
	for _, st := range stmts {
		fmt.Fprintf(&b, "  %.200s\n", st.query)
	}
	return b.String()
}

// lpoLiterals renders bound arguments as SQL literals for EXPLAIN.
func lpoLiterals(t *testing.T, args []driver.NamedValue) []string {
	t.Helper()
	out := make([]string, len(args))
	for i, a := range args {
		switch v := a.Value.(type) {
		case string:
			out[i] = "'" + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), "'", "''") + "'"
		case []byte:
			out[i] = "'" + strings.ReplaceAll(strings.ReplaceAll(string(v), `\`, `\\`), "'", "''") + "'"
		case time.Time:
			out[i] = "'" + v.UTC().Format("2006-01-02 15:04:05.999999") + "'"
		case int64, int, float64:
			out[i] = fmt.Sprint(v)
		case bool:
			out[i] = map[bool]string{true: "1", false: "0"}[v]
		case nil:
			out[i] = "NULL"
		default:
			t.Fatalf("lpoLiterals: unsupported argument %T (%v)", a.Value, a.Value)
		}
	}
	return out
}
