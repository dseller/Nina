package routetable

import (
	"strings"
	"testing"

	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/oas"
)

const svcSpec = `
openapi: 3.1.0
info: { title: Svc, version: 1.0.0 }
paths:
  /v1/things:
    get:
      operationId: listThings
      responses: { '200': { description: ok } }
    post:
      operationId: createThing
      responses: { '201': { description: ok } }
  /v1/things/{id}:
    get:
      operationId: getThing
      parameters:
        - name: id
          in: path
          required: true
          schema: { type: string }
      responses: { '200': { description: ok } }
    delete:
      operationId: deleteThing
      parameters:
        - name: id
          in: path
          required: true
          schema: { type: string }
      responses: { '204': { description: ok } }
  /v1/admin/purge:
    post:
      operationId: adminPurge
      responses: { '200': { description: ok } }
`

func loadSpec(t *testing.T, src string) *oas.Spec {
	t.Helper()
	s, err := oas.Load([]byte(src), oas.LoadOptions{})
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	return s
}

func build(t *testing.T, cfgYAML string, specs map[string]*oas.Spec) (*Table, error) {
	t.Helper()
	cfg, err := config.Parse("test.yaml", []byte(cfgYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return Build(BuildInput{Config: cfg, Specs: specs})
}

func routeSet(tb *Table) map[string]string {
	out := map[string]string{}
	for _, r := range tb.Routes {
		out[r.Method+" "+r.GatewayPath] = r.UpstreamPath
	}
	return out
}

func TestStripPrefixAndMount(t *testing.T) {
	cfg := `
version: 1
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
    strip_prefix: /v1
expose:
  - backend: svc
    prefix: /api
`
	tb, err := build(t, cfg, map[string]*oas.Spec{"svc": loadSpec(t, svcSpec)})
	if err != nil {
		t.Fatal(err)
	}
	got := routeSet(tb)
	want := map[string]string{
		"GET /api/things":         "/v1/things",
		"POST /api/things":        "/v1/things",
		"GET /api/things/{id}":    "/v1/things/{id}",
		"DELETE /api/things/{id}": "/v1/things/{id}",
		"POST /api/admin/purge":   "/v1/admin/purge",
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("route %s -> %q, want %q", k, got[k], v)
		}
	}
}

func TestIncludeExclude(t *testing.T) {
	cfg := `
version: 1
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
expose:
  - backend: svc
    prefix: /api
    include: ["*Thing", "*Things"]
    exclude: ["delete*"]
`
	tb, err := build(t, cfg, map[string]*oas.Spec{"svc": loadSpec(t, svcSpec)})
	if err != nil {
		t.Fatal(err)
	}
	got := routeSet(tb)
	for _, want := range []string{"GET /api/v1/things", "POST /api/v1/things", "GET /api/v1/things/{id}"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s (have %v)", want, keys(got))
		}
	}
	// adminPurge fails the include globs; deleteThing is excluded.
	for _, gone := range []string{"POST /api/v1/admin/purge", "DELETE /api/v1/things/{id}"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s should not be served", gone)
		}
	}
}

func TestEmptySelectionIsAnError(t *testing.T) {
	cfg := `
version: 1
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
expose:
  - backend: svc
    include: ["nothingMatchesThis"]
`
	_, err := build(t, cfg, map[string]*oas.Spec{"svc": loadSpec(t, svcSpec)})
	if err == nil {
		t.Fatal("expected an error when globs select nothing")
	}
	if !strings.Contains(err.Error(), "selected no operations") {
		t.Errorf("error should explain the empty selection, got: %v", err)
	}
}

func TestOverridePathAndDisable(t *testing.T) {
	cfg := `
version: 1
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
    strip_prefix: /v1
expose:
  - backend: svc
    prefix: /api
    overrides:
      createThing:
        path: /api/things/new
      adminPurge:
        disabled: true
`
	tb, err := build(t, cfg, map[string]*oas.Spec{"svc": loadSpec(t, svcSpec)})
	if err != nil {
		t.Fatal(err)
	}
	got := routeSet(tb)
	if _, ok := got["POST /api/things/new"]; !ok {
		t.Errorf("override path was not applied (have %v)", keys(got))
	}
	if _, ok := got["POST /api/things"]; ok {
		t.Error("the original path should be replaced by the override")
	}
	if _, ok := got["POST /api/admin/purge"]; ok {
		t.Error("a disabled operation should not be served")
	}
}

