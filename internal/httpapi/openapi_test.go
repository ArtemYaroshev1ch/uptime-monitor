package httpapi

// Contract tests between the router and api/openapi.yaml, which is written
// by hand:
//
//   - TestOpenAPIRoutes: every route of the router is documented, and every
//     documented operation has a route.
//   - checkResponse, called by testAPI.do for every request of every HTTP
//     test: the status code is documented for that operation and the body
//     matches the documented schema (no missing required fields, no
//     undocumented ones, right types, enums, uuid and date-time formats).
//   - TestMain: after a full run, every documented operation was called at
//     least once, so no part of the document goes unchecked.
//
// The schema checks cover the subset of OpenAPI 3.1 / JSON Schema the
// document uses; an unsupported keyword fails the test instead of being
// silently ignored.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const specFile = "../../api/openapi.yaml"

// openAPI is the parsed document plus the path templates as regular
// expressions, longest first.
type openAPI struct {
	doc   map[string]any
	paths []specPath
}

type specPath struct {
	template string
	re       *regexp.Regexp
}

// The document is parsed once per test binary; tests run in parallel.
var loadSpec = sync.OnceValues(func() (*openAPI, error) {
	raw, err := os.ReadFile(specFile)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", specFile, err)
	}
	s := &openAPI{doc: doc}
	for tmpl := range asMap(doc["paths"]) {
		// "/api/v1/monitors/{id}" -> ^/api/v1/monitors/[^/]+$
		pattern := regexp.MustCompile(`\\\{[^/]+\\\}`).ReplaceAllString(regexp.QuoteMeta(tmpl), `[^/]+`)
		s.paths = append(s.paths, specPath{template: tmpl, re: regexp.MustCompile("^" + pattern + "$")})
	}
	// A literal segment must win over a parameter: try the most specific
	// (longest) template first.
	sort.Slice(s.paths, func(i, j int) bool { return len(s.paths[i].template) > len(s.paths[j].template) })
	return s, nil
})

func spec(t *testing.T) *openAPI {
	t.Helper()
	s, err := loadSpec()
	require.NoError(t, err)
	return s
}

// called records the operations the tests exercised ("GET /api/v1/me").
var called = struct {
	sync.Mutex
	ops map[string]bool
}{ops: map[string]bool{}}

func TestMain(m *testing.M) {
	code := m.Run()
	// Only a full run calls everything; -run selects a subset.
	if code == 0 && flag.Lookup("test.run").Value.String() == "" {
		if missing := uncalledOperations(); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "openapi.yaml operations no test called: %s\n", strings.Join(missing, ", "))
			code = 1
		}
	}
	os.Exit(code)
}

func uncalledOperations() []string {
	s, err := loadSpec()
	if err != nil {
		return []string{err.Error()}
	}
	called.Lock()
	defer called.Unlock()
	var missing []string
	for _, op := range s.operations() {
		if !called.ops[op] {
			missing = append(missing, op)
		}
	}
	return missing
}

var httpMethods = []string{"get", "post", "put", "patch", "delete"}

// operations returns "METHOD /template" for every documented operation.
func (s *openAPI) operations() []string {
	var ops []string
	for tmpl, item := range asMap(s.doc["paths"]) {
		for _, m := range httpMethods {
			if _, ok := asMap(item)[m]; ok {
				ops = append(ops, strings.ToUpper(m)+" "+tmpl)
			}
		}
	}
	sort.Strings(ops)
	return ops
}

func TestOpenAPIRoutes(t *testing.T) {
	t.Parallel()
	routes, ok := newTestRouter(t, fakeDB{}).(chi.Routes)
	require.True(t, ok, "the router must be a chi router")

	var got []string
	require.NoError(t, chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	}))
	sort.Strings(got)

	require.Equal(t, spec(t).operations(), got,
		"routes of the router and operations of api/openapi.yaml differ")
}

