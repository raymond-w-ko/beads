package issueops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	publicops "github.com/steveyegge/beads/issueops"
)

func validThreadGuard() *publicops.ThreadGuard {
	return &publicops.ThreadGuard{
		Cutoff:      time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		MemberLabel: "mail",
		Edge:        "replies-to",
		Threads: []publicops.GuardedThread{{
			Root:    "bd-root",
			Members: []publicops.GuardedMember{{ID: "bd-root", Revision: "r1"}},
		}},
	}
}

// TestDeleteInTxRefusesAThreadGuardWithCascade pins that the refusal comes
// before any read: the mock expects no statement after BEGIN, so a query the
// body issued first fails the test as unexpected.
func TestDeleteInTxRefusesAThreadGuardWithCascade(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectBegin()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	_, err = DeleteInTx(context.Background(), tx, publicops.DeleteRequest{
		IDs:         []string{"bd-root"},
		Cascade:     true,
		ThreadGuard: validThreadGuard(),
	})
	if !errors.Is(err, publicops.ErrValidation) {
		t.Fatalf("DeleteInTx with a thread guard and cascade: err = %v, want ErrValidation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestValidateThreadGuardRefusesAnUncheckableGuard(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*publicops.ThreadGuard)
	}{
		{"no member label", func(g *publicops.ThreadGuard) { g.MemberLabel = " " }},
		{"no edge", func(g *publicops.ThreadGuard) { g.Edge = "" }},
		{"no threads", func(g *publicops.ThreadGuard) { g.Threads = nil }},
		{"no root", func(g *publicops.ThreadGuard) { g.Threads[0].Root = "" }},
		{"no members", func(g *publicops.ThreadGuard) { g.Threads[0].Members = nil }},
		{"member with no id", func(g *publicops.ThreadGuard) { g.Threads[0].Members[0].ID = "" }},
		{"member with no revision", func(g *publicops.ThreadGuard) { g.Threads[0].Members[0].Revision = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			guard := validThreadGuard()
			test.mutate(guard)
			err := ValidateThreadGuard(publicops.DeleteRequest{IDs: []string{"bd-root"}, ThreadGuard: guard})
			if !errors.Is(err, publicops.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
	if err := ValidateThreadGuard(publicops.DeleteRequest{IDs: []string{"bd-root"}, ThreadGuard: validThreadGuard()}); err != nil {
		t.Errorf("a valid guard was refused: %v", err)
	}
	if err := ValidateThreadGuard(publicops.DeleteRequest{IDs: []string{"bd-root"}, Cascade: true}); err != nil {
		t.Errorf("cascade with no guard was refused: %v", err)
	}
}

func TestThreadGuardMismatchErrorNamesEveryThreadAndCheck(t *testing.T) {
	err := error(&publicops.ThreadGuardMismatchError{Failed: []publicops.ThreadGuardFailure{
		{Root: "bd-a", Check: publicops.ThreadGuardMembership, IDs: []string{"bd-x"}},
		{Root: "bd-b", Check: publicops.ThreadGuardRevision, IDs: []string{"bd-b", "bd-c"}},
		{Check: publicops.ThreadGuardCoverage, IDs: []string{"bd-y"}},
	}})
	if !errors.Is(err, publicops.ErrThreadGuardMismatch) {
		t.Errorf("errors.Is(ErrThreadGuardMismatch) = false for %v", err)
	}
	for _, want := range []string{"thread bd-a: membership (bd-x)", "thread bd-b: revision (bd-b, bd-c)", "request: coverage (bd-y)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}
