package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// contractRoot is the committed contract directory, from this package.
var contractRoot = filepath.Join("..", "..", "telemetry", "contract")

// versionDir is the committed directory of the current contract version.
var versionDir = filepath.Join(contractRoot, ContractVersion)

// validator checks an instance against the subset of JSON Schema 2020-12
// the contract's schema uses. It refuses a keyword it does not implement;
// so the schema cannot grow a rule the tests silently ignore.
type validator struct {
	root map[string]any
}

var annotationKeywords = []string{"$schema", "$id", "title", "description", "$defs", "format", "x-stability", "x-absent"}

func loadSchema(t *testing.T, path string) validator {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	return validator{root: root}
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(data, &v))
	return v
}

// errors returns every rule the instance breaks, sorted; none means valid.
func (v validator) errors(inst any) []string {
	out := v.check(v.root, inst, "$")
	slices.Sort(out)
	return out
}

func (v validator) check(node map[string]any, inst any, at string) []string {
	var out []string
	for k, val := range node {
		switch k {
		case "$ref":
			ref, _ := val.(string)
			target, ok := v.resolve(ref)
			if !ok {
				out = append(out, at+": unresolved reference "+ref)
				continue
			}
			out = append(out, v.check(target, inst, at)...)
		case "type":
			if !typeMatches(val.(string), inst) {
				out = append(out, fmt.Sprintf("%s: not of type %v", at, val))
			}
		case "const":
			if !reflect.DeepEqual(val, inst) {
				out = append(out, fmt.Sprintf("%s: not the constant %v", at, val))
			}
		case "enum":
			found := false
			for _, e := range val.([]any) {
				if reflect.DeepEqual(e, inst) {
					found = true
				}
			}
			if !found {
				out = append(out, fmt.Sprintf("%s: %v is not in the list", at, inst))
			}
		case "pattern":
			if s, ok := inst.(string); ok && !regexp.MustCompile(val.(string)).MatchString(s) {
				out = append(out, fmt.Sprintf("%s: does not match %v", at, val))
			}
		case "maxLength":
			if s, ok := inst.(string); ok && utf8.RuneCountInString(s) > int(val.(float64)) {
				out = append(out, at+": longer than "+fmt.Sprint(val))
			}
		case "minimum":
			if n, ok := inst.(float64); ok && n < val.(float64) {
				out = append(out, fmt.Sprintf("%s: below %v", at, val))
			}
		case "maximum":
			if n, ok := inst.(float64); ok && n > val.(float64) {
				out = append(out, fmt.Sprintf("%s: above %v", at, val))
			}
		case "maxItems":
			if a, ok := inst.([]any); ok && len(a) > int(val.(float64)) {
				out = append(out, fmt.Sprintf("%s: more than %v items", at, val))
			}
		case "items":
			if a, ok := inst.([]any); ok {
				for i, e := range a {
					out = append(out, v.check(val.(map[string]any), e, fmt.Sprintf("%s[%d]", at, i))...)
				}
			}
		case "properties":
			if o, ok := inst.(map[string]any); ok {
				for name, sub := range val.(map[string]any) {
					if e, present := o[name]; present {
						out = append(out, v.check(sub.(map[string]any), e, at+"."+name)...)
					}
				}
			}
		case "required":
			if o, ok := inst.(map[string]any); ok {
				for _, name := range val.([]any) {
					if _, present := o[name.(string)]; !present {
						out = append(out, fmt.Sprintf("%s: missing %v", at, name))
					}
				}
			}
		case "oneOf":
			passing := 0
			for _, sub := range val.([]any) {
				if len(v.check(sub.(map[string]any), inst, at)) == 0 {
					passing++
				}
			}
			if passing != 1 {
				out = append(out, fmt.Sprintf("%s: matches %d of the alternatives, not one", at, passing))
				for _, sub := range val.([]any) {
					out = append(out, v.check(sub.(map[string]any), inst, at)...)
				}
			}
		case "allOf":
			for _, sub := range val.([]any) {
				out = append(out, v.check(sub.(map[string]any), inst, at)...)
			}
		case "not":
			if len(v.check(val.(map[string]any), inst, at)) == 0 {
				out = append(out, at+": matches a schema it must not match")
			}
		case "if", "then", "else":
			// Evaluated together below.
		default:
			if !slices.Contains(annotationKeywords, k) {
				out = append(out, at+": unsupported keyword "+k)
			}
		}
	}
	if cond, ok := node["if"].(map[string]any); ok {
		if len(v.check(cond, inst, at)) == 0 {
			if then, ok := node["then"].(map[string]any); ok {
				out = append(out, v.check(then, inst, at)...)
			}
		} else if els, ok := node["else"].(map[string]any); ok {
			out = append(out, v.check(els, inst, at)...)
		}
	}
	return out
}

func (v validator) resolve(ref string) (map[string]any, bool) {
	rest, ok := strings.CutPrefix(ref, "#/$defs/")
	if !ok {
		return nil, false
	}
	defs, _ := v.root["$defs"].(map[string]any)
	target, ok := defs[rest].(map[string]any)
	return target, ok
}

func typeMatches(typ string, inst any) bool {
	switch typ {
	case "object":
		_, ok := inst.(map[string]any)
		return ok
	case "array":
		_, ok := inst.([]any)
		return ok
	case "string":
		_, ok := inst.(string)
		return ok
	case "boolean":
		_, ok := inst.(bool)
		return ok
	case "integer":
		n, ok := inst.(float64)
		return ok && n == math.Trunc(n)
	}
	return false
}

// TestValidatorRefusesWhatItDoesNotImplement asserts the validator reports a
// keyword outside its subset and applies the ones it knows; a schema
// rule is never skipped without a failure.
func TestValidatorRefusesWhatItDoesNotImplement(t *testing.T) {
	v := validator{root: map[string]any{}}
	require.NotEmpty(t, v.check(map[string]any{"uniqueItems": true}, []any{}, "$"))
	require.Empty(t, v.check(map[string]any{"type": "string", "pattern": "^a$", "description": "x"}, "a", "$"))
	require.NotEmpty(t, v.check(map[string]any{"type": "string", "pattern": "^a$"}, "b", "$"))
	require.NotEmpty(t, v.check(map[string]any{"type": "integer"}, 1.5, "$"))
	require.NotEmpty(t, v.check(map[string]any{"maxItems": 1.0}, []any{1.0, 2.0}, "$"))
	require.NotEmpty(t, v.check(map[string]any{"not": map[string]any{"type": "string"}}, "x", "$"))
}
