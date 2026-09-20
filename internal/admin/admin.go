// Package admin serves the operational surface: metrics, health, the published
// OpenAPI document and a documentation viewer.
//
// It binds to its own listener so none of this has to be exposed publicly.
package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rivencove/nina/internal/runtime"
)

// Options configures the admin mux.
type Options struct {
	Server   *runtime.Server
	Gatherer prometheus.Gatherer
	Version  string
	// Docs enables /openapi.json, /openapi.yaml and /docs.
	Docs bool
}

// Handler builds the admin mux.
func Handler(o Options) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /metrics", promhttp.HandlerFor(o.Gatherer, promhttp.HandlerOpts{}))

	// Liveness: the process is up and able to answer.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	// Readiness: a runtime has been built. Deliberately not tied to upstream
	// health, so one sick backend cannot pull the gateway out of rotation.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !o.Server.Ready() {
			http.Error(w, "no runtime has been built", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ready")
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		rt := o.Server.Current()
		if rt == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		type backendStatus struct {
			Name    string   `json:"name"`
			Breaker string   `json:"circuit_breaker"`
			Healthy []string `json:"healthy_hosts"`
			Down    []string `json:"unhealthy_hosts"`
			// Surfaced so this can be audited without reading the config.
			InsecureTLS bool `json:"tls_verification_disabled,omitempty"`
		}
		out := struct {
			Version    string `json:"version"`
			Generation uint64 `json:"generation"`
			Routes     int    `json:"routes"`
			// Surfaced so the served-but-undocumented set can be audited without
			// reading the config.
			HiddenRoutes int             `json:"hidden_routes,omitempty"`
			HideTags     []string        `json:"hide_tags,omitempty"`
			Degraded     []string        `json:"degraded_backends,omitempty"`
			Backends     []backendStatus `json:"backends"`
		}{
			Version:      o.Version,
			Generation:   rt.Generation,
			Routes:       len(rt.Table.Routes),
			HiddenRoutes: len(rt.Table.HiddenRoutes()),
			HideTags:     rt.Config.Spec.HideTags,
			Degraded:     rt.Table.Degraded,
		}
		for _, b := range rt.Backends() {
			bs := backendStatus{
				Name:        b.Name,
				Breaker:     b.Breaker().State().String(),
				InsecureTLS: b.InsecureTLS(),
			}
			for _, h := range b.Pool().Hosts() {
				if h.Healthy() {
					bs.Healthy = append(bs.Healthy, h.URL.Host)
				} else {
					bs.Down = append(bs.Down, h.URL.Host)
				}
			}
			out.Backends = append(out.Backends, bs)
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	})

	// /routes is the operator's view of the invariant: what is served is what is
	// published.
	mux.HandleFunc("GET /routes", func(w http.ResponseWriter, r *http.Request) {
		rt := o.Server.Current()
		if rt == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		type routeInfo struct {
			Method      string   `json:"method"`
			Path        string   `json:"path"`
			OperationID string   `json:"operation_id"`
			Backend     string   `json:"backend"`
			Upstream    string   `json:"upstream_path"`
			Middleware  []string `json:"middleware,omitempty"`
			// Hidden routes are served but absent from the published document,
			// so this listing is the only place they show up.
			Hidden bool `json:"hidden,omitempty"`
		}
		out := make([]routeInfo, 0, len(rt.Table.Routes))
		for _, rr := range rt.Table.Routes {
			out = append(out, routeInfo{
				Method: rr.Method, Path: rr.GatewayPath, OperationID: rr.OperationID,
				Backend: rr.Backend.Name, Upstream: rr.UpstreamPath, Middleware: rr.Middleware,
				Hidden: rr.Hidden,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	})

	if o.Docs {
		spec := func(contentType string, pick func(*runtime.Runtime) []byte) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				rt := o.Server.Current()
				if rt == nil {
					http.Error(w, "not ready", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Cache-Control", "no-cache")
				_, _ = w.Write(pick(rt))
			}
		}
		mux.HandleFunc("GET /openapi.json", spec("application/json", func(rt *runtime.Runtime) []byte {
			return rt.Table.SpecJSON
		}))
		mux.HandleFunc("GET /openapi.yaml", spec("application/yaml", func(rt *runtime.Runtime) []byte {
			return rt.Table.SpecYAML
		}))
		mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(docsPage))
		})
	}

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		var b strings.Builder
		b.WriteString("nina admin\n\n")
		for _, p := range []string{"/metrics", "/healthz", "/readyz", "/status", "/routes"} {
			b.WriteString(p + "\n")
		}
		if o.Docs {
			b.WriteString("/openapi.json\n/openapi.yaml\n/docs\n")
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(b.String()))
	})

	return mux
}

// docsPage renders the merged document. The viewer is loaded from a CDN rather
// than vendored, so an air-gapped deployment should simply leave docs disabled
// and fetch /openapi.json instead.
const docsPage = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>API Reference</title>
</head>
<body>
  <script id="api-reference" data-url="/openapi.json"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>
`