// checkResponse fails t if rec is not a documented answer to method path.
// Requests to undocumented paths (deliberately unknown URLs) are skipped:
// TestOpenAPIRoutes covers the route list.
func checkResponse(t *testing.T, method, path string, rec *httptest.ResponseRecorder) {
	t.Helper()
	s := spec(t)
	path, _, _ = strings.Cut(path, "?")

	tmpl := ""
	for _, p := range s.paths {
		if p.re.MatchString(path) {
			tmpl = p.template
			break
		}
	}
	if tmpl == "" {
		return
	}
	item := asMap(asMap(s.doc["paths"])[tmpl])
	op, ok := item[strings.ToLower(method)]
	if !ok {
		return // 405, not an operation
	}
	name := method + " " + tmpl
	called.Lock()
	called.ops[name] = true
	called.Unlock()

	responses := asMap(asMap(op)["responses"])
	resp, ok := responses[strconv.Itoa(rec.Code)]
	if !ok {
		t.Errorf("openapi: %s answered %d, which is not documented", name, rec.Code)
		return
	}
	resp = s.resolve(t, resp)

	content := asMap(asMap(resp)["content"])
	if len(content) == 0 {
		if rec.Body.Len() != 0 {
			t.Errorf("openapi: %s %d must have no body, got %q", name, rec.Code, rec.Body.String())
		}
		return
	}
	media := asMap(content["application/json"])
	if media == nil {
		t.Errorf("openapi: %s %d: only application/json is supported", name, rec.Code)
		return
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("openapi: %s %d: Content-Type %q, want application/json", name, rec.Code, ct)
	}
	var body any
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		t.Errorf("openapi: %s %d: body is not JSON: %v", name, rec.Code, err)
		return
	}
	for _, msg := range s.validate(t, media["schema"], body, "body") {
		t.Errorf("openapi: %s %d: %s", name, rec.Code, msg)
	}
}

// resolve follows a local $ref ("#/components/schemas/Monitor").
func (s *openAPI) resolve(t *testing.T, node any) any {
	t.Helper()
	for {
		ref, ok := asMap(node)["$ref"].(string)
		if !ok {
			return node
		}
		node = any(s.doc)
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			node = asMap(node)[part]
		}
		require.NotNil(t, node, "openapi: unresolved $ref %s", ref)
	}
}

// Keywords validate understands, plus annotations that do not constrain.
var schemaKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "enum": true, "const": true, "format": true,
	"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
	"description": true, "example": true, "default": true,
}

// validate returns what is wrong with v against schema; at names the value.
// Objects are closed: a property missing from "properties" is an error
// unless additionalProperties is a schema. That catches fields the code
// returns but the document does not describe.
func (s *openAPI) validate(t *testing.T, schema, v any, at string) []string {
	t.Helper()
	sc := asMap(s.resolve(t, schema))
	for k := range sc {
		if !schemaKeywords[k] {
			t.Fatalf("openapi: schema at %s uses %q, which the contract test does not support", at, k)
		}
	}

	if !typeMatches(sc["type"], v) {
		return []string{fmt.Sprintf("%s: %s, want type %v", at, jsonType(v), sc["type"])}
	}
	if c, ok := sc["const"]; ok && !sameValue(c, v) {
		return []string{fmt.Sprintf("%s: %v, want %v", at, v, c)}
	}
	if enum, ok := sc["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return sameValue(e, v) }) {
		return []string{fmt.Sprintf("%s: %v is not one of %v", at, v, enum)}
	}

	var errs []string
	switch val := v.(type) {
	case string:
		if msg := checkFormat(sc["format"], val); msg != "" {
			errs = append(errs, at+": "+msg)
		}
	case []any:
		for i, item := range val {
			errs = append(errs, s.validate(t, sc["items"], item, fmt.Sprintf("%s[%d]", at, i))...)
		}
	case map[string]any:
		props := asMap(sc["properties"])
		for _, r := range asSlice(sc["required"]) {
			if _, ok := val[r.(string)]; !ok {
				errs = append(errs, fmt.Sprintf("%s: required field %q is missing", at, r))
			}
		}
		for k, fv := range val {
			if ps, ok := props[k]; ok {
				errs = append(errs, s.validate(t, ps, fv, at+"."+k)...)
				continue
			}
			extra, ok := sc["additionalProperties"]
			if !ok || extra == false {
				errs = append(errs, fmt.Sprintf("%s: field %q is not documented", at, k))
				continue
			}
			if extra != true {
				errs = append(errs, s.validate(t, extra, fv, at+"."+k)...)
			}
		}
	}
	return errs
}

