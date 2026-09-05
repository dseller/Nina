package router

import (
	"testing"
)

func mustAdd(t *testing.T, r *Router[string], method, tmpl string) {
	t.Helper()
	if err := r.Add(method, tmpl, method+" "+tmpl); err != nil {
		t.Fatalf("add %s %s: %v", method, tmpl, err)
	}
}

func TestMatch(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/users")
	mustAdd(t, r, "POST", "/users")
	mustAdd(t, r, "GET", "/users/{id}")
	mustAdd(t, r, "GET", "/users/{id}/orders/{orderId}")
	mustAdd(t, r, "GET", "/users/me")
	mustAdd(t, r, "GET", "/")

	tests := []struct {
		method, path string
		want         string
		params       map[string]string
	}{
		{"GET", "/users", "GET /users", nil},
		{"POST", "/users", "POST /users", nil},
		{"GET", "/users/", "GET /users", nil},
		{"GET", "/users/123", "GET /users/{id}", map[string]string{"id": "123"}},
		{"GET", "/users/me", "GET /users/me", nil},
		{"GET", "/users/7/orders/9", "GET /users/{id}/orders/{orderId}",
			map[string]string{"id": "7", "orderId": "9"}},
		{"GET", "/", "GET /", nil},
	}
	for _, tc := range tests {
		got := r.Match(tc.method, tc.path)
		if !got.Found {
			t.Errorf("%s %s: no match", tc.method, tc.path)
			continue
		}
		if got.Payload != tc.want {
			t.Errorf("%s %s: payload = %q, want %q", tc.method, tc.path, got.Payload, tc.want)
		}
		for k, v := range tc.params {
			if gv, ok := got.Params.Get(k); !ok || gv != v {
				t.Errorf("%s %s: param %s = %q (ok=%v), want %q", tc.method, tc.path, k, gv, ok, v)
			}
		}
	}
}

