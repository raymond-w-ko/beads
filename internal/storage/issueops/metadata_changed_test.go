package issueops

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// TestMetadataChangedComparesNumbersExactly pins the no-op detector every
// route uses (review bd-ldmiy finding 3): two integer literals inside the
// 64-bit range compare as integers, so the 2^53+1 edit a float64 round trip
// dropped is a change; an integer literal against another spelling is equal
// only when that spelling's float64 is exactly the integer; every other pair
// is the old float64 answer, so 0.1 and 0.10000000000000001, which the server
// stores alike, are no change.
func TestMetadataChangedComparesNumbersExactly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		current, next string
		want          bool
	}{
		{`{"n":1}`, `{"n":1.0}`, false},
		{`{"n":1000}`, `{"n":1e3}`, false},
		{`{"n":1000}`, `{"n":1E3}`, false},
		{`{"n":-150}`, `{"n":-1.50e2}`, false},
		{`{"n":0}`, `{"n":0.0e5}`, false},
		{`{"n":-0}`, `{"n":-0.0}`, false},
		{`{"n":0}`, `{"n":-0}`, true},
		{`{"n":0}`, `{"n":-0.0}`, true},
		{`{"n":0.1}`, `{"n":0.10000000000000001}`, false},
		{`{"n":0.1}`, `{"n":1e-1}`, false},
		{`{"jira.sprint":0.1}`, `{"jira.sprint":0.1000000000000000055511151231257827}`, false},
		{`{"n":0.1}`, `{"n":0.2}`, true},
		{`{"a":{"b":9007199254740992}}`, `{"a":{"b":9007199254740993}}`, true},
		{`{"a":[9007199254740992]}`, `{"a":[9007199254740993]}`, true},
		{`{"n":9007199254740993}`, `{"n":9007199254740993.0}`, true},
		{`{"n":9007199254740992}`, `{"n":9007199254740993.0}`, false},
		{`{"n":9007199254740992}`, `{"n":9.007199254740992e15}`, false},
		{`{"n":9223372036854775807}`, `{"n":9223372036854775806}`, true},
		{`{"n":9223372036854775808}`, `{"n":9223372036854775809}`, true},
		{`{"n":-9223372036854775808}`, `{"n":-9223372036854775807}`, true},
		{`{"n":18446744073709551615}`, `{"n":18446744073709551614}`, true},
		{`{"n":18446744073709551615}`, `{"n":18446744073709551616}`, true},
		{`{"n":18446744073709551616}`, `{"n":18446744073709551617}`, false},
		{`{"n":18446744073709551616}`, `{"n":1.8446744073709552e19}`, false},
		// Dolt prints a stored float64 by its shortest digits: 2^63 stored from
		// 9.223372036854775807e18 reads back as 9223372036854776000, and the
		// same spelling set again is no change (found by the live oracle on
		// Dolt 2.6.2). Another integer literal still is one.
		{`{"n":9223372036854776000}`, `{"n":9.223372036854775807e18}`, false},
		{`{"n":9223372036854776000}`, `{"n":9223372036854775808.0}`, false},
		{`{"n":9223372036854776000}`, `{"n":9223372036854775808}`, true},
		{`{"n":1152921504606847000}`, `{"n":1152921504606846976.0}`, false},
		{`{"n":1152921504606847001}`, `{"n":1152921504606846976.0}`, true},
		{`{"n":1e300}`, `{"n":1.0e300}`, false},
		{`{"n":1}`, `{"n":"1"}`, true},
		{`{"n":"1e0"}`, `{"n":1}`, true},
		{``, `{}`, false},
		{`{"a":1}`, `{"a":1,"b":null}`, true},
		{`{"a":1,"b":2}`, `{"b":2,"a":1.0}`, false},
	}
	for _, tc := range tests {
		got, err := metadataChanged(json.RawMessage(tc.current), json.RawMessage(tc.next))
		if err != nil {
			t.Fatalf("metadataChanged(%s, %s) error = %v", tc.current, tc.next, err)
		}
		if got != tc.want {
			t.Errorf("metadataChanged(%s, %s) = %v, want %v", tc.current, tc.next, got, tc.want)
		}
	}
	if _, err := metadataChanged(json.RawMessage(`{"n":1e400}`), json.RawMessage(`{"n":1}`)); err == nil {
		t.Error("metadataChanged read 1e400, want the float64 overflow error the old form gave")
	}
}

