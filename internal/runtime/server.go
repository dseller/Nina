package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/httperr"
	"github.com/rivencove/nina/internal/observ"
	"github.com/rivencove/nina/internal/reqctx"
)

// Server owns the current Runtime and serves traffic against it.
//
// The runtime is held in an atomic pointer: a request reads it once on entry and
// keeps using that snapshot to completion, so a reload can never change a
// request's routing or middleware halfway through.
type Server struct {
	current atomic.Pointer[Runtime]
	deps    Deps
	log     *slog.Logger
	metrics *observ.Metrics

	configPath string
	drainGrace time.Duration
	// settings holds the request-scoped knobs a reload can change. Swapped as
	// a unit so a request never mixes the CIDR list of one generation with the
	// body limit of another.
	settings atomic.Pointer[serverSettings]

	generation atomic.Uint64
	ready      atomic.Bool
	// reloadMu serialises reloads so two triggers cannot build at once.
	reloadMu sync.Mutex
}

// serverSettings is immutable once stored: a reload builds a fresh value and
// swaps the pointer rather than mutating the fields a live request may be
// reading.
type serverSettings struct {
	trustedProxies []*net.IPNet
	maxBody        int64
}

// ServerOptions configures a Server.
type ServerOptions struct {
	ConfigPath string
	DrainGrace time.Duration
}

// NewServer builds the first runtime and returns a ready server.
func NewServer(ctx context.Context, cfg *config.Config, deps Deps, opt ServerOptions) (*Server, error) {
	s := &Server{
		deps:       deps,
		log:        deps.Logger,
		metrics:    deps.Metrics,
		configPath: opt.ConfigPath,
		drainGrace: opt.DrainGrace,
	}
	if s.drainGrace == 0 {
		s.drainGrace = 30 * time.Second
	}
	trusted, err := reqctx.ParseCIDRs(cfg.Server.TrustedProxyCIDRs)
	if err != nil {
		return nil, fmt.Errorf("server.trusted_proxy_cidrs: %w", err)
	}
	s.settings.Store(&serverSettings{trustedProxies: trusted, maxBody: cfg.Server.MaxRequestBody})

	rt, err := Build(ctx, cfg, deps, s.generation.Add(1))
	if err != nil {
		return nil, err
	}
	s.current.Store(rt)
	s.ready.Store(true)
	return s, nil
}

// Current returns the runtime now serving traffic.
func (s *Server) Current() *Runtime { return s.current.Load() }

// Ready reports whether a runtime has ever been built successfully.
//
// It is deliberately not tied to upstream health: one sick backend should not
// get the whole gateway pulled out of a load balancer.
func (s *Server) Ready() bool { return s.ready.Load() }

// Reload rebuilds from the configuration file and swaps the result in.
//
// A failure leaves the running runtime untouched. This is the single most
// important property of the reload path: a typo in a config file must never take
// down a healthy gateway.
func (s *Server) Reload(ctx context.Context, reason string) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	start := time.Now()
	cfg, err := config.Load(s.configPath)
	if err != nil {
		s.recordFailure("config", err, reason)
		return err
	}
	rt, err := Build(ctx, cfg, s.deps, s.generation.Add(1))
	if err != nil {
		s.recordFailure("build", err, reason)
		return err
	}

	trusted, err := reqctx.ParseCIDRs(cfg.Server.TrustedProxyCIDRs)
	if err != nil {
		rt.Retire(0)
		s.recordFailure("config", err, reason)
		return err
	}

	old := s.current.Swap(rt)
	// Read on the hot path, written only here under reloadMu. A request that
	// started before this point keeps the previous settings to completion.
	s.settings.Store(&serverSettings{trustedProxies: trusted, maxBody: cfg.Server.MaxRequestBody})

	s.log.Info("configuration reloaded",
		"reason", reason,
		"generation", rt.Generation,
		"routes", len(rt.Table.Routes),
		"took", time.Since(start).Round(time.Millisecond))

	if old != nil {
		old.Retire(s.drainGrace)
	}
	return nil
}

