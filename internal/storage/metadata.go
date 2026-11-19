// Package storage defines the interface for issue storage backends.
package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// NormalizeMetadataValue converts metadata values to a validated JSON string.
// Accepts string, []byte, or json.RawMessage and returns a validated JSON string.
// Returns an error if the value is not valid JSON or is an unsupported type.
//
// This supports GH#1417: allow UpdateIssue metadata updates via json.RawMessage/[]byte.
func NormalizeMetadataValue(value interface{}) (string, error) {
	var jsonStr string

	switch v := value.(type) {
	case string:
		jsonStr = v
	case []byte:
		jsonStr = string(v)
	case json.RawMessage:
		jsonStr = string(v)
	default:
		return "", fmt.Errorf("metadata must be string, []byte, or json.RawMessage, got %T", value)
	}

	// Validate that it's valid JSON
	if !json.Valid([]byte(jsonStr)) {
		return "", fmt.Errorf("metadata is not valid JSON")
	}

	return jsonStr, nil
}

// MetadataFieldType defines the type of a metadata field for schema validation.
type MetadataFieldType string

const (
	MetadataFieldString MetadataFieldType = "string"
	MetadataFieldInt    MetadataFieldType = "int"
	MetadataFieldFloat  MetadataFieldType = "float"
	MetadataFieldBool   MetadataFieldType = "bool"
	MetadataFieldEnum   MetadataFieldType = "enum"
)

// MetadataFieldSchema defines validation rules for a single metadata field.
type MetadataFieldSchema struct {
	Type     MetadataFieldType
	Values   []string // allowed values for enum type
	Required bool
	Min      *float64 // min value for int/float
	Max      *float64 // max value for int/float
}

// MetadataSchemaConfig holds the parsed metadata validation configuration.
type MetadataSchemaConfig struct {
	Mode   string                         // "none", "warn", "error"
	Fields map[string]MetadataFieldSchema // field name → schema
}

// MetadataValidationError describes a single schema violation.
type MetadataValidationError struct {
	Field   string
	Message string
}

func (e MetadataValidationError) Error() string {
	return fmt.Sprintf("metadata.%s: %s", e.Field, e.Message)
}

// ValidateMetadataSchema validates a metadata blob against a schema config.
// Returns a list of validation errors. An empty list means validation passed.
// If metadata is nil/empty and no fields are required, returns no errors.
func ValidateMetadataSchema(metadata json.RawMessage, schema MetadataSchemaConfig) []MetadataValidationError {
	if len(schema.Fields) == 0 {
		return nil
	}

	// Parse metadata into a map
	var parsed map[string]interface{}
	if len(metadata) == 0 || string(metadata) == "{}" || string(metadata) == "null" {
		parsed = map[string]interface{}{}
	} else {
		if err := json.Unmarshal(metadata, &parsed); err != nil {
			// Not a JSON object — can't validate fields
			return []MetadataValidationError{{Field: "(root)", Message: "metadata must be a JSON object for schema validation"}}
		}
	}

	var errs []MetadataValidationError

	for fieldName, fieldSchema := range schema.Fields {
		val, exists := parsed[fieldName]

		// Check required
		if fieldSchema.Required && !exists {
			errs = append(errs, MetadataValidationError{
				Field:   fieldName,
				Message: "required field is missing",
			})
			continue
		}

		if !exists {
			continue
		}

		// Type-check the value
		switch fieldSchema.Type {
		case MetadataFieldString:
			if _, ok := val.(string); !ok {
				errs = append(errs, MetadataValidationError{
					Field:   fieldName,
					Message: fmt.Sprintf("expected string, got %T", val),
				})
			}

		case MetadataFieldInt:
			num, ok := val.(float64)
			if !ok {
				errs = append(errs, MetadataValidationError{
					Field:   fieldName,
					Message: fmt.Sprintf("expected int, got %T", val),
				})
			} else {
				if num != float64(int64(num)) {
					errs = append(errs, MetadataValidationError{
						Field:   fieldName,
						Message: fmt.Sprintf("expected int, got float %v", num),
					})
				} else {
					if fieldSchema.Min != nil && num < *fieldSchema.Min {
						errs = append(errs, MetadataValidationError{
							Field:   fieldName,
							Message: fmt.Sprintf("value %v is below minimum %v", num, *fieldSchema.Min),
						})
					}
					if fieldSchema.Max != nil && num > *fieldSchema.Max {
						errs = append(errs, MetadataValidationError{
							Field:   fieldName,
							Message: fmt.Sprintf("value %v exceeds maximum %v", num, *fieldSchema.Max),
						})
					}
				}
			}

		case MetadataFieldFloat:
			num, ok := val.(float64)
			if !ok {
				errs = append(errs, MetadataValidationError{
					Field:   fieldName,
					Message: fmt.Sprintf("expected float, got %T", val),
				})
			} else {
				if fieldSchema.Min != nil && num < *fieldSchema.Min {
					errs = append(errs, MetadataValidationError{
						Field:   fieldName,
						Message: fmt.Sprintf("value %v is below minimum %v", num, *fieldSchema.Min),
					})
				}
				if fieldSchema.Max != nil && num > *fieldSchema.Max {
					errs = append(errs, MetadataValidationError{
						Field:   fieldName,
						Message: fmt.Sprintf("value %v exceeds maximum %v", num, *fieldSchema.Max),
					})
				}
			}

		case MetadataFieldBool:
			if _, ok := val.(bool); !ok {
				errs = append(errs, MetadataValidationError{
					Field:   fieldName,
					Message: fmt.Sprintf("expected bool, got %T", val),
				})
			}

		case MetadataFieldEnum:
			str, ok := val.(string)
			if !ok {
				errs = append(errs, MetadataValidationError{
					Field:   fieldName,
					Message: fmt.Sprintf("expected string (enum), got %T", val),
				})
			} else {
				found := false
				for _, allowed := range fieldSchema.Values {
					if str == allowed {
						found = true
						break
					}
				}
				if !found {
					errs = append(errs, MetadataValidationError{
						Field:   fieldName,
						Message: fmt.Sprintf("value %q is not one of: %v", str, fieldSchema.Values),
					})
				}
			}
		}
	}

	return errs
}

