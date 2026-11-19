package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestMergePatchMetadataJSONRFC7396AppendixA runs the fifteen examples of RFC
// 7396 Appendix A. The four whose patch is not an object (an array, null, a
// string) are refusals here: metadata is an object, so a whole-document
// replacement is --metadata's job, not a merge patch's.
func TestMergePatchMetadataJSONRFC7396AppendixA(t *testing.T) {
	t.Parallel()
	tests := []struct {
		original string
		patch    string
		want     string // empty: the patch is refused
	}{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{`["a","b"]`, `["c","d"]`, ``},
		{`{"a":"b"}`, `["c"]`, ``},
		{`{"a":"foo"}`, `null`, ``},
		{`{"a":"foo"}`, `"bar"`, ``},
		{`{"e":null}`, `{"a":1}`, `{"a":1,"e":null}`},
		{`[1,2]`, `{"a":"b","c":null}`, `{"a":"b"}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.original+" + "+tc.patch, func(t *testing.T) {
			got, err := MergePatchMetadataJSON(json.RawMessage(tc.original), json.RawMessage(tc.patch))
			if tc.want == "" {
				if err == nil {
					t.Fatalf("MergePatchMetadataJSON() = %s, want a refusal", got)
				}
				if ValidateMetadataMergePatch(json.RawMessage(tc.patch)) == nil {
					t.Fatal("ValidateMetadataMergePatch() accepted a patch the merge refuses")
				}
				return
			}
			if err != nil {
				t.Fatalf("MergePatchMetadataJSON() error = %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("MergePatchMetadataJSON() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestMergePatchMetadataJSONKeepsUnnamedValueText pins that a member the
// patch does not name keeps its text at any depth, an int64 past 2^53 and a
// float spelled 1.0 included, because nothing is decoded through a float64.
func TestMergePatchMetadataJSONKeepsUnnamedValueText(t *testing.T) {
	t.Parallel()
	existing := json.RawMessage(`{"everturn":{"id":9007199254740993,"f":1.0,"e":1e3,"landing":{"at":"x"}},"top":9007199254740993}`)
	got, err := MergePatchMetadataJSON(existing, json.RawMessage(`{"everturn":{"worktree":{"path":"wt/a","n":9007199254740995}}}`))
	if err != nil {
		t.Fatalf("MergePatchMetadataJSON() error = %v", err)
	}
	want := `{"everturn":{"e":1e3,"f":1.0,"id":9007199254740993,"landing":{"at":"x"},"worktree":{"n":9007199254740995,"path":"wt/a"}},"top":9007199254740993}`
	if string(got) != want {
		t.Fatalf("MergePatchMetadataJSON() = %s, want %s", got, want)
	}
}

// TestMergePatchMetadataJSONNamesAreLiteral pins that a dotted or slashed name
// is one member at every depth: there is no path grammar.
func TestMergePatchMetadataJSONNamesAreLiteral(t *testing.T) {
	t.Parallel()
	existing := json.RawMessage(`{"jira.sprint":"old","jira":{"sprint":"nested"}}`)
	got, err := MergePatchMetadataJSON(existing, json.RawMessage(`{"jira.sprint":"new","jira":{"a/b":{"c.d":1}}}`))
	if err != nil {
		t.Fatalf("MergePatchMetadataJSON() error = %v", err)
	}
	want := `{"jira":{"a/b":{"c.d":1},"sprint":"nested"},"jira.sprint":"new"}`
	if string(got) != want {
		t.Fatalf("MergePatchMetadataJSON() = %s, want %s", got, want)
	}
}

// TestMergePatchMetadataJSONReplacesNestedObjectInTwoPatches pins the order
// rule a caller uses to replace a nested object whole: null it, then set it.
func TestMergePatchMetadataJSONReplacesNestedObjectInTwoPatches(t *testing.T) {
	t.Parallel()
	current := json.RawMessage(`{"everturn":{"landing":{"old":1,"keep":2},"worktree":"wt/a"}}`)
	for _, patch := range []string{`{"everturn":{"landing":null}}`, `{"everturn":{"landing":{"new":3}}}`} {
		var err error
		current, err = MergePatchMetadataJSON(current, json.RawMessage(patch))
		if err != nil {
			t.Fatalf("MergePatchMetadataJSON(%s) error = %v", patch, err)
		}
	}
	want := `{"everturn":{"landing":{"new":3},"worktree":"wt/a"}}`
	if string(current) != want {
		t.Fatalf("after two patches = %s, want %s", current, want)
	}
}

// TestValidateMetadataMergePatchRefusals pins what is refused before a write.
// Top-level names follow ValidateMetadataKey, as --set-metadata's do; nested
// names are not checked.
func TestValidateMetadataMergePatchRefusals(t *testing.T) {
	t.Parallel()
	for _, patch := range []string{``, `{`, `null`, `[]`, `"x"`, `1`, `true`, `{"bad key":1}`, `{"1st":1}`, `{"":1}`} {
		if err := ValidateMetadataMergePatch(json.RawMessage(patch)); err == nil {
			t.Errorf("ValidateMetadataMergePatch(%q) = nil, want a refusal", patch)
		}
	}
	for _, patch := range []string{`{}`, `{"a":null}`, `{"jira.sprint":{"bad key":1,"":2}}`} {
		if err := ValidateMetadataMergePatch(json.RawMessage(patch)); err != nil {
			t.Errorf("ValidateMetadataMergePatch(%q) = %v, want nil", patch, err)
		}
	}
	err := ValidateMetadataMergePatch(json.RawMessage(`[1]`))
	if err == nil || !strings.Contains(err.Error(), "an array") {
		t.Fatalf("array refusal = %v, want it named", err)
	}
}

// TestMergePatchMetadataJSONDuplicateNamesLastWins pins the rule for a name
// given twice in one object of a patch: the last member wins and the earlier
// one is ignored, not merged with it, at the top level and below. Every route
// resolves a patch through this function, and the lifecycle contract pins the
// same case on the dolt, uow and embedded stores.
func TestMergePatchMetadataJSONDuplicateNamesLastWins(t *testing.T) {
	t.Parallel()
	existing := json.RawMessage(`{"a":{"k":0,"x":0},"e":{"l":1,"m":2}}`)
	got, err := MergePatchMetadataJSON(existing, json.RawMessage(`{"a":{"x":1},"a":{"y":2},"e":{"l":null,"l":3,"m":4,"m":null}}`))
	if err != nil {
		t.Fatalf("MergePatchMetadataJSON() error = %v", err)
	}
	const want = `{"a":{"k":0,"x":0,"y":2},"e":{"l":3}}`
	if string(got) != want {
		t.Fatalf("MergePatchMetadataJSON() = %s, want %s", got, want)
	}
}

// TestValidateMetadataMergePatchDepthBound pins the depth bound: a patch
// nested more than MaxMetadataMergePatchDepth deep is refused before any
// merge, a 10,000-deep one included, in time linear in its length; one at the
// bound merges. Brackets inside strings do not count.
func TestValidateMetadataMergePatchDepthBound(t *testing.T) {
	t.Parallel()
	nested := func(depth int) json.RawMessage {
		return json.RawMessage(strings.Repeat(`{"a":`, depth-1) + `{}` + strings.Repeat(`}`, depth-1))
	}
	if depth := jsonNestingDepth(json.RawMessage(`{"s":"{{[[\"{{"}`)); depth != 1 {
		t.Fatalf("jsonNestingDepth counted brackets inside a string: %d, want 1", depth)
	}
	if depth := jsonNestingDepth(nested(MaxMetadataMergePatchDepth)); depth != MaxMetadataMergePatchDepth {
		t.Fatalf("jsonNestingDepth(nested(%d)) = %d", MaxMetadataMergePatchDepth, depth)
	}
	atBound := nested(MaxMetadataMergePatchDepth)
	if _, err := MergePatchMetadataJSON(json.RawMessage(`{"b":1}`), atBound); err != nil {
		t.Fatalf("a patch %d deep: %v, want it merged", MaxMetadataMergePatchDepth, err)
	}
	for _, depth := range []int{MaxMetadataMergePatchDepth + 1, 10000} {
		start := time.Now()
		err := ValidateMetadataMergePatch(nested(depth))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("nested %d deep", depth)) {
			t.Fatalf("a patch %d deep: %v, want the depth refusal", depth, err)
		}
		if _, err := MergePatchMetadataJSON(json.RawMessage(`{}`), nested(depth)); err == nil {
			t.Fatalf("MergePatchMetadataJSON merged a patch %d deep", depth)
		}
		t.Logf("%d deep (%d bytes): refused in %v: %v", depth, len(nested(depth)), time.Since(start), err)
	}
	arrays := json.RawMessage(`{"a":` + strings.Repeat(`[`, MaxMetadataMergePatchDepth) + strings.Repeat(`]`, MaxMetadataMergePatchDepth) + `}`)
	if err := ValidateMetadataMergePatch(arrays); err == nil {
		t.Fatal("arrays nested past the bound inside a member were accepted")
	}
}

// rfc7396Reference is RFC 7396 section 2's MergePatch pseudocode over decoded
// values, the comparison test's reference:
//
//	define MergePatch(Target, Patch):
//	  if Patch is an Object:
//	    if Target is not an Object:
//	      Target = {} # Ignore the contents and set it to an empty Object
//	    for each Name/Value pair in Patch:
//	      if Value is null:
//	        if Name exists in Target:
//	          remove the Name/Value pair from Target
//	      else:
//	        Target[Name] = MergePatch(Target[Name], Value)
//	    return Target
//	  else:
//	    return Patch
func rfc7396Reference(target, patch any) any {
	patchObject, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	targetObject, ok := target.(map[string]any)
	if !ok {
		targetObject = map[string]any{}
	}
	for name, value := range patchObject {
		if value == nil {
			delete(targetObject, name)
			continue
		}
		targetObject[name] = rfc7396Reference(targetObject[name], value)
	}
	return targetObject
}

// mergePatchGen draws random JSON values for the comparison test: nested
// objects and arrays, nulls inside arrays, empty objects, strings, booleans
// and numbers exact in a float64, under names that include dotted, slashed,
// empty and multi-byte ones.
type mergePatchGen struct {
	rng *rand.Rand
}

var mergePatchGenNames = []string{"a", "b", "c", "everturn", "landing", "jira.sprint", "a/b", "c.d/e", "", "ключ", "名前", "e\u0301", "x y", "1st", "null"}

func (g mergePatchGen) name(topLevel bool) string {
	for {
		name := mergePatchGenNames[g.rng.Intn(len(mergePatchGenNames))]
		if !topLevel || ValidateMetadataKey(name) == nil {
			return name
		}
	}
}

func (g mergePatchGen) scalar() any {
	switch g.rng.Intn(6) {
	case 0:
		return nil
	case 1:
		return g.rng.Intn(2) == 0
	case 2:
		return float64(g.rng.Intn(2001) - 1000)
	case 3:
		return float64(g.rng.Intn(64)) / 8
	case 4:
		return ""
	default:
		return g.name(false) + "<&>\u2028"
	}
}

// value draws a value at most depth levels of objects and arrays deep; out of
// ten draws, objectBias are objects and two are arrays. In a patch a null
// member is a removal, in a target it is a stored null.
func (g mergePatchGen) value(depth int, objectBias int) any {
	if depth == 0 {
		return g.scalar()
	}
	switch k := g.rng.Intn(10); {
	case k < objectBias:
		return g.object(depth, false)
	case k < objectBias+2:
		n := g.rng.Intn(4)
		array := make([]any, n)
		for i := range array {
			array[i] = g.value(depth-1, 3)
		}
		return array
	default:
		return g.scalar()
	}
}

func (g mergePatchGen) object(depth int, topLevel bool) map[string]any {
	n := g.rng.Intn(5)
	object := make(map[string]any, n)
	for i := 0; i < n; i++ {
		object[g.name(topLevel)] = g.value(depth-1, 6)
	}
	return object
}

// TestMergePatchMetadataJSONMatchesRFCReference is a comparison test: the
// piece under test is MergePatchMetadataJSON on raw JSON text; the reference
// is rfc7396Reference over decoded values. For each of mergePatchSeedCount
// seeds, a random target document (an object, or now and then an array, a
// scalar or null, which the RFC treats as {}) and a random object patch;
// relation: the two results are equal once both are decoded. Numbers are kept
// exact in a float64, so decoding the reference loses nothing. A failure
// prints its seed and both inputs.
func TestMergePatchMetadataJSONMatchesRFCReference(t *testing.T) {
	t.Parallel()
	const mergePatchSeedCount = 5000
	start := time.Now()
	objects, removals := 0, 0
	for seed := int64(1); seed <= mergePatchSeedCount; seed++ {
		g := mergePatchGen{rng: rand.New(rand.NewSource(seed))}
		var target any
		switch g.rng.Intn(10) {
		case 0:
			target = []any{g.scalar(), nil}
		case 1:
			target = g.scalar()
		default:
			target = g.object(4, false)
			objects++
		}
		patch := g.object(4, true)
		for name := range patch {
			if patch[name] == nil {
				removals++
			}
		}
		targetText, err := json.Marshal(target)
		if err != nil {
			t.Fatalf("seed %d: marshal target: %v", seed, err)
		}
		patchText, err := json.Marshal(patch)
		if err != nil {
			t.Fatalf("seed %d: marshal patch: %v", seed, err)
		}
		got, err := MergePatchMetadataJSON(targetText, patchText)
		if err != nil {
			t.Fatalf("seed %d: MergePatchMetadataJSON(%s, %s): %v", seed, targetText, patchText, err)
		}
		var gotValue any
		if err := json.Unmarshal(got, &gotValue); err != nil {
			t.Fatalf("seed %d: result %s does not decode: %v", seed, got, err)
		}
		var targetCopy, patchCopy any
		_ = json.Unmarshal(targetText, &targetCopy)
		_ = json.Unmarshal(patchText, &patchCopy)
		wantText, _ := json.Marshal(rfc7396Reference(targetCopy, patchCopy))
		var wantValue any
		_ = json.Unmarshal(wantText, &wantValue)
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("seed %d: MergePatchMetadataJSON(%s, %s) = %s, reference %s", seed, targetText, patchText, got, wantText)
		}
	}
	t.Logf("%d seeds (1..%d; %d object targets, %d top-level removals) agree with the RFC 7396 reference in %v",
		mergePatchSeedCount, mergePatchSeedCount, objects, removals, time.Since(start).Round(time.Millisecond))
}

// TestValidateMetadataMergePatchRefusesTextTheStoreWouldNotKeep pins review
// bd-ldmiy finding 1: a patch whose text is not UTF-8, or holds a \u escape of
// a lone surrogate, in a name or in a string value, is refused with
// ErrMetadataMergePatchText, because encoding/json and the store would both
// read it as U+FFFD. The e9 case is the reviewer's: the raw byte as a member
// name removed an existing U+FFFD member. A literal U+FFFD, an escaped
// surrogate pair and an escaped backslash before "ud800" stay accepted.
func TestValidateMetadataMergePatchRefusesTextTheStoreWouldNotKeep(t *testing.T) {
	t.Parallel()
	e9 := string([]byte{0xe9})
	for _, patch := range []string{
		`{"everturn":{"` + e9 + `":null}}`,
		`{"everturn":{"x":"` + e9 + `"}}`,
		`{"x":"ok","y":["` + string([]byte{0xff, 0xfe}) + `"]}`,
		`{"x":"` + string([]byte{0xed, 0xa0, 0x80}) + `"}`,
		`{"everturn":{"\ud800":null}}`,
		`{"everturn":{"\uD800":null}}`,
		`{"x":"\udc00"}`,
		`{"x":"a\ud800"}`,
		`{"x":"\ud800A"}`,
		`{"x":"\ud800\n"}`,
		`{"x":"\ud83d\ud83d"}`,
		`{"x":"\ude00\ud83d"}`,
		`{"\udbff":1}`,
	} {
		err := ValidateMetadataMergePatch(json.RawMessage(patch))
		if !errors.Is(err, ErrMetadataMergePatchText) {
			t.Errorf("ValidateMetadataMergePatch(%q) = %v, want ErrMetadataMergePatchText", patch, err)
		}
		if _, err := MergePatchMetadataJSON(json.RawMessage(`{"everturn":{"`+"�"+`":"keep"}}`), json.RawMessage(patch)); !errors.Is(err, ErrMetadataMergePatchText) {
			t.Errorf("MergePatchMetadataJSON(%q) = %v, want ErrMetadataMergePatchText", patch, err)
		}
	}
	if got := ErrMetadataMergePatchText.Error(); got != `metadata merge patch is not valid UTF-8 text: it holds an invalid byte or an unpaired \u surrogate escape` {
		t.Errorf("message = %q, want the fixed text", got)
	}
	for _, tc := range []struct{ patch, want string }{
		{`{"everturn":{"` + "�" + `":null}}`, `{"everturn":{"other":1}}`},
		{`{"everturn":{"�":null}}`, `{"everturn":{"other":1}}`},
		{`{"x":"😀"}`, `{"everturn":{"other":1,"` + "�" + `":"keep"},"x":"😀"}`},
		{`{"x":"😀 é \\ud800"}`, `{"everturn":{"other":1,"` + "�" + `":"keep"},"x":"😀 é \\ud800"}`},
		{`{"x":"é😀"}`, `{"everturn":{"other":1,"` + "�" + `":"keep"},"x":"é😀"}`},
	} {
		got, err := MergePatchMetadataJSON(json.RawMessage(`{"everturn":{"other":1,"`+"�"+`":"keep"}}`), json.RawMessage(tc.patch))
		if err != nil {
			t.Errorf("MergePatchMetadataJSON(%q) error = %v, want it accepted", tc.patch, err)
			continue
		}
		var gotValue, wantValue any
		if err := json.Unmarshal(got, &gotValue); err != nil {
			t.Fatalf("decode %s: %v", got, err)
		}
		if err := json.Unmarshal([]byte(tc.want), &wantValue); err != nil {
			t.Fatalf("decode %s: %v", tc.want, err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("MergePatchMetadataJSON(%q) = %s, want %s", tc.patch, got, tc.want)
		}
	}
}

// TestJSONTextKeepsItsCharactersIsLinear bounds the text check: a patch of
// 200,000 escaped surrogate pairs (1.2 MB) is read in one pass.
func TestJSONTextKeepsItsCharactersIsLinear(t *testing.T) {
	t.Parallel()
	patch := json.RawMessage(`{"x":"` + strings.Repeat(`😀`, 200000) + `"}`)
	start := time.Now()
	if !jsonTextKeepsItsCharacters(patch) {
		t.Fatal("refused 200,000 escaped surrogate pairs")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("checked %d bytes in %v", len(patch), elapsed)
	}
	if jsonTextKeepsItsCharacters(json.RawMessage(string(patch[:len(patch)-2]) + `\ud83d"}`)) {
		t.Error("accepted a trailing lone high surrogate")
	}
}
