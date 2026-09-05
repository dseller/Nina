package apikey

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/reqctx"
	"github.com/rivencove/nina/internal/routetable"
)

func newInstance(t *testing.T, cfg string) chain.Instance {
	t.Helper()
	inst, err := New("keys", json.RawMessage(cfg), chain.Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return inst
}

// serve runs a request through the middleware and reports what the terminal
// handler saw.
func serve(t *testing.T, inst chain.Instance, r *http.Request) (*httptest.ResponseRecorder, *reqctx.Info, bool) {
	t.Helper()
	mw, err := inst.ForRoute(&routetable.Route{Method: "GET", GatewayPath: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	info := &reqctx.Info{}
	r = r.WithContext(reqctx.With(r.Context(), info))

	var reached bool
	var seen *http.Request
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		reached = true
		seen = req
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if seen != nil {
		r = seen
	}
	return rec, info, reached
}

const basicCfg = `{
  "type": "apikey",
  "in": "header",
  "name": "X-API-Key",
  "keys": {"acme": "secret-acme", "globex": "secret-globex"},
  "scopes": {"acme": ["read", "write"], "globex": ["read"]}
}`

func TestValidKeyResolvesConsumer(t *testing.T) {
	inst := newInstance(t, basicCfg)
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-API-Key", "secret-globex")

	rec, info, reached := serve(t, inst, req)
	if !reached {
		t.Fatalf("request was rejected: %d %s", rec.Code, rec.Body)
	}
	if info.Consumer != "globex" {
		t.Errorf("consumer = %q, want globex", info.Consumer)
	}
	if len(info.Scopes) != 1 || info.Scopes[0] != "read" {
		t.Errorf("scopes = %v, want [read]", info.Scopes)
	}
}

func TestMissingKeyIsUnauthorized(t *testing.T) {
	inst := newInstance(t, basicCfg)
	rec, _, reached := serve(t, inst, httptest.NewRequest("GET", "/x", nil))
	if reached {
		t.Fatal("a request with no key reached the handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Errorf("content type = %q", ct)
	}
}

func TestUnknownKeyIsUnauthorized(t *testing.T) {
	inst := newInstance(t, basicCfg)
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-API-Key", "not-a-real-key")
	rec, _, reached := serve(t, inst, req)
	if reached {
		t.Fatal("an unknown key was accepted")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	// The response must not reveal which consumers exist.
	if strings.Contains(rec.Body.String(), "acme") || strings.Contains(rec.Body.String(), "globex") {
		t.Errorf("rejection leaked consumer names: %s", rec.Body)
	}
}

// A near-miss must be rejected: prefix matching would be catastrophic.
func TestPartialKeyIsRejected(t *testing.T) {
	inst := newInstance(t, basicCfg)
	for _, key := range []string{"secret-acm", "secret-acmex", "secret", ""} {
		req := httptest.NewRequest("GET", "/x", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		_, _, reached := serve(t, inst, req)
		if reached {
			t.Errorf("key %q was accepted", key)
		}
	}
}

func TestInsufficientScopeIsForbidden(t *testing.T) {
	cfg := `{
	  "type": "apikey",
	  "keys": {"acme": "k1", "globex": "k2"},
	  "scopes": {"acme": ["read", "write"], "globex": ["read"]},
	  "required_scopes": ["write"]
	}`
	inst := newInstance(t, cfg)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-API-Key", "k2") // globex has read only
	rec, _, reached := serve(t, inst, req)
	if reached {
		t.Fatal("a consumer without the required scope was let through")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (authenticated but not authorised)", rec.Code)
	}

	req2 := httptest.NewRequest("GET", "/x", nil)
	req2.Header.Set("X-API-Key", "k1") // acme has write
	if _, _, ok := serve(t, inst, req2); !ok {
		t.Error("a consumer with the required scope was rejected")
	}
}

// The gateway consumes the credential; an upstream must not be able to replay it.
func TestCredentialIsStrippedByDefault(t *testing.T) {
	inst := newInstance(t, basicCfg)
	mw, err := inst.ForRoute(&routetable.Route{Method: "GET", GatewayPath: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-API-Key", "secret-acme")
	req = req.WithContext(reqctx.With(req.Context(), &reqctx.Info{}))

	var upstreamSaw string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamSaw = r.Header.Get("X-API-Key")
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if upstreamSaw != "" {
		t.Errorf("the API key reached the upstream: %q", upstreamSaw)
	}
}

func TestQueryKeyIsStripped(t *testing.T) {
	cfg := `{"type":"apikey","in":"query","name":"api_key","keys":{"acme":"k1"}}`
	inst := newInstance(t, cfg)
	mw, _ := inst.ForRoute(&routetable.Route{Method: "GET", GatewayPath: "/x"})

	req := httptest.NewRequest("GET", "/x?api_key=k1&keep=yes", nil)
	req = req.WithContext(reqctx.With(req.Context(), &reqctx.Info{}))

	var query string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(query, "k1") {
		t.Errorf("the API key survived in the query string: %q", query)
	}
	if !strings.Contains(query, "keep=yes") {
		t.Errorf("an unrelated query parameter was dropped: %q", query)
	}
}

func TestForwardConsumerHeader(t *testing.T) {
	cfg := `{"type":"apikey","keys":{"acme":"k1"},"forward_consumer_header":"X-Consumer"}`
	inst := newInstance(t, cfg)
	mw, _ := inst.ForRoute(&routetable.Route{Method: "GET", GatewayPath: "/x"})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-API-Key", "k1")
	req = req.WithContext(reqctx.With(req.Context(), &reqctx.Info{}))

	var got string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Consumer")
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "acme" {
		t.Errorf("X-Consumer = %q, want acme", got)
	}
}

// Two consumers sharing a key would make the resolved identity ambiguous, which
// silently breaks per-consumer rate limits and audit logs.
func TestSharedKeyIsRejectedAtBuild(t *testing.T) {
	cfg := `{"type":"apikey","keys":{"acme":"same","globex":"same"}}`
	_, err := New("keys", json.RawMessage(cfg), chain.Deps{})
	if err == nil || !strings.Contains(err.Error(), "share the same key") {
		t.Errorf("expected a shared-key error, got %v", err)
	}
}

func TestEmptyKeyIsRejectedAtBuild(t *testing.T) {
	cfg := `{"type":"apikey","keys":{"acme":""}}`
	if _, err := New("keys", json.RawMessage(cfg), chain.Deps{}); err == nil {
		t.Error("an empty key should be refused at build time")
	}
}

func TestNoKeysIsRejectedAtBuild(t *testing.T) {
	if _, err := New("keys", json.RawMessage(`{"type":"apikey"}`), chain.Deps{}); err == nil {
		t.Error("a key middleware with no keys should be refused")
	}
}

func TestSecuritySchemeIsPublished(t *testing.T) {
	inst := newInstance(t, basicCfg)
	sc, ok := inst.(chain.SecurityContributor)
	if !ok {
		t.Fatal("apikey should contribute a security scheme")
	}
	node := sc.SecurityScheme()
	var found bool
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "type" && node.Content[i+1].Value == "apiKey" {
			found = true
		}
	}
	if !found {
		t.Error("published scheme should declare type: apiKey")
	}
}
