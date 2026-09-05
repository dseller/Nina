package runtime_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tlsUpstream serves its own OpenAPI document and its API over TLS with an
// untrusted certificate, which is the situation `tls.insecure_skip_verify`
// exists for.
func newTLSUpstream(t *testing.T, spec string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = io.WriteString(w, spec)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path, "tls": r.TLS != nil})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func tlsConfig(u *httptest.Server, insecure bool) string {
	tlsBlock := ""
	if insecure {
		tlsBlock = "    tls: { insecure_skip_verify: true }\n"
	}
	return fmt.Sprintf(`
version: 1
server:
  listen: ":0"
defaults:
  timeout: 5s
backends:
  - name: secure
    spec: { url: %s/openapi.yaml }
    hosts: ["%s"]
%sexpose:
  - backend: secure
    prefix: /api
    exclude: ["internal*"]
`, u.URL, u.URL, tlsBlock)
}

// Without the flag the gateway cannot even fetch the document, so the runtime
// fails to build. That failure is the protection working.
func TestTLSUpstreamFailsWithoutTheFlag(t *testing.T) {
	up := newTLSUpstream(t, usersSpec)
	_, err := tryBuildGateway(t, tlsConfig(up, false))
	if err == nil {
		t.Fatal("an untrusted upstream certificate was accepted by default")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Errorf("error should point at the certificate, got: %v", err)
	}
}

// With it, both the spec fetch and the proxied traffic work over TLS.
func TestTLSUpstreamWorksWithInsecureSkipVerify(t *testing.T) {
	up := newTLSUpstream(t, usersSpec)
	srv, cleanup := buildGateway(t, tlsConfig(up, true))
	defer cleanup()

	rt := srv.Current()
	if len(rt.Table.Routes) == 0 {
		t.Fatal("no routes were built, so the spec was not fetched over TLS")
	}

	gw := httptest.NewServer(srv)
	defer gw.Close()

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
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["tls"] != true {
		t.Error("the upstream request did not arrive over TLS")
	}
	if got["path"] != "/users/abc" {
		t.Errorf("upstream saw %q", got["path"])
	}

	// The posture has to be auditable without reading the config file.
	for _, b := range rt.Backends() {
		if b.Name == "secure" && !b.InsecureTLS() {
			t.Error("the backend does not report that verification is disabled")
		}
	}
}

// A plain-HTTP backend alongside a lax one must not inherit the setting.
func TestInsecureFlagDoesNotLeakBetweenBackends(t *testing.T) {
	lax := newTLSUpstream(t, usersSpec)
	plain := newUpstream(t, ordersSpec)

	cfg := fmt.Sprintf(`
version: 1
server:
  listen: ":0"
defaults:
  timeout: 5s
backends:
  - name: lax
    spec: { url: %s/openapi.yaml }
    hosts: ["%s"]
    tls: { insecure_skip_verify: true }
  - name: strict
    spec: { url: %s/openapi.yaml }
    hosts: ["%s"]
expose:
  - backend: lax
    prefix: /lax
    exclude: ["internal*"]
  - backend: strict
    prefix: /strict
`, lax.URL, lax.URL, plain.URL, plain.URL)

	srv, cleanup := buildGateway(t, cfg)
	defer cleanup()

	for _, b := range srv.Current().Backends() {
		switch b.Name {
		case "lax":
			if !b.InsecureTLS() {
				t.Error("lax backend should have verification disabled")
			}
		case "strict":
			if b.InsecureTLS() {
				t.Error("the setting leaked onto a backend that did not ask for it")
			}
		}
	}
}
