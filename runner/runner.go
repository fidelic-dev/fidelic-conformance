package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Request is one HTTP call in a test (a setup step, the main request, or a
// teardown step).
type Request struct {
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
	Body    any               `yaml:"body"`
	// Query holds URL query parameters that the runner URL-encodes and appends
	// to Path. Lets SOQL tests carry a raw `q: "SELECT ... WHERE ..."` instead
	// of a hand-encoded query string. Values support {{...}} substitution.
	Query map[string]string `yaml:"query"`
	// Save captures values from this request's JSON response into the test's
	// substitution scope, e.g. {accountId: "$.id"}. Referenced later as
	// "{{saved:accountId}}".
	Save map[string]string `yaml:"save"`
}

// Expect is the assertion applied to the main request's response.
type Expect struct {
	Status int  `yaml:"status"`
	Body   any  `yaml:"body"`
	Strict bool `yaml:"strict"`
}

// Test is one conformance case loaded from a YAML file.
type Test struct {
	ID       string    `yaml:"id"`
	Name     string    `yaml:"name"`
	Citation string    `yaml:"citation"`
	Setup    []Request `yaml:"setup"`
	Request  Request   `yaml:"request"`
	Expect   Expect    `yaml:"expect"`
	Teardown []Request `yaml:"teardown"`
	// Requires lists org-side prerequisites this test depends on (custom fields,
	// validation rules, etc.). Purely informational: printed on failure so a
	// missing prerequisite reads as "provision your org", not a confusing diff.
	Requires []string `yaml:"requires"`
	// Targets restricts which target(s) this test runs against. Empty = all
	// targets. This is both an ergonomics feature and a provenance safety rail:
	// emulator-only tests (e.g. /__emulator__/ control calls) set
	// `targets: [emulator]` so they can NEVER be dispatched at a real org.
	Targets []string `yaml:"targets"`

	// Pending marks a ruler-first fixture written BEFORE its feature exists: it
	// documents the target semantics (RED by design) but is NOT run for pass/fail,
	// so the frozen suite stays green. Remove the flag when the feature lands — then
	// it MUST pass. Report separately so it is never silently forgotten.
	Pending bool `yaml:"pending"`

	// file is the source path, kept for error messages.
	file string
}

// Result is the outcome of running one test.
type Result struct {
	Test    Test
	Pass    bool
	Status  int    // actual HTTP status of the main request
	Err     error  // transport/setup error (as opposed to an assertion failure)
	Diffs   []Diff // assertion mismatches
	RawBody []byte // actual main-response body, for failure diagnostics
}

// Runner executes tests against a target.
type Runner struct {
	Target *Target
	Client *http.Client
}

// New builds a Runner for the given target.
func New(target *Target) *Runner {
	return &Runner{
		Target: target,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

// ResetEmulator clears the emulator's data/faults/hooks between tests so state
// registered by one test (a hook, a fault) can't leak into the next — the
// declarative-hook and fault suites otherwise accumulate. Emulator-only: the
// /__emulator__/ control surface doesn't exist on a real org, so the caller only
// invokes this for emulator targets.
func (r *Runner) ResetEmulator(ctx context.Context) error {
	sc := &scope{rand: randToken(), saved: map[string]string{}, target: r.Target}
	_, _, err := r.do(ctx, Request{Method: "POST", Path: "/__emulator__/reset"}, sc)
	return err
}

// LoadTests discovers *.yaml/*.yml files under dir. If only is non-empty, only
// files whose path contains that substring are kept (supports "--only
// tests/auth" style filtering).
func LoadTests(dir, only string) ([]Test, error) {
	var tests []Test
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		if only != "" && !strings.Contains(filepath.ToSlash(path), filepath.ToSlash(only)) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var t Test
		if err := yaml.Unmarshal(raw, &t); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		if t.ID == "" {
			return fmt.Errorf("%s: test has no id", path)
		}
		t.file = path
		tests = append(tests, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].ID < tests[j].ID })
	return tests, nil
}

// RunsOn reports whether this test should run against the named target. A test
// with no Targets runs everywhere; otherwise the target must be listed.
func (t Test) RunsOn(target string) bool {
	if len(t.Targets) == 0 {
		return true
	}
	for _, x := range t.Targets {
		if x == target {
			return true
		}
	}
	return false
}

// scope holds the per-test substitution values.
type scope struct {
	rand   string
	saved  map[string]string
	target *Target
}

// Run executes a single test end to end. Teardown always runs, even if setup,
// the main request, or the assertion failed — so real-org runs don't litter.
func (r *Runner) Run(ctx context.Context, t Test) Result {
	sc := &scope{
		rand:   randToken(),
		saved:  map[string]string{},
		target: r.Target,
	}
	res := Result{Test: t}

	// Teardown runs unconditionally once we begin.
	defer func() {
		for i := range t.Teardown {
			if _, _, err := r.do(ctx, t.Teardown[i], sc); err != nil {
				// Teardown failures are surfaced but don't flip a PASS to FAIL;
				// the human reviews litter separately.
				fmt.Fprintf(os.Stderr, "  [%s] teardown step %d error: %v\n", t.ID, i, err)
			}
		}
	}()

	for i := range t.Setup {
		status, body, err := r.do(ctx, t.Setup[i], sc)
		if err != nil {
			res.Err = fmt.Errorf("setup step %d: %w", i, err)
			return res
		}
		if status < 200 || status >= 300 {
			res.Err = fmt.Errorf("setup step %d returned HTTP %d: %s", i, status, truncate(body))
			return res
		}
		if err := applySaves(t.Setup[i].Save, body, sc); err != nil {
			res.Err = fmt.Errorf("setup step %d: %w", i, err)
			return res
		}
	}

	status, body, err := r.do(ctx, t.Request, sc)
	if err != nil {
		res.Err = err
		return res
	}
	res.Status = status
	res.RawBody = body

	// Capture any values the main request declares (e.g. the created id) so
	// teardown can reference them via {{saved:...}}. Only meaningful on a
	// successful response; on failures the assertion below reports the problem.
	if status >= 200 && status < 300 && len(t.Request.Save) > 0 {
		if err := applySaves(t.Request.Save, body, sc); err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] warning: could not save from main response: %v\n", t.ID, err)
		}
	}

	if status != t.Expect.Status {
		res.Diffs = append(res.Diffs, Diff{"$status", fmt.Sprintf("expected HTTP %d, got %d", t.Expect.Status, status)})
	}

	if t.Expect.Body != nil {
		var actual any
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &actual); err != nil {
				res.Diffs = append(res.Diffs, Diff{"$body", fmt.Sprintf("response is not valid JSON: %v; raw=%s", err, truncate(body))})
				return res
			}
		}
		// Resolve {{rand}}/{{saved:...}} in the expected body so it lines up
		// with values injected into the request. {{match:...}} tokens are left
		// untouched (the token regex doesn't match them) for the comparator.
		expected := sc.substValue(t.Expect.Body)
		res.Diffs = append(res.Diffs, CompareBody(expected, actual, t.Expect.Strict)...)
	}

	res.Pass = len(res.Diffs) == 0
	return res
}