// A static segment must win over a parameter at the same position, but a static
// branch that dead-ends must fall back to the parameter branch rather than 404.
func TestStaticBeatsParamWithBacktrack(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/users/me/settings")
	mustAdd(t, r, "GET", "/users/{id}")
	mustAdd(t, r, "GET", "/users/{id}/posts")

	if got := r.Match("GET", "/users/me/settings"); !got.Found || got.Payload != "GET /users/me/settings" {
		t.Errorf("static route lost: %+v", got)
	}
	// "me" enters the static branch, which has no /posts child; the walk has to
	// back out and re-try "me" as {id}.
	got := r.Match("GET", "/users/me/posts")
	if !got.Found || got.Payload != "GET /users/{id}/posts" {
		t.Fatalf("backtrack failed: %+v", got)
	}
	if v, _ := got.Params.Get("id"); v != "me" {
		t.Errorf("id = %q, want me", v)
	}
	// And "me" alone must fall through to {id} too, since /users/me was never
	// registered as a terminal route here.
	if got := r.Match("GET", "/users/me"); !got.Found || got.Payload != "GET /users/{id}" {
		t.Errorf("terminal backtrack failed: %+v", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/users")
	mustAdd(t, r, "DELETE", "/users")

	got := r.Match("POST", "/users")
	if got.Found {
		t.Fatal("POST should not match")
	}
	if !got.PathMatched {
		t.Fatal("path should be reported as matched so the caller can send 405")
	}
	want := []string{"DELETE", "GET"}
	if len(got.Allow) != len(want) {
		t.Fatalf("Allow = %v, want %v", got.Allow, want)
	}
	for i := range want {
		if got.Allow[i] != want[i] {
			t.Errorf("Allow = %v, want %v", got.Allow, want)
		}
	}
}

func TestNoMatch(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/users/{id}")

	for _, p := range []string{"/orders", "/users/1/2", "/"} {
		if got := r.Match("GET", p); got.Found || got.PathMatched {
			t.Errorf("%s should not match, got %+v", p, got)
		}
	}
}

// An empty segment must not be captured as a parameter value; //  is not a
// valid id.
func TestEmptySegmentDoesNotMatchParam(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/users/{id}")
	if got := r.Match("GET", "/users//"); got.Found {
		t.Errorf("empty segment matched {id} as %+v", got)
	}
}

func TestPercentEncodedParam(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/files/{name}")

	// %2F inside a value must not split the segment, and the value handed to the
	// caller must be decoded.
	got := r.Match("GET", "/files/a%2Fb")
	if !got.Found {
		t.Fatal("no match")
	}
	if v, _ := got.Params.Get("name"); v != "a/b" {
		t.Errorf("name = %q, want a/b", v)
	}
}

func TestHeadFallsBackToGet(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/users")
	got := r.Match("HEAD", "/users")
	if !got.Found || got.Payload != "GET /users" {
		t.Errorf("HEAD should fall back to GET, got %+v", got)
	}
}

func TestDuplicateRouteIsAnError(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/users/{id}")
	// Same shape, different parameter spelling: the published document could not
	// represent this, so it has to be rejected.
	if err := r.Add("GET", "/users/{userId}", "x"); err == nil {
		t.Fatal("expected a conflict for a re-spelled parameter")
	}
	if err := r.Add("GET", "/users/{id}", "x"); err == nil {
		t.Fatal("expected a conflict for an exact duplicate")
	}
}

func BenchmarkMatch(b *testing.B) {
	r := New[string]()
	for _, tmpl := range []string{
		"/users", "/users/{id}", "/users/{id}/orders", "/users/{id}/orders/{orderId}",
		"/orders", "/orders/{id}", "/products/{sku}/reviews/{reviewId}", "/health",
	} {
		_ = r.Add("GET", tmpl, tmpl)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := r.Match("GET", "/users/12345/orders/67890"); !got.Found {
			b.Fatal("no match")
		}
	}
}

// Nested-resource generators (DRF routers, among others) routinely emit
// /enums/{id}/ alongside /enums/{enum_pk}/values/. Those are different paths, so
// both are legal OpenAPI and both must register and match, each seeing its own
// parameter name.
func TestDifferentParamNamesAtTheSamePosition(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/enums/{id}/")
	mustAdd(t, r, "DELETE", "/enums/{id}/")
	mustAdd(t, r, "GET", "/enums/{enum_pk}/values/")
	mustAdd(t, r, "POST", "/enums/{enum_pk}/values/")
	mustAdd(t, r, "GET", "/enums/{enum_pk}/values/{value_id}/")

	got := r.Match("GET", "/enums/42/")
	if !got.Found || got.Payload != "GET /enums/{id}/" {
		t.Fatalf("terminal route: %+v", got)
	}
	if v, ok := got.Params.Get("id"); !ok || v != "42" {
		t.Errorf("id = %q (ok=%v), want 42", v, ok)
	}
	if _, ok := got.Params.Get("enum_pk"); ok {
		t.Error("the sibling template's parameter name leaked into this match")
	}

	got = r.Match("GET", "/enums/42/values/")
	if !got.Found || got.Payload != "GET /enums/{enum_pk}/values/" {
		t.Fatalf("nested route: %+v", got)
	}
	if v, ok := got.Params.Get("enum_pk"); !ok || v != "42" {
		t.Errorf("enum_pk = %q (ok=%v), want 42", v, ok)
	}

	got = r.Match("GET", "/enums/42/values/7/")
	if !got.Found {
		t.Fatalf("deep nested route: %+v", got)
	}
	pk, _ := got.Params.Get("enum_pk")
	vid, _ := got.Params.Get("value_id")
	if pk != "42" || vid != "7" {
		t.Errorf("params = %v, want enum_pk=42 value_id=7", got.Params)
	}
}

// Two spellings of the same shape for one method are still a genuine duplicate.
func TestSameShapeDifferentSpellingStillConflicts(t *testing.T) {
	r := New[string]()
	mustAdd(t, r, "GET", "/users/{id}")
	if err := r.Add("GET", "/users/{userId}", "x"); err == nil {
		t.Fatal("two spellings of the same shape should still conflict")
	}
	// A different method at the same position may spell it differently, and each
	// must see its own name.
	if err := r.Add("POST", "/users/{userId}", "POST /users/{userId}"); err != nil {
		t.Fatalf("a different method should be allowed to spell it differently: %v", err)
	}
	if got := r.Match("GET", "/users/9"); got.Found {
		if v, _ := got.Params.Get("id"); v != "9" {
			t.Errorf("GET param id = %q", v)
		}
	}
	got := r.Match("POST", "/users/9")
	if !got.Found {
		t.Fatal("POST did not match")
	}
	if v, _ := got.Params.Get("userId"); v != "9" {
		t.Errorf("POST param userId = %q, want 9", v)
	}
}