// validMetadataKeyRe validates metadata key names for use in JSON path expressions.
// Allows alphanumeric, underscore, dot (dotted keys like "jira.sprint"), and
// slash (path-style keys like "jira/sprint").
var validMetadataKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_./]*$`)

// ValidateMetadataKey checks that a metadata key is safe for use in JSON path
// expressions. Keys must start with a letter or underscore and contain only
// alphanumeric characters, underscores, dots, and slashes.
func ValidateMetadataKey(key string) error {
	if !validMetadataKeyRe.MatchString(key) {
		return fmt.Errorf("invalid metadata key %q: must match %s", key, validMetadataKeyRe.String())
	}
	return nil
}

// JSONMetadataPath returns a MySQL/Dolt JSON path expression for the given
// metadata key. The key is always quoted so that "gc.routed_to" produces
// '$."gc.routed_to"' instead of '$.gc.routed_to' (which dolt interprets as a
// nested path: {gc: {routed_to: ...}}); quoting is valid for plain keys too,
// so no character list needs to stay in sync with validMetadataKeyRe. Slash
// and mixed-case keys are proven to round-trip through the real Dolt/
// go-mysql-server JSON path parser (see TestMetadataFilterSuite's
// MetadataFieldMatchSlashKey and MetadataFieldMatchMixedCaseKey subtests in
// cmd/bd/metadata_filter_test.go).
//
// Backslashes and quotes are also escaped, but every production caller
// (sqlbuild.AppendMetadataClauses and doltTransaction.SearchIssues) validates
// the key with ValidateMetadataKey first, which rejects `"` and `\`, so those escaping
// branches are unreachable in practice today and are exercised only by the
// string-level unit test in metadata_jsonpath_test.go, not against the real
// SQL engines. Treat that escaping as defense-in-depth, not a proven
// contract, unless a caller starts passing unvalidated keys here.
func JSONMetadataPath(key string) string {
	return `$."` + jsonPathEscaper.Replace(key) + `"`
}

var jsonPathEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// MergeMetadataJSON merges incoming metadata JSON into existing metadata.
// Top-level keys from incoming overwrite keys in existing; keys only in
// existing are preserved. Both inputs must be JSON objects (or empty/null).
func MergeMetadataJSON(existing, incoming json.RawMessage) (json.RawMessage, error) {
	base := make(map[string]json.RawMessage)
	if len(existing) > 0 {
		trimmed := strings.TrimSpace(string(existing))
		if trimmed != "" && trimmed != "null" {
			if err := json.Unmarshal(existing, &base); err != nil {
				return nil, fmt.Errorf("existing metadata is not a JSON object: %w", err)
			}
		}
	}

	overlay := make(map[string]json.RawMessage)
	if err := json.Unmarshal(incoming, &overlay); err != nil {
		return nil, fmt.Errorf("new metadata is not a JSON object: %w", err)
	}

	for k, v := range overlay {
		base[k] = v
	}

	result, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal merged metadata: %w", err)
	}
	return json.RawMessage(result), nil
}

