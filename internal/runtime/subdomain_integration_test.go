package runtime_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/observ"
	"github.com/rivencove/nina/internal/runtime"
	"github.com/rivencove/nina/internal/specsrc"
)

// syncBuffer lets the test read log lines the server writes from its own
// goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// requestLine waits for the nth access log entry (1-based) and returns it. The
// line is written after the response is sent, so it can trail the client.
func (s *syncBuffer) requestLine(t *testing.T, n int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		var lines []map[string]any
		sc := bufio.NewScanner(bytes.NewReader(s.b.Bytes()))
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil && m["msg"] == "request" {
				lines = append(lines, m)
			}
		}
		s.mu.Unlock()
		if len(lines) >= n {
			return lines[n-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("access log line %d never written (have %d)", n, len(lines))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubdomainIsLogged(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	cfgYAML := strings.Replace(twoBackendConfig(users, orders), "  listen: \":0\"\n", `  listen: ":0"
  trusted_proxy_cidrs: ["127.0.0.1", "::1"]
  subdomain:
    hosts: ["{subdomain}.example.com"]
    exclude: [www]
    trust_forwarded_host: true
`, 1)

	dir := t.TempDir()
	path := filepath.Join(dir, "nina.yaml")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	reg := prometheus.NewRegistry()
	srv, err := runtime.NewServer(context.Background(), cfg, runtime.Deps{
		Logger:   slog.New(slog.NewJSONHandler(logs, nil)),
		Metrics:  observ.New(reg, "test"),
		Fetcher:  specsrc.New(filepath.Join(dir, "cache")),
		Registry: testRegistry(),
	}, runtime.ServerOptions{ConfigPath: path, DrainGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Current().Retire(0)
	gw := httptest.NewServer(srv)
	defer gw.Close()

	sent := 0
	send := func(host, forwarded, path string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", gw.URL+path, nil)
		req.Host = host
		if forwarded != "" {
			req.Header.Set("X-Forwarded-Host", forwarded)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		sent++
		return logs.requestLine(t, sent)
	}

	cases := []struct {
		name, host, forwarded, path, want string
	}{
		{"matched route", "acme.example.com", "", "/api/orders", "acme"},
		{"unmatched route still logs the subdomain", "acme.example.com:8080", "", "/nope", "acme"},
		{"excluded label", "www.example.com", "", "/api/orders", ""},
		{"foreign host", "acme.other.com", "", "/api/orders", ""},
		{"trusted proxy's forwarded host wins", "lb.internal", "globex.example.com", "/api/orders", "globex"},
		{"rightmost forwarded value is the proxy's", "lb.internal", "spoof.example.com, initech.example.com", "/api/orders", "initech"},
	}
	for _, c := range cases {
		line := send(c.host, c.forwarded, c.path)
		if got, _ := line["subdomain"].(string); got != c.want {
			t.Errorf("%s: subdomain = %q, want %q", c.name, got, c.want)
		}
	}

	// Subdomains are unbounded, so they must never reach a metric label.
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "subdomain" || l.GetValue() == "acme" || l.GetValue() == "globex" {
					t.Errorf("metric %s carries subdomain label %s=%s", f.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}
}

func TestSubdomainIgnoresForwardedHostFromUntrustedPeer(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	// The test client is loopback, which is deliberately not trusted here.
	cfgYAML := strings.Replace(twoBackendConfig(users, orders), "  listen: \":0\"\n", `  listen: ":0"
  trusted_proxy_cidrs: ["10.0.0.0/8"]
  subdomain:
    hosts: ["{subdomain}.example.com"]
    trust_forwarded_host: true
`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "nina.yaml")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	srv, err := runtime.NewServer(context.Background(), cfg, runtime.Deps{
		Logger:   slog.New(slog.NewJSONHandler(logs, nil)),
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

	req, _ := http.NewRequest("GET", gw.URL+"/api/orders", nil)
	req.Host = "acme.example.com"
	req.Header.Set("X-Forwarded-Host", "spoof.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := logs.requestLine(t, 1)["subdomain"]; got != "acme" {
		t.Errorf("subdomain = %v, want acme from Host: an untrusted peer's X-Forwarded-Host was believed", got)
	}
}
