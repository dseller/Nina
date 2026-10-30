package admin_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rivencove/nina/internal/admin"
	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/observ"
	"github.com/rivencove/nina/internal/runtime"
	"github.com/rivencove/nina/internal/scanguard"
	"github.com/rivencove/nina/internal/specsrc"
)

const spec = `
openapi: 3.1.0
info: { title: Ping, version: 1.0.0 }
paths:
  /ping:
    get:
      operationId: ping
      responses:
        '200': { description: ok }
`

// newServer builds a gateway whose upstream is never contacted.
func newServer(t *testing.T, extra string) *runtime.Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ping.yaml"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "nina.yaml")
	doc := `
version: 1
server:
  listen: ":0"
` + extra + `
backends:
  - name: ping
    spec: { file: ./ping.yaml }
    hosts: ["http://127.0.0.1:1"]
expose:
  - backend: ping
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(context.Background(), cfg, runtime.Deps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:  observ.New(prometheus.NewRegistry(), "test"),
		Fetcher:  specsrc.New(filepath.Join(dir, "cache")),
		Registry: chain.NewRegistry(),
	}, runtime.ServerOptions{ConfigPath: path, DrainGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Current().Retire(0) })
	return srv
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestScanGuardBans(t *testing.T) {
	srv := newServer(t, "  scan_guard: {}\n")
	h := admin.Handler(admin.Options{Server: srv, Gatherer: prometheus.NewRegistry()})
	srv.Current().ScanGuard.Miss(context.Background(), "203.0.113.7", "/.env")

	rec := do(t, h, "GET", "/scanguard/bans")
	var bans []scanguard.Ban
	if err := json.Unmarshal(rec.Body.Bytes(), &bans); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	if len(bans) != 1 || bans[0].Client != "203.0.113.7" || bans[0].Reason != "trap" {
		t.Fatalf("bans = %+v", bans)
	}

	if rec := do(t, h, "DELETE", "/scanguard/bans"); rec.Code != 400 {
		t.Errorf("unban without a client = %d, want 400", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/scanguard/bans?client=203.0.113.7"); rec.Code != 204 {
		t.Errorf("unban = %d, want 204", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/scanguard/bans?client=203.0.113.7"); rec.Code != 404 {
		t.Errorf("second unban = %d, want 404", rec.Code)
	}
	if rec := do(t, h, "GET", "/scanguard/bans"); rec.Body.String() != "[]\n" {
		t.Errorf("bans after unban = %q", rec.Body)
	}
}

func TestScanGuardBansWhenDisabled(t *testing.T) {
	srv := newServer(t, "")
	h := admin.Handler(admin.Options{Server: srv, Gatherer: prometheus.NewRegistry()})
	if rec := do(t, h, "GET", "/scanguard/bans"); rec.Code != 404 {
		t.Errorf("got %d, want 404", rec.Code)
	}
}