// MaxMetadataMergePatchDepth is the deepest nesting of objects and arrays a
// metadata merge patch may have; the patch itself is depth 1. A deeper patch is
// refused before any write. The merge recurses once per object level of the
// patch and re-reads the member text at each, so an unbounded depth would cost
// time quadratic in the patch's length. 100 is MySQL's limit on the depth of a
// JSON document.
const MaxMetadataMergePatchDepth = 100

// ValidateMetadataMergePatch checks that patch can be applied as a metadata
// merge patch: a JSON object, nested at most MaxMetadataMergePatchDepth deep,
// whose top-level member names pass ValidateMetadataKey. Member names below
// the top level are literal and are not checked, and member values are not
// inspected. It is what every caller runs before a write, so a refused patch
// writes nothing.
func ValidateMetadataMergePatch(patch json.RawMessage) error {
	_, err := metadataMergePatchMembers(patch)
	return err
}

// ErrMetadataMergePatchText refuses a merge patch holding text the store would
// not keep as typed: a byte sequence that is not UTF-8, or a \u escape of a
// lone UTF-16 surrogate (a high half not followed by a low half, or a low half
// alone). Both decode to U+FFFD, so such a patch would name, set or remove a
// U+FFFD member it never spelled.
var ErrMetadataMergePatchText = errors.New("metadata merge patch is not valid UTF-8 text: it holds an invalid byte or an unpaired \\u surrogate escape")

func metadataMergePatchMembers(patch json.RawMessage) (map[string]json.RawMessage, error) {
	if !json.Valid(patch) {
		return nil, fmt.Errorf("metadata merge patch is not valid JSON")
	}
	if !jsonTextKeepsItsCharacters(patch) {
		return nil, ErrMetadataMergePatchText
	}
	if depth := jsonNestingDepth(patch); depth > MaxMetadataMergePatchDepth {
		return nil, fmt.Errorf("metadata merge patch is nested %d deep, more than the limit of %d", depth, MaxMetadataMergePatchDepth)
	}
	members, isObject := jsonObjectMembers(patch)
	if !isObject {
		return nil, fmt.Errorf("metadata merge patch must be a JSON object, got %s", jsonKindOf(patch))
	}
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ValidateMetadataKey(key); err != nil {
			return nil, err
		}
	}
	return members, nil
}

// MergePatchMetadataJSON applies patch to existing as an RFC 7396 JSON Merge
// Patch and returns the merged document. An object member of the patch
// recurses into the same-named member of the document, a null member removes
// it, and any other member replaces it; a document member that is absent or is
// not an object counts as {} before an object member is merged into it.
//
// Member names are literal at every depth: there is no path grammar, so a
// dotted or slashed name is one member. Values the patch does not name keep
// their text, since the recursion decodes only into map[string]json.RawMessage
// and only along the patch's own paths; a number is never read through a
// float64. The encoding is MergeMetadataJSON's: members sorted by name, each
// object compacted, and <, >, & and U+2028/U+2029 escaped inside strings.
//
// A name that appears twice in one object of the patch takes its last value,
// as encoding/json decodes it (and as Dolt stores such a document): the
// earlier member is ignored, not merged with the later one.
//
// Existing metadata that is not an object (empty, null, or any other value)
// counts as {}, the RFC's rule for the target, where MergeMetadataJSON would
// refuse it. Only the patch is checked: ValidateMetadataMergePatch.
func MergePatchMetadataJSON(existing, patch json.RawMessage) (json.RawMessage, error) {
	members, err := metadataMergePatchMembers(patch)
	if err != nil {
		return nil, err
	}
	base, isObject := jsonObjectMembers(existing)
	if !isObject {
		base = make(map[string]json.RawMessage)
	}
	merged, err := applyMergePatchMembers(base, members)
	if err != nil {
		return nil, err
	}
	result, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal merged metadata: %w", err)
	}
	return json.RawMessage(result), nil
}

// applyMergePatchMembers is RFC 7396's MergePatch for an object patch, with
// target already resolved to an object (possibly empty). It mutates target.
func applyMergePatchMembers(target, patch map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	for name, value := range patch {
		if isJSONNull(value) {
			delete(target, name)
			continue
		}
		members, isObject := jsonObjectMembers(value)
		if !isObject {
			target[name] = value
			continue
		}
		child, childIsObject := jsonObjectMembers(target[name])
		if !childIsObject {
			child = make(map[string]json.RawMessage)
		}
		merged, err := applyMergePatchMembers(child, members)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal merged metadata member %q: %w", name, err)
		}
		target[name] = encoded
	}
	return target, nil
}

// jsonObjectMembers decodes raw as an object's members. It reports false for
// anything that is not a JSON object, including null and invalid JSON.
func jsonObjectMembers(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	members := make(map[string]json.RawMessage)
	if err := json.Unmarshal(trimmed, &members); err != nil {
		return nil, false
	}
	return members, true
}

