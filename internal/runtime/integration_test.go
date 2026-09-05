package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/mw/cors"
	"github.com/rivencove/nina/internal/mw/headers"
	"github.com/rivencove/nina/internal/mw/ratelimit"
	"github.com/rivencove/nina/internal/mw/validate"
	"github.com/rivencove/nina/internal/observ"
	"github.com/rivencove/nina/internal/runtime"
	"github.com/rivencove/nina/internal/specsrc"
	yaml "go.yaml.in/yaml/v4"
)

const usersSpec = `
openapi: 3.0.3
info: { title: Users, version: 1.0.0 }
paths:
  /users:
    get:
      operationId: listUsers
      parameters:
        - name: limit
          in: query
          schema: { type: integer, minimum: 1, maximum: 100 }
      responses:
        '200': { description: ok }
    post:
      operationId: createUser
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/NewUser' }
      responses:
        '201': { description: created }
  /users/{id}:
    get:
      operationId: getUser
      parameters:
        - name: id
          in: path
          required: true
          schema: { type: string, minLength: 2 }
      responses:
        '200': { description: ok }
  /internal/dump:
    get:
      operationId: internalDump
      responses:
        '200': { description: ok }
components:
  schemas:
    NewUser:
      type: object
      required: [email]
      properties:
        email: { type: string, minLength: 3 }
        age: { type: integer, minimum: 0 }
`

const ordersSpec = `
openapi: 3.1.0
info: { title: Orders, version: 1.0.0 }
paths:
  /orders:
    get:
      operationId: listOrders
      responses:
        '200': { description: ok }
`

// upstream is a stub backend that serves its own spec and echoes requests.
type upstream struct {
	*httptest.Server
	hits atomic.Int64
	// lastPath records what the gateway actually asked for.
	lastPath atomic.Value
}

func newUpstream(t *testing.T, spec string) *upstream {
	t.Helper()
	u := &upstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = io.WriteString(w, spec)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.lastPath.Store(r.URL.RequestURI())
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":   r.URL.Path,
			"query":  r.URL.RawQuery,
			"method": r.Method,
			"body":   string(body),
			"xff":    r.Header.Get("X-Forwarded-For"),
		})
	})
	u.Server = httptest.NewServer(mux)
	t.Cleanup(u.Close)
	return u
}

func testRegistry() *chain.Registry {
	r := chain.NewRegistry()
	r.Register("validate", validate.New)
	r.Register("cors", cors.New)
	r.Register("headers", headers.New)
	r.Register("ratelimit", ratelimit.New)
	return r
}

func buildGateway(t *testing.T, cfgYAML string) (*runtime.Server, func()) {
	t.Helper()
	srv, err := tryBuildGateway(t, cfgYAML)
	if err != nil {
		t.Fatalf("build gateway: %v", err)
	}
	return srv, func() {
		if rt := srv.Current(); rt != nil {
			rt.Retire(0)
		}
	}
}

// tryBuildGateway returns the build error instead of failing, for tests that
// assert a configuration is rejected.
func tryBuildGateway(t *testing.T, cfgYAML string) (*runtime.Server, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nina.yaml")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := runtime.Deps{
		Logger:   log,
		Metrics:  observ.New(prometheus.NewRegistry(), "test"),
		Fetcher:  specsrc.New(filepath.Join(dir, "cache")),
		Registry: testRegistry(),
	}
	return runtime.NewServer(context.Background(), cfg, deps, runtime.ServerOptions{
		ConfigPath: path, DrainGrace: time.Second,
	})
}

func twoBackendConfig(users, orders *upstream) string {
	return fmt.Sprintf(`
version: 1
server:
  listen: ":0"
spec:
  title: Test Gateway
  version: 9.9.9
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
  strict:
    type: validate
    request: enforce
expose:
  - backend: users
    prefix: /api
    exclude: ["internal*"]
    middleware: [strict]
  - backend: orders
    prefix: /api
`, users.URL, users.URL, orders.URL, orders.URL)
}

