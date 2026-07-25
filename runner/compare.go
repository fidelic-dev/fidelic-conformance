package runner

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Diff is a single mismatch between an expected and an actual response body.
type Diff struct {
	Path string // JSON path where the mismatch occurred, e.g. "$.errors[0].fields"
	Msg  string
}

func (d Diff) String() string { return fmt.Sprintf("%s: %s", d.Path, d.Msg) }

// sfIDRe matches a Salesforce record ID: 15 (case-sensitive) or 18
// (case-safe) base-62 characters.
var sfIDRe = regexp.MustCompile(`^[a-zA-Z0-9]{15}([a-zA-Z0-9]{3})?$`)

// iso8601Layouts covers the datetime shapes Salesforce emits. SF's canonical
// form is "2024-05-01T12:34:56.000+0000" (no colon in the zone offset).
var iso8601Layouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05.000Z07:00",
	"2006-01-02T15:04:05-0700",
	time.RFC3339,
	time.RFC3339Nano,
}

// CompareBody compares an expected body (from a test's YAML) against an actual
// decoded JSON body. Matchers may appear anywhere in the expected tree as
// string values of the form "{{match:NAME}}".
//
// Semantics when strict is false (the default):
//   - Objects: every expected key must match; extra actual keys are ignored
//     (real-org responses carry many fields we don't assert on).
//   - Arrays: an empty expected array requires an empty actual array; a
//     non-empty expected array uses contains/subset semantics — every expected
//     element must match at least one actual element (order-independent). This
//     is what makes error-array assertions robust.
//
// When strict is true, objects must have exactly the expected key set and
// arrays must match element-for-element in order.
func CompareBody(expected, actual any, strict bool) []Diff {
	return compare("$", expected, actual, strict)
}

func compare(path string, exp, act any, strict bool) []Diff {
	if s, ok := exp.(string); ok && isMatcher(s) {
		return applyMatcher(path, s, act)
	}

	switch e := exp.(type) {
	case map[string]any:
		am, ok := act.(map[string]any)
		if !ok {
			return one(path, "expected object, got %s", typeName(act))
		}
		var diffs []Diff
		for k, ev := range e {
			av, present := am[k]
			if !present {
				if s, ok := ev.(string); ok && s == "{{match:any}}" {
					continue // match:any tolerates an absent key
				}
				diffs = append(diffs, Diff{path + "." + k, "missing key"})
				continue
			}
			diffs = append(diffs, compare(path+"."+k, ev, av, strict)...)
		}
		if strict {
			for k := range am {
				if _, ok := e[k]; !ok {
					diffs = append(diffs, Diff{path + "." + k, "unexpected key (strict mode)"})
				}
			}
		}
		return diffs

	case []any:
		aa, ok := act.([]any)
		if !ok {
			return one(path, "expected array, got %s", typeName(act))
		}
		if len(e) == 0 {
			if len(aa) != 0 {
				return one(path, "expected empty array, got %d element(s)", len(aa))
			}
			return nil
		}
		if strict {
			if len(aa) != len(e) {
				return one(path, "expected %d array element(s), got %d", len(e), len(aa))
			}
			var diffs []Diff
			for i := range e {
				diffs = append(diffs, compare(fmt.Sprintf("%s[%d]", path, i), e[i], aa[i], strict)...)
			}
			return diffs
		}
		// Non-strict: contains/subset semantics.
		var diffs []Diff
		for i, ev := range e {
			if !containsMatch(ev, aa) {
				diffs = append(diffs, Diff{fmt.Sprintf("%s[%d]", path, i), "no actual array element matches this expected element"})
			}
		}
		return diffs

	default:
		if !scalarEqual(exp, act) {
			return one(path, "expected %v (%s), got %v (%s)", exp, typeName(exp), act, typeName(act))
		}
		return nil
	}
}

// containsMatch reports whether ev matches at least one element of aa under
// non-strict comparison.
func containsMatch(ev any, aa []any) bool {
	for _, av := range aa {
		if len(compare("$", ev, av, false)) == 0 {
			return true
		}
	}
	return false
}

func isMatcher(s string) bool {
	return strings.HasPrefix(s, "{{match:") && strings.HasSuffix(s, "}}")
}

func applyMatcher(path, matcher string, act any) []Diff {
	name := strings.TrimSuffix(strings.TrimPrefix(matcher, "{{match:"), "}}")
	// {{match:regex:PATTERN}} — assert a string against a regular expression.
	// Used where a response is structurally stable but has variable substrings
	// (e.g. a parser error message whose line/column vary with the payload).
	if pattern, ok := strings.CutPrefix(name, "regex:"); ok {
		s, ok := act.(string)
		if !ok {
			return one(path, "match:regex — expected a string, got %s", typeName(act))
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return one(path, "match:regex — invalid pattern %q: %v", pattern, err)
		}
		if !re.MatchString(s) {
			return one(path, "match:regex — %q does not match /%s/", s, pattern)
		}
		return nil
	}
	switch name {
	case "any":
		return nil
	case "string":
		if _, ok := act.(string); !ok {
			return one(path, "match:string — expected a string, got %s", typeName(act))
		}
		return nil
	case "sfid":
		s, ok := act.(string)
		if !ok {
			return one(path, "match:sfid — expected a string, got %s", typeName(act))
		}
		if !sfIDRe.MatchString(s) {
			return one(path, "match:sfid — %q is not a 15/18-char Salesforce ID", s)
		}
		return nil
	case "iso8601":
		s, ok := act.(string)
		if !ok {
			return one(path, "match:iso8601 — expected a string, got %s", typeName(act))
		}
		for _, layout := range iso8601Layouts {
			if _, err := time.Parse(layout, s); err == nil {
				return nil
			}
		}
		return one(path, "match:iso8601 — %q is not a recognized ISO-8601 datetime", s)
	default:
		return one(path, "unknown matcher %q", matcher)
	}
}

// scalarEqual compares two scalars, normalizing JSON/YAML numeric type skew
// (YAML ints vs JSON float64) so 201 == 201.0.
func scalarEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
		return false
	}
	return a == b
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	default:
		return 0, false
	}
}

func typeName(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "bool"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func one(path, format string, args ...any) []Diff {
	return []Diff{{path, fmt.Sprintf(format, args...)}}
}
