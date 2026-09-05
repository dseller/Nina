package oas

import (
	"os"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"
	yaml "go.yaml.in/yaml/v4"
)

func loadFixture(t *testing.T, name string) *Spec {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(b, LoadOptions{BasePath: "testdata"})
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return s
}

// exposeAll builds the ExposedOp list the same way the route builder will:
// gateway path = prefix + upstream path, operationId = backend_upstreamId.
func exposeAll(src *SpecSource, prefix string) []ExposedOp {
	var out []ExposedOp
	for _, op := range src.Spec.Operations() {
		out = append(out, ExposedOp{
			Source:      src,
			Op:          op,
			GatewayPath: prefix + op.Path,
			OperationID: strings.ToLower(src.Backend) + "_" + op.OperationID,
		})
	}
	return out
}

func mergeFixtures(t *testing.T, dedupe bool) (*yaml.Node, []byte) {
	t.Helper()
	users := &SpecSource{Spec: loadFixture(t, "users.yaml"), Backend: "users", Namespace: "Users"}
	orders := &SpecSource{Spec: loadFixture(t, "orders.yaml"), Backend: "orders", Namespace: "Orders"}

	ops := append(exposeAll(users, "/api"), exposeAll(orders, "/api")...)
	root, err := Merge(ops, MergeOptions{
		Title:   "Nina Gateway",
		Version: "1.0.0",
		Servers: []string{"https://api.example.com"},
		Dedupe:  dedupe,
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out, err := Render(root)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return root, out
}

func TestMergeProducesValidDocument(t *testing.T) {
	_, out := mergeFixtures(t, true)

	doc, err := libopenapi.NewDocument(out)
	if err != nil {
		t.Fatalf("merged document does not parse: %v\n%s", err, out)
	}
	model, errs := doc.BuildV3Model()
	if errs != nil {
		t.Fatalf("merged document is not a valid v3 model: %v\n%s", errs, out)
	}
	if model.Model.Version != "3.1.0" {
		t.Errorf("version = %q, want 3.1.0", model.Model.Version)
	}
	if got := model.Model.Info.Title; got != "Nina Gateway" {
		t.Errorf("title = %q", got)
	}
}

func TestMergeNamespacesPathsAndOperations(t *testing.T) {
	root, _ := mergeFixtures(t, true)
	paths := mapGet(root, "paths")

	want := []string{
		"/api/internal/debug",
		"/api/orders", "/api/orders/{id}",
		"/api/users", "/api/users/{id}",
	}
	got := mapKeys(paths)
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	op := mapGet(mapGet(paths, "/api/users/{id}"), "get")
	if op == nil {
		t.Fatal("GET /api/users/{id} missing")
	}
	if id := mapGet(op, "operationId"); id == nil || id.Value != "users_getUser" {
		t.Errorf("operationId = %v, want users_getUser", id)
	}
	// Provenance has to survive so operators can trace a published route back.
	for k, want := range map[string]string{
		"x-nina-backend":               "users",
		"x-nina-upstream-path":         "/users/{id}",
		"x-nina-upstream-operation-id": "getUser",
	} {
		if v := mapGet(op, k); v == nil || v.Value != want {
			t.Errorf("%s = %v, want %q", k, v, want)
		}
	}
}

func TestMergeNamespacesComponents(t *testing.T) {
	root, _ := mergeFixtures(t, false)
	schemas := mapGet(mapGet(root, "components"), "schemas")
	for _, name := range []string{
		"UsersUser", "UsersProfile", "UsersNewUser", "UsersError",
		"OrdersOrder", "OrdersOrderLine", "OrdersNewOrder", "OrdersError",
	} {
		if mapGet(schemas, name) == nil {
			t.Errorf("missing schema %s (have %v)", name, mapKeys(schemas))
		}
	}
	// Components reached only through another component must be pulled in too.
	if mapGet(schemas, "UsersProfile") == nil {
		t.Error("transitive component UsersProfile was not imported")
	}
	if params := mapGet(mapGet(root, "components"), "parameters"); mapGet(params, "UsersTraceId") == nil {
		t.Error("missing parameter component UsersTraceId")
	}
	if resps := mapGet(mapGet(root, "components"), "responses"); mapGet(resps, "UsersErrorResponse") == nil {
		t.Error("missing response component UsersErrorResponse")
	}
}

func TestMergeDedupesIdenticalComponents(t *testing.T) {
	root, _ := mergeFixtures(t, true)
	schemas := mapGet(mapGet(root, "components"), "schemas")

	// users.Error and orders.Error are structurally identical and share a base
	// name, so they collapse onto one shared component.
	if mapGet(schemas, "Error") == nil {
		t.Fatalf("expected deduped Error schema, have %v", mapKeys(schemas))
	}
	for _, gone := range []string{"UsersError", "OrdersError"} {
		if mapGet(schemas, gone) != nil {
			t.Errorf("%s should have been collapsed into Error", gone)
		}
	}
	walkRefs(root, func(ref *yaml.Node) {
		for _, gone := range []string{"UsersError", "OrdersError"} {
			if ref.Value == componentRef("schemas", gone) {
				t.Errorf("ref to collapsed component %s survives", gone)
			}
		}
	})

	// User and Order are NOT identical and must stay namespaced.
	if mapGet(schemas, "UsersUser") == nil || mapGet(schemas, "OrdersOrder") == nil {
		t.Error("structurally distinct components were wrongly collapsed")
	}
}

// TestMergeNoDanglingRefs is the invariant that matters most: every $ref in the
// published document must resolve inside it.
func TestMergeNoDanglingRefs(t *testing.T) {
	for _, dedupe := range []bool{false, true} {
		root, _ := mergeFixtures(t, dedupe)
		comps := mapGet(root, "components")
		var bad []string
		walkRefs(root, func(ref *yaml.Node) {
			section, name, _, ok := parseLocalRef(ref.Value)
			if !ok {
				bad = append(bad, ref.Value+" (not a local components pointer)")
				return
			}
			if mapGet(mapGet(comps, section), name) == nil {
				bad = append(bad, ref.Value)
			}
		})
		if len(bad) > 0 {
			t.Errorf("dedupe=%v: dangling refs: %v", dedupe, bad)
		}
	}
}

func TestMergeFoldsPathLevelParameters(t *testing.T) {
	spec := loadFixture(t, "pathparams.yaml")
	src := &SpecSource{Spec: spec, Backend: "svc", Namespace: "Svc"}
	root, err := Merge(exposeAll(src, "/svc"), MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// The path-level tenant parameter must appear on both operations.
	for _, method := range []string{"get", "delete"} {
		op := mapGet(mapGet(mapGet(root, "paths"), "/svc/things/{id}"), method)
		names := map[string]bool{}
		for _, p := range mapGet(op, "parameters").Content {
			if n := mapGet(p, "name"); n != nil {
				names[n.Value] = true
			}
		}
		if !names["tenant"] {
			t.Errorf("%s: path-level `tenant` parameter was not folded in", method)
		}
		if !names["id"] {
			t.Errorf("%s: path-level `id` parameter was not folded in", method)
		}
	}

	// GET overrides `id` at the operation level; the override must win and must
	// not be duplicated.
	get := mapGet(mapGet(mapGet(root, "paths"), "/svc/things/{id}"), "get")
	var idCount int
	var idDesc string
	for _, p := range mapGet(get, "parameters").Content {
		if n := mapGet(p, "name"); n != nil && n.Value == "id" {
			idCount++
			if d := mapGet(p, "description"); d != nil {
				idDesc = d.Value
			}
		}
	}
	if idCount != 1 {
		t.Errorf("id parameter appears %d times, want 1", idCount)
	}
	if idDesc != "operation-level override" {
		t.Errorf("operation-level parameter did not win, description = %q", idDesc)
	}
}

func TestMergeDetectsRouteCollision(t *testing.T) {
	a := &SpecSource{Spec: loadFixture(t, "users.yaml"), Backend: "a", Namespace: "A"}
	b := &SpecSource{Spec: loadFixture(t, "users.yaml"), Backend: "b", Namespace: "B"}

	// Both backends exposed under the same prefix: namespacing cannot save this.
	ops := append(exposeAll(a, "/x"), exposeAll(b, "/x")...)
	_, err := Merge(ops, MergeOptions{})
	if err == nil {
		t.Fatal("expected a collision error")
	}
	var ce *CollisionError
	if !asCollision(err, &ce) {
		t.Fatalf("error is not a CollisionError: %v", err)
	}
	if !strings.Contains(err.Error(), "a.") || !strings.Contains(err.Error(), "b.") {
		t.Errorf("collision error should name both claimants, got: %v", err)
	}
}

func asCollision(err error, target **CollisionError) bool {
	if ce, ok := err.(*CollisionError); ok {
		*target = ce
		return true
	}
	return false
}

func TestNormalise30(t *testing.T) {
	spec := loadFixture(t, "oas30quirks.yaml")
	if spec.Version != "3.0.3" {
		t.Fatalf("declared version = %q", spec.Version)
	}
	src := &SpecSource{Spec: spec, Backend: "q", Namespace: "Q"}
	root, err := Merge(exposeAll(src, ""), MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	schema := mapGet(mapGet(mapGet(root, "components"), "schemas"), "QThing")
	if schema == nil {
		t.Fatalf("QThing missing, have %v", mapKeys(mapGet(mapGet(root, "components"), "schemas")))
	}
	props := mapGet(schema, "properties")

	// nullable: true -> type: [string, "null"]
	nick := mapGet(props, "nickname")
	typ := mapGet(nick, "type")
	if typ == nil || typ.Kind != yaml.SequenceNode || len(typ.Content) != 2 ||
		typ.Content[0].Value != "string" || typ.Content[1].Value != "null" {
		got, _ := yaml.Marshal(nick)
		t.Errorf("nullable was not normalised: %s", got)
	}
	if mapGet(nick, "nullable") != nil {
		t.Error("`nullable` keyword should be removed in 3.1 output")
	}

	// exclusiveMinimum: true + minimum: 0 -> exclusiveMinimum: 0
	score := mapGet(props, "score")
	ex := mapGet(score, "exclusiveMinimum")
	if ex == nil || ex.Value != "0" {
		got, _ := yaml.Marshal(score)
		t.Errorf("exclusiveMinimum was not normalised: %s", got)
	}
	if mapGet(score, "minimum") != nil {
		t.Error("`minimum` should be consumed by the exclusiveMinimum rewrite")
	}
}