func TestGatewayEndToEnd(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, twoBackendConfig(users, orders))
	defer cleanup()

	gw := httptest.NewServer(srv)
	defer gw.Close()

	t.Run("proxies to the right upstream and path", func(t *testing.T) {
		resp, err := http.Get(gw.URL + "/api/users/abc")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		// The /api prefix is a gateway concern and must be stripped before the
		// upstream sees the request.
		if got["path"] != "/users/abc" {
			t.Errorf("upstream saw path %q, want /users/abc", got["path"])
		}
		if got["xff"] == "" {
			t.Error("X-Forwarded-For was not set")
		}
	})

	t.Run("routes each prefix to its own backend", func(t *testing.T) {
		before := orders.hits.Load()
		resp, err := http.Get(gw.URL + "/api/orders")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if orders.hits.Load() != before+1 {
			t.Error("request did not reach the orders backend")
		}
	})

	t.Run("query string is forwarded", func(t *testing.T) {
		resp, err := http.Get(gw.URL + "/api/users?limit=5")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		if got["query"] != "limit=5" {
			t.Errorf("upstream saw query %q, want limit=5", got["query"])
		}
	})

	t.Run("excluded operations are not served", func(t *testing.T) {
		resp, err := http.Get(gw.URL + "/api/internal/dump")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status %d, want 404 for an excluded operation", resp.StatusCode)
		}
	})

	t.Run("unknown path is 404", func(t *testing.T) {
		resp, err := http.Get(gw.URL + "/api/nope")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status %d, want 404", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Errorf("content type %q, want a problem document", ct)
		}
	})

	t.Run("wrong method is 405 with Allow", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, gw.URL+"/api/users", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status %d, want 405", resp.StatusCode)
		}
		allow := resp.Header.Get("Allow")
		if !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
			t.Errorf("Allow = %q, want GET and POST", allow)
		}
	})
}

func TestGatewayValidatesRequests(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, twoBackendConfig(users, orders))
	defer cleanup()

	gw := httptest.NewServer(srv)
	defer gw.Close()

	t.Run("rejects an out-of-range query parameter", func(t *testing.T) {
		before := users.hits.Load()
		resp, err := http.Get(gw.URL + "/api/users?limit=500")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", resp.StatusCode)
		}
		if users.hits.Load() != before {
			t.Error("an invalid request was forwarded upstream")
		}
		var p struct {
			Errors []struct{ In, Name, Message string } `json:"errors"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&p)
		if len(p.Errors) == 0 || p.Errors[0].Name != "limit" {
			t.Errorf("expected a field error naming limit, got %+v", p.Errors)
		}
	})

	t.Run("rejects a non-numeric query parameter", func(t *testing.T) {
		resp, err := http.Get(gw.URL + "/api/users?limit=abc")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d, want 400", resp.StatusCode)
		}
	})

	t.Run("accepts a valid query parameter", func(t *testing.T) {
		resp, err := http.Get(gw.URL + "/api/users?limit=50")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("status %d, want 200", resp.StatusCode)
		}
	})

	t.Run("rejects a path parameter that fails its schema", func(t *testing.T) {
		// minLength is 2, so a single character must be refused.
		resp, err := http.Get(gw.URL + "/api/users/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d, want 400", resp.StatusCode)
		}
	})

	t.Run("rejects a body missing a required field", func(t *testing.T) {
		resp, err := http.Post(gw.URL+"/api/users", "application/json", strings.NewReader(`{"age":3}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status %d, want 400: %s", resp.StatusCode, b)
		}
	})

	t.Run("forwards a valid body intact", func(t *testing.T) {
		payload := `{"email":"a@b.c","age":30}`
		resp, err := http.Post(gw.URL+"/api/users", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		// The body was buffered for validation; it must still arrive upstream.
		if got["body"] != payload {
			t.Errorf("upstream received body %q, want %q", got["body"], payload)
		}
	})
}

