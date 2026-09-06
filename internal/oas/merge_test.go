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

// TestMergePublishesSecuritySchemes covers the invariant that makes the
// published document usable in a documentation viewer: every scheme an
// operation requires must be described in components.securitySchemes, or the
// viewer shows a locked operation with no way to supply a credential.
func TestMergePublishesSecuritySchemes(t *testing.T) {
	root, _ := mergeFixtures(t, false)
	paths := mapGet(root, "paths")
	schemes := mapGet(mapGet(root, "components"), "securitySchemes")

	for _, name := range []string{"UsersBearerAuth", "UsersApiKey", "OrdersBearerAuth"} {
		if mapGet(schemes, name) == nil {
			t.Errorf("missing security scheme %s (have %v)", name, mapKeys(schemes))
		}
	}
	if bearer := mapGet(schemes, "UsersBearerAuth"); bearer != nil {
		if v := mapGet(bearer, "scheme"); v == nil || v.Value != "bearer" {
			t.Errorf("UsersBearerAuth scheme = %v, want bearer", v)
		}
	}

	// An operation that declares nothing inherits the source document's
	// requirement; the gateway's own top-level security describes the gateway.
	list := mapGet(mapGet(paths, "/api/users"), "get")
	if got := securityNames(mapGet(list, "security")); len(got) != 1 || got[0] != "UsersBearerAuth" {
		t.Errorf("listUsers security = %v, want [UsersBearerAuth]", got)
	}

	// An operation-level requirement wins, keeps its alternatives in order, and
	// keeps the empty alternative that means "authentication is optional".
	get := mapGet(mapGet(paths, "/api/users/{id}"), "get")
	sec := mapGet(get, "security")
	if sec == nil || len(sec.Content) != 2 {
		t.Fatalf("getUser security = %v, want 2 alternatives", securityNames(sec))
	}
	first := mapKeys(sec.Content[0])
	if len(first) != 2 || first[0] != "UsersBearerAuth" || first[1] != "UsersApiKey" {
		t.Errorf("getUser first alternative = %v, want [UsersBearerAuth UsersApiKey]", first)
	}
	if len(sec.Content[1].Content) != 0 {
		t.Errorf("the empty alternative must survive, got %v", mapKeys(sec.Content[1]))
	}
}

// TestMergeDropsUndeclaredSecuritySchemes: an upstream naming a scheme it never
// declared must not leave a requirement pointing at nothing. Dropping it is the
// only honest option, and it must not degrade into `{}`, which would claim the
// operation needs no authentication at all.
func TestMergeDropsUndeclaredSecuritySchemes(t *testing.T) {
	root, _ := mergeFixtures(t, false)
	op := mapGet(mapGet(mapGet(root, "paths"), "/api/internal/debug"), "get")
	if op == nil {
		t.Fatal("GET /api/internal/debug missing")
	}
	if sec := mapGet(op, "security"); sec != nil {
		t.Errorf("security = %v, want the key to be dropped entirely", securityNames(sec))
	}
}

// TestMergeDedupeRenamesSecurityRequirements: requirements name schemes rather
// than $ref-ing them, so the dedupe pass's ref rewrite cannot reach them.
func TestMergeDedupeRenamesSecurityRequirements(t *testing.T) {
	root, _ := mergeFixtures(t, true)
	schemes := mapGet(mapGet(root, "components"), "securitySchemes")

	// The two backends' bearerAuth are identical, so they collapse.
	if mapGet(schemes, "BearerAuth") == nil {
		t.Fatalf("expected a deduped BearerAuth, have %v", mapKeys(schemes))
	}
	for _, gone := range []string{"UsersBearerAuth", "OrdersBearerAuth"} {
		if mapGet(schemes, gone) != nil {
			t.Errorf("%s should have collapsed into BearerAuth", gone)
		}
	}

	for _, tc := range []struct{ path, want string }{
		{"/api/users", "BearerAuth"},
		{"/api/orders", "BearerAuth"},
	} {
		op := mapGet(mapGet(mapGet(root, "paths"), tc.path), "get")
		got := securityNames(mapGet(op, "security"))
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("GET %s security = %v, want [%s]", tc.path, got, tc.want)
		}
	}
}

// TestMergeNoDanglingSecurityRequirements is the security-scheme counterpart of
// TestMergeNoDanglingRefs.
func TestMergeNoDanglingSecurityRequirements(t *testing.T) {
	for _, dedupe := range []bool{false, true} {
		root, _ := mergeFixtures(t, dedupe)
		schemes := mapGet(mapGet(root, "components"), "securitySchemes")
		var bad []string
		for _, item := range mapGet(root, "paths").Content {
			for _, op := range item.Content {
				if op.Kind != yaml.MappingNode {
					continue
				}
				for _, name := range securityNames(mapGet(op, "security")) {
					if mapGet(schemes, name) == nil {
						bad = append(bad, name)
					}
				}
			}
		}
		if len(bad) > 0 {
			t.Errorf("dedupe=%v: requirements naming undeclared schemes: %v", dedupe, bad)
		}
	}
}

