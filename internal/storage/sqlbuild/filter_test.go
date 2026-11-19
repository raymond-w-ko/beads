package sqlbuild

import (
	"math/rand"
	"strings"
	"testing"
)

// TestGlobToLikePattern exercises globToLikePattern directly against the
// package that actually calls it from BuildIssueFilterClauses (be-ucslk4).
// This used to be pinned only against a byte-identical but unreachable copy
// in internal/storage/issueops, so an escaping regression in this — the
// live — implementation could ship unseen.
func TestGlobToLikePattern(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "trailing star", in: "tech-*", want: "tech-%"},
		{name: "surrounding stars", in: "*foo*", want: "%foo%"},
		{name: "question mark", in: "v?", want: "v_"},
		{name: "literal percent", in: "5%", want: "5|%"},
		{name: "literal underscore", in: "snake_case", want: "snake|_case"},
		{name: "literal pipe", in: "a|b", want: "a||b"},
		{name: "no metachars", in: "needs-pm", want: "needs-pm"},
		{name: "empty", in: "", want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := globToLikePattern(tc.in)
			if got != tc.want {
				t.Errorf("globToLikePattern(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLabelPatternClause pins bd-ypak8 and bd-q6rw5: a valid UTF-8 glob
// without literal %, _, \ or | is sent as a bare LIKE, which the engine can
// turn into an idx_labels_label range; any other glob keeps LIKE ... ESCAPE
// '|' (see labelPatternClause for why each of them does).
func TestLabelPatternClause(t *testing.T) {
	t.Parallel()

	const bare = "id IN (SELECT issue_id FROM labels WHERE label LIKE ?)"
	const escaped = "id IN (SELECT issue_id FROM labels WHERE label LIKE ? ESCAPE '|')"
	cases := []struct {
		glob, clause, pattern string
	}{
		{"class:*", bare, "class:%"},
		{"tech-*", bare, "tech-%"},
		{"*foo*", bare, "%foo%"},
		{"v?", bare, "v_"},
		{"a|b*", escaped, "a||b%"},
		{"needs-pm", bare, "needs-pm"},
		{"5%*", escaped, "5|%%"},
		{"snake_case*", escaped, "snake|_case%"},
		{`a\b*`, escaped, `a\b%`},
		{"a|_*", escaped, "a|||_%"},
		{"tag-\xff*", escaped, "tag-\uFFFD%"},
		{"tag-\uFFFD*", bare, "tag-\uFFFD%"},
	}
	for _, tc := range cases {
		clause, pattern := labelPatternClause("labels", tc.glob)
		if clause != tc.clause || pattern != tc.pattern {
			t.Errorf("labelPatternClause(%q) = (%q, %q), want (%q, %q)", tc.glob, clause, pattern, tc.clause, tc.pattern)
		}
	}
}

// TestLabelPatternClauseOldOrBareEqual pins the invariant behind the bare
// form (bd-q6rw5, review bd-kx6uj): every glob either takes today's clause,
// LIKE ? ESCAPE '|' bound to globToLikePattern(glob), or the bare LIKE ?
// bound to a pattern byte-equal to globToLikePattern(glob), so the two forms
// differ only by the ESCAPE clause; and a bare pattern holds no | or \.
// Globs are seeded random byte strings, valid and invalid UTF-8, built from
// the tokens that stress the boundary:
// the glob wildcards, every LIKE metacharacter and the escape, NUL, U+FFFD,
// a lone surrogate half's bytes and a truncated multi-byte rune.
//
// Bounds: labelPatternInvariantSeeds x labelPatternInvariantPerSeed globs
// (4 x 5000), each of 0 to 5 tokens.
func TestLabelPatternClauseOldOrBareEqual(t *testing.T) {
	t.Parallel()

	tokens := []string{
		"*", "?", "|", "%", "_", `\`, "\x00", "�",
		"\xed\xa0\x80", // the UTF-8 form of the lone surrogate U+D800
		"\xe2\x82",     // U+20AC (€) cut after two of its three bytes
		"\xff", "\xc0\x80", "\x80",
		"a", "Z", ":", " ", "class", "tag-", "é", "\U0010FFFF",
	}
	const old = "id IN (SELECT issue_id FROM labels WHERE label LIKE ? ESCAPE '|')"
	const bare = "id IN (SELECT issue_id FROM labels WHERE label LIKE ?)"
	for _, seed := range labelPatternInvariantSeeds {
		r := rand.New(rand.NewSource(seed))
		for i := 0; i < labelPatternInvariantPerSeed; i++ {
			var b strings.Builder
			for k := r.Intn(6); k > 0; k-- {
				b.WriteString(tokens[r.Intn(len(tokens))])
			}
			glob := b.String()
			want := globToLikePattern(glob)
			clause, pattern := labelPatternClause("labels", glob)
			switch clause {
			case old:
				if pattern != want {
					t.Fatalf("seed %d glob %q (% x): old clause binds %q, want globToLikePattern %q", seed, glob, glob, pattern, want)
				}
			case bare:
				if pattern != want {
					t.Fatalf("seed %d glob %q (% x): bare clause binds %q (% x), want globToLikePattern %q (% x)", seed, glob, glob, pattern, pattern, want, want)
				}
				if strings.ContainsAny(pattern, `|\`) {
					t.Fatalf("seed %d glob %q: bare clause binds %q, which holds | or \\", seed, glob, pattern)
				}
			default:
				t.Fatalf("seed %d glob %q: clause %q is neither form", seed, glob, clause)
			}
		}
	}
}

var labelPatternInvariantSeeds = []int64{1, 2, 3, 4}

const labelPatternInvariantPerSeed = 5000