func (s *Server) recordFailure(stage string, err error, reason string) {
	// Deliberately Error, not Fatal: the previous runtime keeps serving.
	s.log.Error("reload failed; keeping the running configuration",
		"stage", stage, "reason", reason, "error", err)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Re-read on failure: the runtime we picked up may have been retired between
	// the load and the acquire.
	var rt *Runtime
	for i := 0; i < 8; i++ {
		rt = s.current.Load()
		if rt == nil {
			break
		}
		if rt.Acquire() {
			break
		}
		rt = nil
	}
	if rt == nil {
		http.Error(w, "gateway is not ready", http.StatusServiceUnavailable)
		return
	}
	defer rt.Release()

	set := s.settings.Load()

	info := &reqctx.Info{
		RequestID: requestID(r),
		Start:     time.Now(),
		ClientIP:  reqctx.ClientIP(r, set.trustedProxies),
	}
	r = r.WithContext(reqctx.With(r.Context(), info))
	w.Header().Set("X-Request-Id", info.RequestID)

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer s.finish(rec, r, info)
	defer s.recover(rec, r, info)

	if set.maxBody > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(rec, r.Body, set.maxBody)
	}

	match := rt.Router.Match(r.Method, r.URL.EscapedPath())
	if !match.Found {
		if match.PathMatched {
			rec.Header().Set("Allow", joinComma(match.Allow))
			httperr.Write(rec, r, httperr.Problem{
				Status: http.StatusMethodNotAllowed,
				Title:  "Method not allowed",
				Detail: "this path accepts " + joinComma(match.Allow),
			})
			return
		}
		httperr.Write(rec, r, httperr.Problem{
			Status: http.StatusNotFound,
			Title:  "No such route",
			Detail: "the gateway publishes no operation for " + r.Method + " " + r.URL.Path,
		})
		return
	}

	br := match.Payload
	info.Params = match.Params
	info.OperationID = br.Route.OperationID
	info.Backend = br.Route.Backend.Name
	info.Template = br.Route.GatewayPath

	br.Handler.ServeHTTP(rec, r)
}

// recover turns a middleware or handler panic into a 500 instead of killing the
// process and every other in-flight request with it.
func (s *Server) recover(rec *statusRecorder, r *http.Request, info *reqctx.Info) {
	v := recover()
	if v == nil {
		return
	}
	if v == http.ErrAbortHandler {
		panic(v) // the server's own signal; let it through
	}
	s.log.Error("panic while serving request",
		"request_id", info.RequestID, "route", info.OperationID,
		"panic", v, "stack", string(debug.Stack()))
	if !rec.wrote {
		httperr.Write(rec, r, httperr.Problem{
			Status: http.StatusInternalServerError,
			Title:  "Internal gateway error",
		})
	}
}

func (s *Server) finish(rec *statusRecorder, r *http.Request, info *reqctx.Info) {
	elapsed := time.Since(info.Start)
	route := info.Template
	if route == "" {
		route = "unmatched"
	}
	if s.metrics != nil {
		s.metrics.Requests.WithLabelValues(route, r.Method, observ.StatusClass(rec.status)).Inc()
	}

	level := slog.LevelInfo
	if rec.status >= 500 {
		level = slog.LevelError
	} else if rec.status >= 400 {
		level = slog.LevelWarn
	}
	s.log.Log(r.Context(), level, "request",
		"request_id", info.RequestID,
		"method", r.Method,
		"path", r.URL.Path,
		"route", route,
		"operation", info.OperationID,
		"backend", info.Backend,
		"status", rec.status,
		"bytes", rec.bytes,
		"duration_ms", elapsed.Milliseconds(),
		"upstream_ms", info.UpstreamLatency.Milliseconds(),
		"upstream_host", info.UpstreamHost,
		"attempts", info.UpstreamAttempt,
		"consumer", info.Consumer,
		"client_ip", info.ClientIP,
	)
}

// statusRecorder captures the status and byte count for logs and metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// writeUpstreamError renders a gateway-generated upstream failure.
func writeUpstreamError(w http.ResponseWriter, r *http.Request, status int, kind string) {
	titles := map[string]string{
		"breaker_open":    "Upstream is unavailable",
		"no_healthy_host": "Upstream is unavailable",
		"timeout":         "Upstream timed out",
		"connect":         "Upstream could not be reached",
	}
	title, ok := titles[kind]
	if !ok {
		title = "Upstream error"
	}
	httperr.Write(w, r, httperr.Problem{
		Status: status,
		Title:  title,
		Detail: "the gateway could not complete the request against the upstream service",
	})
}

func requestID(r *http.Request) string {
	// Honour an inbound id so a trace survives across hops, but bound its length
	// and character set so it is safe in logs and headers.
	if v := r.Header.Get("X-Request-Id"); v != "" && len(v) <= 128 && isPrintableASCII(v) {
		return v
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func joinComma(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