// An override that drops a path parameter would leave the proxy unable to
// rebuild the upstream URL, so it has to be caught at build time.
func TestOverrideMustKeepPathParameters(t *testing.T) {
	cfg := `
version: 1
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
expose:
  - backend: svc
    overrides:
      getThing:
        path: /things/single
`
	_, err := build(t, cfg, map[string]*oas.Spec{"svc": loadSpec(t, svcSpec)})
	if err == nil {
		t.Fatal("expected an error for an override that drops {id}")
	}
	if !strings.Contains(err.Error(), "{id}") {
		t.Errorf("error should name the missing parameter, got: %v", err)
	}
}

func TestCollisionBetweenBackends(t *testing.T) {
	cfg := `
version: 1
backends:
  - name: a
    spec: { file: ./a.yaml }
    hosts: ["https://a.internal"]
  - name: b
    spec: { file: ./b.yaml }
    hosts: ["https://b.internal"]
expose:
  - backend: a
    prefix: /api
  - backend: b
    prefix: /api
`
	specs := map[string]*oas.Spec{
		"a": loadSpec(t, svcSpec),
		"b": loadSpec(t, svcSpec),
	}
	_, err := build(t, cfg, specs)
	if err == nil {
		t.Fatal("expected a collision error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "collision") || !strings.Contains(msg, "a.") || !strings.Contains(msg, "b.") {
		t.Errorf("collision error should name both claimants, got: %v", err)
	}
}

func TestAllowedMethodsPerPath(t *testing.T) {
	cfg := `
version: 1
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
    strip_prefix: /v1
expose:
  - backend: svc
    prefix: /api
`
	tb, err := build(t, cfg, map[string]*oas.Spec{"svc": loadSpec(t, svcSpec)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tb.Routes {
		if r.GatewayPath != "/api/things/{id}" {
			continue
		}
		if len(r.AllowedMethods) != 2 ||
			r.AllowedMethods[0] != "DELETE" || r.AllowedMethods[1] != "GET" {
			t.Errorf("AllowedMethods for %s = %v, want [DELETE GET]", r, r.AllowedMethods)
		}
	}
}

func TestOperationIDsAreNamespaced(t *testing.T) {
	cfg := `
version: 1
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
expose:
  - backend: svc
`
	tb, err := build(t, cfg, map[string]*oas.Spec{"svc": loadSpec(t, svcSpec)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tb.Routes {
		if !strings.HasPrefix(r.OperationID, "svc_") {
			t.Errorf("operation id %q is not namespaced", r.OperationID)
		}
	}
}

func TestStripPrefixHelper(t *testing.T) {
	tests := []struct{ p, prefix, want string }{
		{"/v1/users", "/v1", "/users"},
		{"/v1", "/v1", "/"},
		{"/v1/users", "/v2", "/v1/users"}, // no match, unchanged
		{"/v1users", "/v1", "/v1users"},   // segment boundary respected
		{"/v1/users", "", "/v1/users"},    // no prefix configured
		{"/v1/users", "/v1/", "/users"},   // trailing slash tolerated
	}
	for _, tc := range tests {
		if got := stripPrefix(tc.p, tc.prefix); got != tc.want {
			t.Errorf("stripPrefix(%q, %q) = %q, want %q", tc.p, tc.prefix, got, tc.want)
		}
	}
}

func TestJoinPathHelper(t *testing.T) {
	tests := []struct{ prefix, p, want string }{
		{"/api", "/users", "/api/users"},
		{"", "/users", "/users"},
		{"/api", "/", "/api"},
		{"", "/", "/"},
		{"/api/", "/users", "/api/users"},
	}
	for _, tc := range tests {
		if got := joinPath(tc.prefix, tc.p); got != tc.want {
			t.Errorf("joinPath(%q, %q) = %q, want %q", tc.prefix, tc.p, got, tc.want)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// taggedSpec exercises hide_tags: one operation carries an `internal` tag and
// is the only user of the Purge schema, so hiding it must take the schema with
// it.
const taggedSpec = `
openapi: 3.1.0
info: { title: Svc, version: 1.0.0 }
paths:
  /things:
    get:
      operationId: listThings
      tags: [public]
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Thing' }
  /admin/purge:
    post:
      operationId: adminPurge
      tags: [public, internal]
      requestBody:
        content:
          application/json:
            schema: { $ref: '#/components/schemas/Purge' }
      responses: { '200': { description: ok } }
components:
  schemas:
    Thing:
      type: object
      properties: { id: { type: string } }
    Purge:
      type: object
      properties: { force: { type: boolean } }
`

func TestHideTagsKeepsRouteButDropsItFromTheDocument(t *testing.T) {
	cfg := `
version: 1
spec:
  hide_tags: ["internal"]
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
expose:
  - backend: svc
    prefix: /api
`
	tb, err := build(t, cfg, map[string]*oas.Spec{"svc": loadSpec(t, taggedSpec)})
	if err != nil {
		t.Fatal(err)
	}

	// Still served, with everything a visible route has.
	got := routeSet(tb)
	if _, ok := got["POST /api/admin/purge"]; !ok {
		t.Fatalf("hidden route is no longer served; have %v", keys(got))
	}
	hidden := tb.HiddenRoutes()
	if len(hidden) != 1 || hidden[0].String() != "POST /api/admin/purge" {
		t.Fatalf("HiddenRoutes() = %v, want just POST /api/admin/purge", hidden)
	}

	// Absent from the document, along with the schema only it referenced.
	doc := string(tb.SpecYAML)
	if strings.Contains(doc, "/api/admin/purge") {
		t.Errorf("hidden route appears in the published document:\n%s", doc)
	}
	if strings.Contains(doc, "Purge") {
		t.Errorf("schema reachable only from a hidden route was published:\n%s", doc)
	}
	// The visible route and its components are untouched.
	if !strings.Contains(doc, "/api/things") || !strings.Contains(doc, "SvcThing") {
		t.Errorf("visible route or its schema went missing:\n%s", doc)
	}
}

func TestHideTagsMatchesAnyTagAndIsExact(t *testing.T) {
	cfgFor := func(tag string) string {
		return `
version: 1
spec:
  hide_tags: ["` + tag + `"]
backends:
  - name: svc
    spec: { file: ./svc.yaml }
    hosts: ["https://svc.internal"]
expose:
  - backend: svc
    prefix: /api
`
	}
	// `public` is carried by both operations, so both are hidden.
	tb, err := build(t, cfgFor("public"), map[string]*oas.Spec{"svc": loadSpec(t, taggedSpec)})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tb.HiddenRoutes()); n != 2 {
		t.Errorf("hid %d routes on a tag both operations carry, want 2", n)
	}
	if !strings.Contains(string(tb.SpecYAML), "paths:") {
		t.Errorf("a document with every operation hidden should still render:\n%s", tb.SpecYAML)
	}

	// Matching is exact: a different case hides nothing.
	tb, err = build(t, cfgFor("Internal"), map[string]*oas.Spec{"svc": loadSpec(t, taggedSpec)})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tb.HiddenRoutes()); n != 0 {
		t.Errorf("tag matching is not exact: %d routes hidden on a case mismatch", n)
	}
}

func TestHiddenRouteStillCollides(t *testing.T) {
	// Hiding a route removes it from the document, not from the router, so it
	// must still fail the build rather than produce two routes for one slot.
	cfg := `
version: 1
spec:
  hide_tags: ["internal"]
backends:
  - name: a
    spec: { file: ./a.yaml }
    hosts: ["https://a.internal"]
  - name: b
    spec: { file: ./b.yaml }
    hosts: ["https://b.internal"]
expose:
  - backend: a
    prefix: /api
    include: ["adminPurge"]
  - backend: b
    prefix: /api
    include: ["adminPurge"]
`
	specs := map[string]*oas.Spec{
		"a": loadSpec(t, taggedSpec),
		"b": loadSpec(t, taggedSpec),
	}
	_, err := build(t, cfg, specs)
	if err == nil {
		t.Fatal("expected a collision error between two hidden routes")
	}
	msg := err.Error()
	if !strings.Contains(msg, "collision") || !strings.Contains(msg, "a.") || !strings.Contains(msg, "b.") {
		t.Errorf("collision error should name both claimants, got: %v", err)
	}
}