// TestPublishedSpecMatchesServedRoutes is the invariant the whole design exists
// to guarantee: the set of routes the router will match is exactly the set of
// operations the published document advertises.
func TestPublishedSpecMatchesServedRoutes(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, twoBackendConfig(users, orders))
	defer cleanup()

	rt := srv.Current()

	// What the gateway serves.
	served := map[string]bool{}
	for _, r := range rt.Table.Routes {
		served[r.Method+" "+r.GatewayPath] = true
	}

	// What the document advertises.
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(rt.Table.SpecYAML, &doc); err != nil {
		t.Fatalf("published spec does not parse: %v", err)
	}
	published := map[string]bool{}
	for path, item := range doc.Paths {
		for method := range item {
			published[strings.ToUpper(method)+" "+path] = true
		}
	}

	for k := range served {
		if !published[k] {
			t.Errorf("route %s is served but not published", k)
		}
	}
	for k := range published {
		if !served[k] {
			t.Errorf("route %s is published but not served", k)
		}
	}
	if len(served) == 0 {
		t.Fatal("no routes were built")
	}
	t.Logf("verified %d routes match between router and published document", len(served))

	// And every published route must actually resolve in the router.
	for _, r := range rt.Table.Routes {
		concrete := strings.ReplaceAll(r.GatewayPath, "{id}", "sample")
		if m := rt.Router.Match(r.Method, concrete); !m.Found {
			t.Errorf("published route %s does not match its own path %s", r, concrete)
		}
	}
}

func TestReloadFailureKeepsRunningConfig(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)

	dir := t.TempDir()
	path := filepath.Join(dir, "nina.yaml")
	good := twoBackendConfig(users, orders)
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := runtime.NewServer(context.Background(), cfg, runtime.Deps{
		Logger:   log,
		Metrics:  observ.New(prometheus.NewRegistry(), "test"),
		Fetcher:  specsrc.New(filepath.Join(dir, "cache")),
		Registry: testRegistry(),
	}, runtime.ServerOptions{ConfigPath: path, DrainGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Current().Retire(0)

	gw := httptest.NewServer(srv)
	defer gw.Close()

	before := srv.Current()
	routesBefore := len(before.Table.Routes)

	// Break the config and reload.
	if err := os.WriteFile(path, []byte("version: 1\nthis is not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.Reload(context.Background(), "test"); err == nil {
		t.Fatal("expected the reload to fail")
	}

	if srv.Current() != before {
		t.Error("a failed reload replaced the running runtime")
	}
	if len(srv.Current().Table.Routes) != routesBefore {
		t.Error("route table changed after a failed reload")
	}
	// And traffic still flows.
	resp, err := http.Get(gw.URL + "/api/orders")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("gateway stopped serving after a failed reload: status %d", resp.StatusCode)
	}
}

func TestReloadSwapsRoutesWithoutDroppingRequests(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)

	dir := t.TempDir()
	path := filepath.Join(dir, "nina.yaml")
	if err := os.WriteFile(path, []byte(twoBackendConfig(users, orders)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := runtime.NewServer(context.Background(), cfg, runtime.Deps{
		Logger:   log,
		Metrics:  observ.New(prometheus.NewRegistry(), "test"),
		Fetcher:  specsrc.New(filepath.Join(dir, "cache")),
		Registry: testRegistry(),
	}, runtime.ServerOptions{ConfigPath: path, DrainGrace: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Current().Retire(0)

	gw := httptest.NewServer(srv)
	defer gw.Close()

	// Hammer the gateway while reloading repeatedly.
	stop := make(chan struct{})
	var failures atomic.Int64
	var requests atomic.Int64
	for i := 0; i < 8; i++ {
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := http.Get(gw.URL + "/api/orders")
				requests.Add(1)
				if err != nil {
					failures.Add(1)
					continue
				}
				if resp.StatusCode != 200 {
					failures.Add(1)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	for i := 0; i < 5; i++ {
		if err := srv.Reload(context.Background(), "test reload"); err != nil {
			t.Fatalf("reload %d failed: %v", i, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	close(stop)
	time.Sleep(100 * time.Millisecond)

	if requests.Load() == 0 {
		t.Fatal("no requests were made")
	}
	if f := failures.Load(); f > 0 {
		t.Errorf("%d of %d requests failed across %d reloads", f, requests.Load(), 5)
	}
	t.Logf("%d requests survived 5 live reloads with no failures", requests.Load())
}