// do performs one request after template substitution and returns status +
// raw body.
func (r *Runner) do(ctx context.Context, req Request, sc *scope) (int, []byte, error) {
	path := sc.subst(req.Path)
	fullURL := r.Target.BaseURL + path
	if len(req.Query) > 0 {
		q := url.Values{}
		for k, v := range req.Query {
			q.Set(k, sc.subst(v))
		}
		sep := "?"
		if strings.Contains(fullURL, "?") {
			sep = "&"
		}
		fullURL += sep + q.Encode()
	}

	headers := map[string]string{}
	for k, v := range req.Headers {
		headers[k] = sc.subst(v)
	}

	var bodyReader io.Reader
	contentType := headers["Content-Type"]
	if req.Body != nil {
		substituted := sc.substValue(req.Body)
		if strings.Contains(contentType, "x-www-form-urlencoded") {
			encoded, err := encodeForm(substituted)
			if err != nil {
				return 0, nil, err
			}
			bodyReader = strings.NewReader(encoded)
		} else {
			buf, err := json.Marshal(substituted)
			if err != nil {
				return 0, nil, err
			}
			bodyReader = bytes.NewReader(buf)
			if contentType == "" {
				headers["Content-Type"] = "application/json"
			}
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, strings.ToUpper(req.Method), fullURL, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	// Inject the target bearer for data requests (not the OAuth token endpoint
	// and not if the test set its own Authorization).
	if r.Target.Bearer() != "" && httpReq.Header.Get("Authorization") == "" && !strings.Contains(path, "/services/oauth2/token") {
		httpReq.Header.Set("Authorization", "Bearer "+r.Target.Bearer())
	}

	resp, err := r.Client.Do(httpReq)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", req.Method, fullURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// --- substitution ---------------------------------------------------------

var tokenRe = regexp.MustCompile(`\{\{(rand|saved:[^}]+|target:[^}]+|env:[^}]+)\}\}`)

// subst replaces {{...}} tokens in a single string.
func (sc *scope) subst(s string) string {
	return tokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		inner := strings.TrimSuffix(strings.TrimPrefix(tok, "{{"), "}}")
		switch {
		case inner == "rand":
			return sc.rand
		case strings.HasPrefix(inner, "saved:"):
			return sc.saved[strings.TrimPrefix(inner, "saved:")]
		case strings.HasPrefix(inner, "env:"):
			return os.Getenv(strings.TrimPrefix(inner, "env:"))
		case strings.HasPrefix(inner, "target:"):
			return sc.targetField(strings.TrimPrefix(inner, "target:"))
		default:
			return tok
		}
	})
}

func (sc *scope) targetField(field string) string {
	switch field {
	case "client_id":
		if sc.target.Auth.ClientID != "" {
			return sc.target.Auth.ClientID
		}
		return "conformance"
	case "client_secret":
		if sc.target.Auth.ClientSecret != "" {
			return sc.target.Auth.ClientSecret
		}
		return "conformance"
	case "base_url":
		return sc.target.BaseURL
	default:
		return ""
	}
}

// substValue recursively substitutes tokens in a decoded YAML value (maps,
// slices, strings). Note: {{match:...}} tokens live only in expect bodies,
// never in requests, so they pass through untouched here.
func (sc *scope) substValue(v any) any {
	switch t := v.(type) {
	case string:
		return sc.subst(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[sc.subst(k)] = sc.substValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = sc.substValue(val)
		}
		return out
	default:
		return v
	}
}

// --- helpers --------------------------------------------------------------

// applySaves extracts JSONPath-lite values ($.a.b) from a response body into
// the substitution scope.
func applySaves(saves map[string]string, body []byte, sc *scope) error {
	if len(saves) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("save: response is not JSON: %w", err)
	}
	for key, expr := range saves {
		val, err := extract(doc, expr)
		if err != nil {
			return fmt.Errorf("save %q: %w", key, err)
		}
		sc.saved[key] = fmt.Sprintf("%v", val)
	}
	return nil
}

// extract walks a "$.a.b" path into a decoded JSON document.
func extract(doc any, expr string) (any, error) {
	p := strings.TrimPrefix(expr, "$")
	p = strings.TrimPrefix(p, ".")
	cur := doc
	if p == "" {
		return cur, nil
	}
	for _, seg := range strings.Split(p, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("path %q: %q is not an object", expr, seg)
		}
		cur, ok = m[seg]
		if !ok {
			return nil, fmt.Errorf("path %q: key %q not found", expr, seg)
		}
	}
	return cur, nil
}

func encodeForm(v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", fmt.Errorf("form-urlencoded body must be a map, got %s", typeName(v))
	}
	form := url.Values{}
	for k, val := range m {
		form.Set(k, fmt.Sprintf("%v", val))
	}
	return form.Encode(), nil
}

func randToken() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "rand000000"
	}
	return hex.EncodeToString(b)
}

func truncate(b []byte) string {
	const max = 400
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