// jsonNestingDepth is the deepest nesting of objects and arrays in raw, a
// valid JSON document, in one pass over its bytes: a scalar is depth 0, {} or
// [] depth 1.
func jsonNestingDepth(raw json.RawMessage) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for _, c := range raw {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
			if depth > deepest {
				deepest = depth
			}
		case c == '}' || c == ']':
			depth--
		}
	}
	return deepest
}

// jsonTextKeepsItsCharacters reports whether raw, a valid JSON document, is
// UTF-8 and every \u escape in its strings stands for a character: a high
// surrogate escape is followed at once by a low one, and no low surrogate
// escape stands alone. A literal U+FFFD and an escaped surrogate pair pass. It
// is one pass over the bytes.
func jsonTextKeepsItsCharacters(raw json.RawMessage) bool {
	if !utf8.Valid(raw) {
		return false
	}
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			i++
			if i >= len(raw) || raw[i] != 'u' {
				continue
			}
			unit, ok := jsonEscapeUnit(raw, i+1)
			if !ok {
				return false
			}
			i += 4
			switch {
			case unit >= 0xD800 && unit <= 0xDBFF:
				if i+2 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
					return false
				}
				low, ok := jsonEscapeUnit(raw, i+3)
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return false
				}
				i += 6
			case unit >= 0xDC00 && unit <= 0xDFFF:
				return false
			}
		}
	}
	return true
}

// jsonEscapeUnit reads the four hex digits of a \u escape starting at raw[at].
func jsonEscapeUnit(raw json.RawMessage, at int) (rune, bool) {
	if at+4 > len(raw) {
		return 0, false
	}
	var unit rune
	for _, c := range raw[at : at+4] {
		unit <<= 4
		switch {
		case c >= '0' && c <= '9':
			unit |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			unit |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			unit |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return unit, true
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// jsonKindOf names the top-level kind of a valid JSON document for error text.
func jsonKindOf(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "empty input"
	}
	switch trimmed[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	default:
		return "a number"
	}
}

// ApplyMetadataEdits applies incremental set (key=value) and unset (key) edits
// to existing metadata and returns the merged JSON. Set values are typed via
// MetadataEditValue; keys are validated with ValidateMetadataKey.
func ApplyMetadataEdits(existing json.RawMessage, setFlags, unsetFlags []string) (json.RawMessage, error) {
	data := make(map[string]json.RawMessage)
	if len(existing) > 0 {
		trimmed := strings.TrimSpace(string(existing))
		if trimmed != "" && trimmed != "null" {
			if err := json.Unmarshal(existing, &data); err != nil {
				return nil, fmt.Errorf("existing metadata is not a JSON object: %w", err)
			}
		}
	}

	for _, kv := range setFlags {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --set-metadata: expected key=value, got %q", kv)
		}
		if err := ValidateMetadataKey(k); err != nil {
			return nil, err
		}
		data[k] = MetadataEditValue(v)
	}

	for _, k := range unsetFlags {
		if err := ValidateMetadataKey(k); err != nil {
			return nil, err
		}
		delete(data, k)
	}

	result, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata: %w", err)
	}
	return json.RawMessage(result), nil
}

// MetadataEditValue converts a --set-metadata string value to JSON, inferring
// the scalar type from the value's spelling: "null" is JSON null, "true"/"false"
// are booleans, anything that scans as a float AND is valid JSON on its own is
// emitted as that literal (so 5 -> 5, 1e3 -> 1e3, while 05, +5 and NaN stay
// strings because they are not valid JSON), and everything else is a string.
//
// This is the shipped v1.2.2 CLI contract (v1.2.2's cmd/bd/update.go toJSONValue,
// copied body-for-body so the parity claim stays checkable). GH#4146 asked for
// string-always so that map[string]string Go consumers keep numeric-looking ids
// and versions like "1e3" intact; that briefly landed here and silently changed
// the CLI contract, so it is served instead by the explicitly typed routes —
// `--metadata` / `--metadata-json` (MergeMetadataJSON) on the CLI and the typed
// set map on the HTTP/Go path (issueops.applyTypedMetadataEdits).
func MetadataEditValue(s string) json.RawMessage {
	if s == "null" {
		return json.RawMessage("null")
	}
	if s == "true" || s == "false" {
		return json.RawMessage(s)
	}
	// Numbers (integer or float), but only when the spelling is also valid JSON
	// on its own — that rejects NaN, Inf, "+5" and leading-zero forms.
	if _, err := fmt.Sscanf(s, "%f", new(float64)); err == nil {
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
	}
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}