func typeMatches(want, v any) bool {
	if want == nil {
		return true
	}
	types, ok := want.([]any)
	if !ok {
		types = []any{want}
	}
	got := jsonType(v)
	for _, w := range types {
		if w == got || (w == "number" && got == "integer") {
			return true
		}
	}
	return false
}

func jsonType(v any) string {
	switch val := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case json.Number:
		if f, err := val.Float64(); err == nil && f == math.Trunc(f) && !strings.ContainsAny(val.String(), ".eE") {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

// sameValue compares a YAML value of the document with a JSON value.
func sameValue(docValue, v any) bool {
	if n, ok := v.(json.Number); ok {
		f, err := n.Float64()
		return err == nil && fmt.Sprint(docValue) == fmt.Sprint(f)
	}
	return docValue == v
}

func checkFormat(format any, v string) string {
	switch format {
	case "uuid":
		if _, err := uuid.Parse(v); err != nil {
			return fmt.Sprintf("%q is not a uuid", v)
		}
	case "date-time":
		if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
			return fmt.Sprintf("%q is not an RFC 3339 date-time", v)
		}
	}
	return ""
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// TestOpenAPIValidator checks the checker itself on documents and bodies
// whose verdict is known.
func TestOpenAPIValidator(t *testing.T) {
	t.Parallel()
	s := spec(t)
	user := map[string]any{"$ref": "#/components/schemas/User"}
	valid := `{"id":"0b8f3c2e-5a1d-4c8e-9f0a-2d6b7e1c4a9f","email":"a@example.com","created_at":"2026-01-02T03:04:05Z"}`

	tests := []struct {
		name    string
		schema  any
		body    string
		wantErr string // substring of the first error; empty: valid
	}{
		{"valid object", user, valid, ""},
		{"missing required", user, `{"id":"0b8f3c2e-5a1d-4c8e-9f0a-2d6b7e1c4a9f","email":"a@example.com"}`, `required field "created_at"`},
		{"undocumented field", user, strings.TrimSuffix(valid, "}") + `,"password_hash":"x"}`, `"password_hash" is not documented`},
		{"bad uuid", user, strings.Replace(valid, "0b8f3c2e", "nope", 1), "is not a uuid"},
		{"bad date-time", user, strings.Replace(valid, "2026-01-02T03:04:05Z", "yesterday", 1), "RFC 3339"},
		{"wrong type", user, `{"id":1,"email":"a@example.com","created_at":"2026-01-02T03:04:05Z"}`, "want type string"},
		{"enum", map[string]any{"type": "string", "enum": []any{"up", "down"}}, `"sideways"`, "is not one of"},
		{"nullable", map[string]any{"type": []any{"integer", "null"}}, `null`, ""},
		{"integer is not a fraction", map[string]any{"type": "integer"}, `1.5`, "want type integer"},
		{"array items", map[string]any{"type": "array", "items": user}, `[` + valid + `,{}]`, "body[1]: required field"},
		{"open map", map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}, `{"a":"x","b":2}`, "body.b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dec := json.NewDecoder(strings.NewReader(tt.body))
			dec.UseNumber()
			var v any
			require.NoError(t, dec.Decode(&v))
			errs := s.validate(t, tt.schema, v, "body")
			if tt.wantErr == "" {
				require.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs)
			require.Contains(t, errs[0], tt.wantErr)
		})
	}
}