// securityNames flattens a `security` node to the scheme names it references.
func securityNames(sec *yaml.Node) []string {
	if sec == nil {
		return nil
	}
	var out []string
	for _, req := range sec.Content {
		out = append(out, mapKeys(req)...)
	}
	return out
}

// TestMergeHonoursChosenSchemeNames: the published name is what a documentation
// viewer labels its credential box with, so a configured name must survive
// verbatim — unprefixed, and untouched by the dedupe pass.
func TestMergeHonoursChosenSchemeNames(t *testing.T) {
	users := &SpecSource{
		Spec: loadFixture(t, "users.yaml"), Backend: "users", Namespace: "Users",
		SchemeNames: map[string]string{"bearerAuth": "Bearer"},
	}
	orders := &SpecSource{Spec: loadFixture(t, "orders.yaml"), Backend: "orders", Namespace: "Orders"}

	root, err := Merge(append(exposeAll(users, "/api"), exposeAll(orders, "/api")...), MergeOptions{Dedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	schemes := mapGet(mapGet(root, "components"), "securitySchemes")

	if mapGet(schemes, "Bearer") == nil {
		t.Fatalf("chosen name Bearer was not published, have %v", mapKeys(schemes))
	}
	if mapGet(schemes, "UsersBearerAuth") != nil {
		t.Error("the derived name should have been replaced, not published alongside")
	}
	// Only the chosen scheme is exempt; the backend's other schemes still derive.
	if mapGet(schemes, "UsersApiKey") == nil {
		t.Errorf("unchosen schemes should still be namespaced, have %v", mapKeys(schemes))
	}
	// The orders backend chose nothing, so its identical bearerAuth must not be
	// dragged onto the chosen name behind the operator's back.
	if mapGet(schemes, "OrdersBearerAuth") == nil {
		t.Errorf("orders' scheme should still be namespaced, have %v", mapKeys(schemes))
	}

	list := mapGet(mapGet(mapGet(root, "paths"), "/api/users"), "get")
	if got := securityNames(mapGet(list, "security")); len(got) != 1 || got[0] != "Bearer" {
		t.Errorf("listUsers security = %v, want [Bearer]", got)
	}
}

// TestMergeSharesOneChosenSchemeName: two backends choosing one name for the
// same credential is how an operator says "these really are the same token".
func TestMergeSharesOneChosenSchemeName(t *testing.T) {
	users := &SpecSource{
		Spec: loadFixture(t, "users.yaml"), Backend: "users", Namespace: "Users",
		SchemeNames: map[string]string{"bearerAuth": "Bearer"},
	}
	orders := &SpecSource{
		Spec: loadFixture(t, "orders.yaml"), Backend: "orders", Namespace: "Orders",
		SchemeNames: map[string]string{"bearerAuth": "Bearer"},
	}

	root, err := Merge(append(exposeAll(users, "/api"), exposeAll(orders, "/api")...), MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	schemes := mapGet(mapGet(root, "components"), "securitySchemes")
	if mapGet(schemes, "Bearer") == nil || mapGet(schemes, "Bearer2") != nil {
		t.Fatalf("expected one shared Bearer, have %v", mapKeys(schemes))
	}
	for _, p := range []string{"/api/users", "/api/orders"} {
		op := mapGet(mapGet(mapGet(root, "paths"), p), "get")
		got := securityNames(mapGet(op, "security"))
		if len(got) != 1 || got[0] != "Bearer" {
			t.Errorf("GET %s security = %v, want [Bearer]", p, got)
		}
	}
}

// TestMergeRejectsConflictingSchemeName: sharing a name is only sound when the
// schemes agree. Publishing one of two different credentials under a single name
// would misdescribe how to call half the gateway.
func TestMergeRejectsConflictingSchemeName(t *testing.T) {
	users := &SpecSource{
		Spec: loadFixture(t, "users.yaml"), Backend: "users", Namespace: "Users",
		SchemeNames: map[string]string{"bearerAuth": "Auth"},
	}
	orders := &SpecSource{
		Spec: loadFixture(t, "users.yaml"), Backend: "orders", Namespace: "Orders",
		// apiKey is a different shape from users' bearerAuth.
		SchemeNames: map[string]string{"apiKey": "Auth"},
	}

	_, err := Merge(append(exposeAll(users, "/a"), exposeAll(orders, "/b")...), MergeOptions{})
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	for _, want := range []string{"Auth", "users", "orders"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}
