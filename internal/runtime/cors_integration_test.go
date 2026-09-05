package runtime_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// corsConfig mounts both backends with a CORS middleware attached.
func corsConfig(users, orders *upstream) string {
	return fmt.Sprintf(`
version: 1
server:
  listen: ":0"
defaults:
  timeout: 5s
backends:
  - name: users
    spec: { url: %s/openapi.yaml }
    hosts: ["%s"]
  - name: orders
    spec: { url: %s/openapi.yaml }
    hosts: ["%s"]
middleware:
  web:
    type: cors
    allow_origins: ["https://app.example"]
    allow_headers: ["Content-Type", "Authorization"]
    max_age: 10m
  strict:
    type: validate
    request: enforce
expose:
  - backend: users
    prefix: /api
    exclude: ["internal*"]
    middleware: [web, strict]
  - backend: orders
    prefix: /api
    middleware: [web]
`, users.URL, users.URL, orders.URL, orders.URL)
}

// A preflight is a browser protocol detail, not an API operation: OpenAPI cannot
// describe it, so the router has to answer OPTIONS itself. Without a synthetic
// route the request dies with 405 before any CORS middleware runs.
func TestCORSPreflightIsAnswered(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, corsConfig(users, orders))
	defer cleanup()

	gw := httptest.NewServer(srv)
	defer gw.Close()

	req, _ := http.NewRequest(http.MethodOptions, gw.URL+"/api/users", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Max-Age = %q, want 600", got)
	}
	// The advertised methods must be the ones actually served, not a guess from
	// one operation.
	allow := resp.Header.Get("Access-Control-Allow-Methods")
	for _, m := range []string{"GET", "POST", "OPTIONS"} {
		if !strings.Contains(allow, m) {
			t.Errorf("Allow-Methods = %q, want it to include %s", allow, m)
		}
	}
	if strings.Contains(allow, "DELETE") {
		t.Errorf("Allow-Methods = %q advertises a method the gateway does not serve", allow)
	}
}

func TestCORSRejectsUnknownOrigin(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, corsConfig(users, orders))
	defer cleanup()

	gw := httptest.NewServer(srv)
	defer gw.Close()

	req, _ := http.NewRequest(http.MethodOptions, gw.URL+"/api/users", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Not an error: the headers are simply withheld and the browser enforces.
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("CORS headers were sent to a disallowed origin: %q", got)
	}
}

func TestCORSHeadersOnNormalRequest(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, corsConfig(users, orders))
	defer cleanup()

	gw := httptest.NewServer(srv)
	defer gw.Close()

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/orders", nil)
	req.Header.Set("Origin", "https://app.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("Allow-Origin = %q on a normal request", got)
	}
	// A non-wildcard origin must vary the cache.
	if !strings.Contains(resp.Header.Get("Vary"), "Origin") {
		t.Error("Vary: Origin is missing on an origin-specific response")
	}
}

// The synthetic preflight route exists only in the router. It must not appear in
// the published document, which describes API operations.
func TestPreflightIsNotPublished(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, corsConfig(users, orders))
	defer cleanup()

	rt := srv.Current()
	if strings.Contains(string(rt.Table.SpecJSON), `"options"`) {
		t.Error("a synthetic preflight route leaked into the published spec")
	}
	for _, r := range rt.Table.Routes {
		if r.Method == http.MethodOptions {
			t.Errorf("preflight route %s leaked into the route table", r)
		}
	}
}

// Preflight must not run request validation: the synthetic route has no upstream
// operation, so anything inspecting one would panic.
func TestPreflightSkipsValidation(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, corsConfig(users, orders))
	defer cleanup()

	gw := httptest.NewServer(srv)
	defer gw.Close()

	// /api/users declares a `limit` schema; a preflight carrying a bad value
	// must still succeed, because validation does not belong on this route.
	req, _ := http.NewRequest(http.MethodOptions, gw.URL+"/api/users?limit=99999", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", resp.StatusCode)
	}
}