// TestApplyMetadataPatchLegacySetRoundsAsTheServer is review bd-ldmiy's
// finding 3 on the legacy flag: with {"jira.sprint":0.1} stored,
// --set-metadata jira.sprint=0.10000000000000001 (the flag's own parser,
// storage.MetadataEditValue) is no change, as at HEAD: the server stores both
// spellings as 0.1. A real change of the same key still is one.
func TestApplyMetadataPatchLegacySetRoundsAsTheServer(t *testing.T) {
	t.Parallel()
	current := json.RawMessage(`{"jira.sprint":0.1}`)
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"0.10000000000000001", false},
		{"1e-1", false},
		{"0.1", false},
		{"0.2", true},
		{"0.1x", true},
	} {
		_, changed, err := ApplyMetadataPatch(current, publicops.MetadataPatch{Set: map[string]json.RawMessage{
			"jira.sprint": storage.MetadataEditValue(tc.value),
		}})
		if err != nil {
			t.Fatalf("--set-metadata jira.sprint=%s: %v", tc.value, err)
		}
		if changed != tc.want {
			t.Errorf("--set-metadata jira.sprint=%s over %s: changed = %v, want %v", tc.value, current, changed, tc.want)
		}
	}
}

// TestDiscardNoopIssueUpdatesComparesMetadataAsApplyMetadataPatch is review
// bd-ldmiy finding 2 at its seam: the unit-of-work route resolves a merge
// patch to a concrete metadata value and DiscardNoopIssueUpdates decides
// whether to write it. It now gives ApplyMetadataPatch's answer, so a
// respelled document ({"n":1.0} over {"n":1}, 1e3 over 1000, members in
// another order) is dropped and an integer edit past 2^53 is kept.
func TestDiscardNoopIssueUpdatesComparesMetadataAsApplyMetadataPatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		stored, resolved string
		keep             bool
	}{
		{`{"n":1}`, `{"n":1.0}`, false},
		{`{"n": 1000}`, `{"n":1e3}`, false},
		{`{"b": 2, "a": "x"}`, `{"a":"x","b":2}`, false},
		{`{"jira.sprint":0.1}`, `{"jira.sprint":0.10000000000000001}`, false},
		{``, `{}`, false},
		{`{"id":9007199254740992}`, `{"id":9007199254740993}`, true},
		{`{"n":1}`, `{"n":2}`, true},
	} {
		issue := &types.Issue{Metadata: json.RawMessage(tc.stored)}
		filtered, err := DiscardNoopIssueUpdates(issue, map[string]interface{}{"metadata": json.RawMessage(tc.resolved)})
		if err != nil {
			t.Fatalf("DiscardNoopIssueUpdates(%s over %s): %v", tc.resolved, tc.stored, err)
		}
		_, kept := filtered["metadata"]
		if kept != tc.keep {
			t.Errorf("DiscardNoopIssueUpdates(%s over %s) kept metadata = %v, want %v", tc.resolved, tc.stored, kept, tc.keep)
		}
		_, changed, err := ApplyMetadataPatch(json.RawMessage(tc.stored), publicops.MetadataPatch{Replace: publicops.Field[json.RawMessage]{Set: true, Value: json.RawMessage(tc.resolved)}})
		if err != nil {
			t.Fatalf("ApplyMetadataPatch(%s over %s): %v", tc.resolved, tc.stored, err)
		}
		if changed != kept {
			t.Errorf("%s over %s: ApplyMetadataPatch changed = %v, DiscardNoopIssueUpdates kept = %v; want one answer", tc.resolved, tc.stored, changed, kept)
		}
	}
}

// TestApplyMetadataPatchMergePatchInOrder pins that the patches of one request
// apply in order to the current document and keep the members they skip.
func TestApplyMetadataPatchMergePatchInOrder(t *testing.T) {
	t.Parallel()
	current := json.RawMessage(`{"everturn":{"landing":{"old":1},"id":9007199254740993},"x":1}`)
	next, changed, err := ApplyMetadataPatch(current, publicops.MetadataPatch{MergePatch: []json.RawMessage{
		json.RawMessage(`{"everturn":{"landing":null}}`),
		json.RawMessage(`{"everturn":{"landing":{"new":2}}}`),
	}})
	if err != nil {
		t.Fatalf("ApplyMetadataPatch() error = %v", err)
	}
	const want = `{"everturn":{"id":9007199254740993,"landing":{"new":2}},"x":1}`
	if !changed || string(next) != want {
		t.Fatalf("ApplyMetadataPatch() = %s, %v; want %s, true", next, changed, want)
	}
	if _, _, err := ApplyMetadataPatch(current, publicops.MetadataPatch{
		MergePatch: []json.RawMessage{json.RawMessage(`{"a":1}`)},
		Unset:      []string{"x"},
	}); err == nil {
		t.Fatal("ApplyMetadataPatch() accepted a merge patch beside Unset")
	}
}

// TestJSONNumbersEqualStaysLinear pins the bound on the comparison: a number
// is read by strconv in one pass, never by expanding its exponent into
// digits, so a huge exponent, a 5,000-digit integer or a 100,000-digit
// fraction costs time linear in its text, and a number a float64 cannot hold
// is the error the old float64 form gave.
func TestJSONNumbersEqualStaysLinear(t *testing.T) {
	t.Parallel()
	long := "9" + strings.Repeat("0", 4999)
	for _, overflow := range []string{"1e999999999", "1e400", long, "-" + long, long + ".5", "1e99999999999999999999"} {
		start := time.Now()
		if _, err := metadataChanged(json.RawMessage(`{"n":`+overflow+`}`), json.RawMessage(`{"n":1}`)); err == nil {
			t.Errorf("metadataChanged(%.40s) read it, want the float64 overflow error", overflow)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("metadataChanged(%.40s) took %v", overflow, elapsed)
		}
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1e-999999999", "0", true},
		{"1e-999999999", "-0", false},
		{"0." + strings.Repeat("1", 5000), "0.1111111111111111", true},
		{"0." + strings.Repeat("1", 100000), "0.11111111111111111", true},
		{"1" + strings.Repeat("0", 20) + "e-20", "1", true},
		{"1e300", "1" + strings.Repeat("0", 300), true},
		{"1e300", "1000", false},
	} {
		start := time.Now()
		if got := jsonNumbersEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("jsonNumbersEqual(%.40s, %.40s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("jsonNumbersEqual(%.40s, %.40s) took %v", tc.a, tc.b, elapsed)
		}
	}
}

// metadataChangedFloat64 is metadataChanged as it was before exact numbers:
// both documents decoded through any, so every number through a float64. It is
// the comparison test's reference.
func metadataChangedFloat64(current, next json.RawMessage) (bool, error) {
	canonical := func(raw json.RawMessage) (string, error) {
		if len(raw) == 0 || string(raw) == "null" {
			return "{}", nil
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(value)
		return string(encoded), err
	}
	left, err := canonical(current)
	if err != nil {
		return false, err
	}
	right, err := canonical(next)
	if err != nil {
		return false, err
	}
	return left != right, nil
}

// floatSpellings returns spellings of v, a float64, that a float64 reads back
// as v: the shortest decimal and exponent forms, 17 and 21 significant digits
// (0.10000000000000001, 1.00000000000000005551e-01), an upper-case exponent,
// and for an integral v its integer digits, with ".0" and as an "e+0" form.
func floatSpellings(v float64) []string {
	if v == 0 {
		if math.Signbit(v) {
			return []string{"-0", "-0.0", "-0e3", "-0.000E-2"}
		}
		return []string{"0", "0.0", "0e3", "0.000E-2"}
	}
	spellings := []string{
		strconv.FormatFloat(v, 'g', -1, 64),
		strconv.FormatFloat(v, 'e', -1, 64),
		strconv.FormatFloat(v, 'e', 16, 64),
		strconv.FormatFloat(v, 'e', 20, 64),
		strings.ToUpper(strconv.FormatFloat(v, 'e', -1, 64)),
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e25 {
		digits := strconv.FormatFloat(v, 'f', 0, 64) // exact digits, not the shortest
		spellings = append(spellings, digits, digits+".0", digits+"e+0")
	}
	return spellings
}

// comparisonNumbers are the values the comparison test spells: integers
// within 2^53, integers past it that a float64 holds exactly (2^53, 2^60,
// 2^63, -2^63, 2^64, 3*2^62), fractions, values written with an exponent,
// values past the 64-bit range, and both zeros.
func comparisonNumber(rng *rand.Rand) float64 {
	switch rng.Intn(6) {
	case 0:
		return float64(rng.Int63n(1<<53+1) - rng.Int63n(1<<53+1))
	case 1:
		return float64(rng.Intn(2001) - 1000)
	case 2:
		return []float64{0.1, 0.2, 0.3, 1.0 / 3, -2.5, 0.125, 1e-7, 123.456}[rng.Intn(8)]
	case 3:
		return []float64{1 << 53, 1 << 60, 1 << 63, -(1 << 63), 1 << 64, 3 << 62, -(1 << 62)}[rng.Intn(7)]
	case 4:
		return []float64{1e3, 1e6, 1e21, 1e300, 1.5e19, -1e19}[rng.Intn(6)]
	default:
		return []float64{0, math.Copysign(0, -1), 1, -1}[rng.Intn(4)]
	}
}

// metadataChangedDoc draws a metadata document whose numbers come from
// numbers: nested objects and arrays, strings, booleans and nulls.
func metadataChangedDoc(rng *rand.Rand, depth int, numbers func() string) string {
	value := func() string {
		switch k := rng.Intn(10); {
		case k < 3:
			return numbers()
		case k < 4:
			return `"s` + strconv.Itoa(rng.Intn(3)) + `"`
		case k < 5:
			return []string{"true", "false", "null"}[rng.Intn(3)]
		case k < 7 && depth > 0:
			n := rng.Intn(3)
			items := make([]string, n)
			for i := range items {
				items[i] = numbers()
			}
			return "[" + strings.Join(items, ",") + "]"
		case depth > 0:
			return metadataChangedDoc(rng, depth-1, numbers)
		default:
			return numbers()
		}
	}
	n := rng.Intn(4)
	members := make([]string, n)
	for i := range members {
		members[i] = fmt.Sprintf("%q:%s", string(rune('a'+i)), value())
	}
	return "{" + strings.Join(members, ",") + "}"
}

// TestMetadataChangedMatchesFloat64Form is a comparison test (review bd-ldmiy
// finding 3): the piece under test is metadataChanged; the reference is the
// old float64 form, metadataChangedFloat64, which the legacy flags answered
// to. For each of seeds 1 to metadataChangedSeedCount, a pair of documents
// from one seeded generator: the second is either a respelling of the first
// (every number spelled anew from the same float64) or an independent draw.
// Numbers take any spelling (integer digits, ".0", exponents in either case,
// 17 and 21 significant digits, so fractions that share a float64 meet), but
// every value is one a float64 holds, so two different integer literals past
// 2^53 never meet in one place. Relation: the two answer alike on every pair.
// The cases where they differ, all integers a float64 cannot hold, are listed
// after as fixed cases. A failure prints its seed.
func TestMetadataChangedMatchesFloat64Form(t *testing.T) {
	t.Parallel()
	const metadataChangedSeedCount = 20000
	start := time.Now()
	same, changed := 0, 0
	for seed := int64(1); seed <= metadataChangedSeedCount; seed++ {
		rng := rand.New(rand.NewSource(seed))
		spell := func(v float64) string {
			spellings := floatSpellings(v)
			return spellings[rng.Intn(len(spellings))]
		}
		var current, next string
		if rng.Intn(2) == 0 {
			var values []float64
			draw := func() string {
				v := comparisonNumber(rng)
				values = append(values, v)
				return spell(v)
			}
			shape := rng.Int63()
			current = metadataChangedDoc(rand.New(rand.NewSource(shape)), 3, draw)
			replay := 0
			respell := func() string {
				v := values[replay]
				replay++
				return spell(v)
			}
			next = metadataChangedDoc(rand.New(rand.NewSource(shape)), 3, respell)
		} else {
			draw := func() string { return spell(comparisonNumber(rng)) }
			current = metadataChangedDoc(rng, 3, draw)
			next = metadataChangedDoc(rng, 3, draw)
		}
		got, err := metadataChanged(json.RawMessage(current), json.RawMessage(next))
		if err != nil {
			t.Fatalf("seed %d: metadataChanged(%s, %s): %v", seed, current, next, err)
		}
		want, err := metadataChangedFloat64(json.RawMessage(current), json.RawMessage(next))
		if err != nil {
			t.Fatalf("seed %d: float64 form(%s, %s): %v", seed, current, next, err)
		}
		if got != want {
			t.Fatalf("seed %d: metadataChanged(%s, %s) = %v, the float64 form %v", seed, current, next, got, want)
		}
		if got {
			changed++
		} else {
			same++
		}
	}
	t.Logf("%d seeds (1..%d): %d unchanged and %d changed pairs answer as the float64 form, in %v",
		metadataChangedSeedCount, metadataChangedSeedCount, same, changed, time.Since(start).Round(time.Millisecond))

	// Where the two differ: an integer literal inside the 64-bit range that a
	// float64 cannot hold, against another integer literal or against a
	// spelling whose float64 is not exactly it. The server keeps such an
	// integer's digits (Dolt 2.6), so the new form writes the edit the float64
	// form dropped.
	for _, tc := range []struct {
		current, next string
	}{
		{`{"n":9007199254740992}`, `{"n":9007199254740993}`},
		{`{"a":{"b":9007199254740995}}`, `{"a":{"b":9007199254740996}}`},
		{`{"a":[-9007199254740993]}`, `{"a":[-9007199254740992]}`},
		{`{"n":1152921504606846976}`, `{"n":1152921504606846977}`},
		{`{"n":9223372036854775808}`, `{"n":9223372036854775809}`},
		{`{"n":12345678901234567890}`, `{"n":12345678901234567891}`},
		{`{"n":9007199254740993}`, `{"n":9007199254740993.0}`},
		{`{"n":18446744073709551615}`, `{"n":18446744073709551616}`},
	} {
		got, err := metadataChanged(json.RawMessage(tc.current), json.RawMessage(tc.next))
		if err != nil || !got {
			t.Errorf("metadataChanged(%s, %s) = %v, %v; want a change", tc.current, tc.next, got, err)
		}
		if old, err := metadataChangedFloat64(json.RawMessage(tc.current), json.RawMessage(tc.next)); err != nil || old {
			t.Errorf("float64 form(%s, %s) = %v, %v; want it to call them equal", tc.current, tc.next, old, err)
		}
	}
	// Both forms fail on a number a float64 cannot hold, as HEAD did.
	for _, doc := range []string{`{"n":1e400}`, `{"n":-1e400}`} {
		if _, err := metadataChangedFloat64(json.RawMessage(doc), json.RawMessage(`{}`)); err == nil {
			t.Errorf("the float64 form read %s, want its overflow error", doc)
		}
		if _, err := metadataChanged(json.RawMessage(doc), json.RawMessage(`{}`)); err == nil {
			t.Errorf("metadataChanged read %s, want the same overflow error", doc)
		}
	}
}
