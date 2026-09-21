// Package runtime builds an immutable serving runtime from configuration and
// swaps it in atomically.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/oas"
	"github.com/rivencove/nina/internal/observ"
	"github.com/rivencove/nina/internal/proxy"
	"github.com/rivencove/nina/internal/reqctx"
	"github.com/rivencove/nina/internal/router"
	"github.com/rivencove/nina/internal/routetable"
	"github.com/rivencove/nina/internal/specsrc"
	"github.com/rivencove/nina/internal/store"
	yaml "go.yaml.in/yaml/v4"
)

// BoundRoute is a route with its fully assembled handler.
type BoundRoute struct {
	Route   *routetable.Route
	Backend *proxy.Backend
	Handler http.Handler
}

// Runtime is an immutable snapshot of everything needed to serve traffic. It is
// never mutated after Build returns; a configuration change produces a new one.
type Runtime struct {
	Generation uint64
	Config     *config.Config
	Table      *routetable.Table
	Router     *router.Router[*BoundRoute]

	backends  map[string]*proxy.Backend
	instances map[string]chain.Instance
	stores    *store.Registry
	// specHashes lets a refresh poll decide whether anything actually changed.
	specHashes map[string]string

	cancel  context.CancelFunc
	refs    atomic.Int64
	retired atomic.Bool
	closed  sync.Once
	log     *slog.Logger
}

// Deps are the process-wide services a runtime borrows.
type Deps struct {
	Logger  *slog.Logger
	Metrics *observ.Metrics
	Fetcher *specsrc.Fetcher
	// Registry supplies middleware constructors.
	Registry *chain.Registry
}

// Build assembles a runtime. It performs every fallible step — fetching specs,
// merging, compiling schemas, dialling nothing — before the caller swaps it in,
// so a bad configuration can be rejected without disturbing live traffic.
func Build(ctx context.Context, cfg *config.Config, deps Deps, generation uint64) (rt *Runtime, err error) {
	log := deps.Logger

	// Anything built before a later step fails has to be released, or a failing
	// reload leaks a Redis client and a JWKS poller per attempt. `committed` flips
	// once ownership passes to the returned Runtime.
	var (
		instances map[string]chain.Instance
		backends  = map[string]*proxy.Backend{}
		stores    *store.Registry
		committed bool
	)
	rtCtx, cancelRuntime := context.WithCancel(context.Background())
	defer func() {
		if committed {
			return
		}
		cancelRuntime()
		chain.CloseAll(instances)
		for _, b := range backends {
			b.Close()
		}
		if stores != nil {
			_ = stores.Close()
		}
	}()

	// 1. Stores.
	var ro *store.RedisOptions
	if cfg.Stores.Redis != nil {
		ro = &store.RedisOptions{
			Addr:     cfg.Stores.Redis.Addr,
			Password: cfg.Stores.Redis.Password,
			DB:       cfg.Stores.Redis.DB,
			Timeout:  cfg.Stores.Redis.Timeout.Std(),
		}
	}
	stores, err = store.NewRegistry(ro)
	if err != nil {
		return nil, err
	}
	if stores.HasRedis() {
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
		if perr := stores.Ping(pctx); perr != nil {
			// Not fatal: rate limiters degrade rather than the gateway refusing
			// to start because a counter store is unreachable.
			log.Warn("redis is not reachable; rate limiters will run degraded",
				"addr", cfg.Stores.Redis.Addr, "error", perr)
		}
		pcancel()
	}

	// 2. Middleware instances (shared across routes).
	deps2 := chain.Deps{Logger: log, Stores: stores, Metrics: deps.Metrics}
	instances, err = deps.Registry.Build(cfg, deps2)
	if err != nil {
		return nil, err
	}

	// 3. Upstream specs.
	specs := map[string]*oas.Spec{}
	hashes := map[string]string{}
	var degraded []string
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.InsecureTLS() && b.UsesTLS() {
			// Loud, and on every build rather than only the first: an operator
			// scanning logs after an incident should not have to scroll back to
			// the original start-up to discover this was on.
			log.Warn("upstream TLS certificate verification is DISABLED for this backend; "+
				"the connection is encrypted but not authenticated, so an interposed attacker would go undetected",
				"backend", b.Name)
		}
		src := specsrc.Source{
			Backend:            b.Name,
			File:               cfg.ResolvePath(b.Spec.File),
			URL:                b.Spec.URL,
			OnError:            b.Spec.OnError,
			InsecureSkipVerify: b.InsecureTLS(),
		}
		res, ferr := deps.Fetcher.Fetch(ctx, src)
		if ferr != nil {
			return nil, ferr
		}
		if res.Stale {
			degraded = append(degraded, b.Name)
			log.Warn("using a cached spec; the upstream document was unreachable", "backend", b.Name)
		}

		spec, lerr := oas.Load(res.Data, oas.LoadOptions{
			BasePath:        cfg.Dir,
			AllowRemoteRefs: b.Spec.URL != "",
		})
		if lerr != nil {
			return nil, fmt.Errorf("backend %q: %w", b.Name, lerr)
		}
		specs[b.Name] = spec
		hashes[b.Name] = res.Hash
	}

	// 4. The route table, and with it the published document.
	table, err := routetable.Build(routetable.BuildInput{
		Config:         cfg,
		Specs:          specs,
		Degraded:       degraded,
		SecurityScheme: securitySchemeFunc(instances),
	})
	if err != nil {
		return nil, err
	}
	if hidden := table.HiddenRoutes(); len(hidden) > 0 {
		// Logged on every build, not only the first: an endpoint that is served
		// but absent from the document is invisible to anyone auditing the
		// published API, so it has to be visible to anyone auditing the logs.
		paths := make([]string, 0, len(hidden))
		for _, r := range hidden {
			paths = append(paths, r.String())
		}
		log.Info("routes are served but hidden from the published document",
			"count", len(hidden), "tags", cfg.Spec.HideTags, "routes", paths)
	}

	// 5. Upstream backends.
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		backend, berr := proxy.NewBackend(proxy.Options{
			Name:               b.Name,
			Hosts:              b.Hosts,
			LoadBalance:        b.LoadBalance,
			Timeout:            b.Timeout.Or(cfg.Defaults.Timeout.Std()),
			Retry:              retryPolicy(b.Retry, cfg.Defaults.Retry),
			Breaker:            breakerConfig(b.CircuitBreaker, cfg.Defaults.CircuitBreaker, b.Name, deps.Metrics),
			ForwardHeaders:     b.ForwardHeaders,
			MaxRetryBody:       cfg.Server.MaxRequestBody,
			InsecureSkipVerify: b.InsecureTLS(),
		})
		if berr != nil {
			return nil, berr
		}
		backends[b.Name] = backend
	}

	// 6. The router, with a fully assembled chain per route.
	rt = &Runtime{
		Generation: generation,
		Config:     cfg,
		Table:      table,
		Router:     router.New[*BoundRoute](),
		backends:   backends,
		instances:  instances,
		stores:     stores,
		specHashes: hashes,
		cancel:     cancelRuntime,
		log:        log,
	}
	for _, route := range table.Routes {
		backend := backends[route.Backend.Name]
		br := &BoundRoute{Route: route, Backend: backend}
		final := rt.proxyHandler(br, deps.Metrics)
		h, aerr := chain.Assemble(route.Middleware, instances, route, final)
		if aerr != nil {
			return nil, aerr
		}
		br.Handler = h
		if rerr := rt.Router.Add(route.Method, route.GatewayPath, br); rerr != nil {
			return nil, rerr
		}
	}

	// 6b. Synthetic OPTIONS routes for CORS preflight.
	//
	// A preflight is a browser protocol detail, not an API operation: OpenAPI
	// does not describe it and the published document must not claim it. But the
	// router would answer OPTIONS with 405 long before any CORS middleware ran,
	// so the gateway has to serve it itself.
	if err = rt.addPreflightRoutes(table, instances); err != nil {
		return nil, err
	}

	// 7. Health checks, once everything else has succeeded.
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.HealthCheck == nil {
			continue
		}
		backend := backends[b.Name]
		recordHealthy := func() {
			if deps.Metrics == nil {
				return
			}
			n := 0
			for _, h := range backend.Pool().Hosts() {
				if h.Healthy() {
					n++
				}
			}
			deps.Metrics.HostsHealthy.WithLabelValues(b.Name).Set(float64(n))
		}
		recordHealthy()
		backend.StartHealthChecks(rtCtx, proxy.HealthCheckConfig{
			Path:           b.HealthCheck.Path,
			Interval:       b.HealthCheck.Interval.Std(),
			Timeout:        b.HealthCheck.Timeout.Std(),
			UnhealthyAfter: b.HealthCheck.UnhealthyAfter,
			HealthyAfter:   b.HealthCheck.HealthyAfter,
		}, func(h *proxy.Host, healthy bool) {
			log.Warn("upstream host health changed", "backend", b.Name, "host", h.URL.Host, "healthy", healthy)
			recordHealthy()
		})
	}

	// Ownership has transferred to rt; stop the deferred cleanup from firing.
	committed = true
	return rt, nil
}

// addPreflightRoutes registers an OPTIONS handler for every gateway path whose
// routes use CORS middleware and which does not already serve OPTIONS.
//
// Only CORS middleware is carried over: the synthetic route has no upstream
// operation, so anything that inspects one (request validation, for instance)
// has nothing to work with and must not run.
func (rt *Runtime) addPreflightRoutes(table *routetable.Table, instances map[string]chain.Instance) error {
	type pathInfo struct {
		methods []string
		cors    []string
		sample  *routetable.Route
	}
	byPath := map[string]*pathInfo{}
	for _, r := range table.Routes {
		pi := byPath[r.GatewayPath]
		if pi == nil {
			pi = &pathInfo{sample: r}
			byPath[r.GatewayPath] = pi
		}
		pi.methods = append(pi.methods, r.Method)
		for _, name := range r.Middleware {
			inst, ok := instances[name]
			if !ok || inst.Category() != chain.CatCORS {
				continue
			}
			if !contains(pi.cors, name) {
				pi.cors = append(pi.cors, name)
			}
		}
	}

	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		pi := byPath[p]
		if len(pi.cors) == 0 || contains(pi.methods, http.MethodOptions) {
			continue
		}
		allow := append(append([]string(nil), pi.methods...), http.MethodOptions)
		sort.Strings(allow)

		route := &routetable.Route{
			Method:         http.MethodOptions,
			GatewayPath:    p,
			OperationID:    "preflight",
			Backend:        pi.sample.Backend,
			Middleware:     pi.cors,
			AllowedMethods: pi.sample.AllowedMethods,
		}
		br := &BoundRoute{Route: route}
		h, err := chain.Assemble(pi.cors, instances, route, preflightTerminal(allow))
		if err != nil {
			return err
		}
		br.Handler = h
		if err := rt.Router.Add(http.MethodOptions, p, br); err != nil {
			return err
		}
	}
	return nil
}

// preflightTerminal answers an OPTIONS request that the CORS middleware chose
// not to handle itself (a plain OPTIONS with no preflight headers).
func preflightTerminal(allow []string) http.Handler {
	joined := strings.Join(allow, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", joined)
		w.WriteHeader(http.StatusNoContent)
	})
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// securitySchemeFunc lets auth middleware describe itself in the merged document.
func securitySchemeFunc(instances map[string]chain.Instance) routetable.SecuritySchemeFunc {
	return func(name string, _ config.MiddlewareDef) (*yaml.Node, bool) {
		inst, ok := instances[name]
		if !ok {
			return nil, false
		}
		sc, ok := inst.(chain.SecurityContributor)
		if !ok {
			return nil, false
		}
		return sc.SecurityScheme(), true
	}
}

func retryPolicy(b, d *config.Retry) proxy.RetryPolicy {
	r := b
	if r == nil {
		r = d
	}
	if r == nil {
		return proxy.RetryPolicy{}
	}
	return proxy.RetryPolicy{
		Attempts:       r.Attempts,
		Backoff:        r.Backoff.Std(),
		Jitter:         r.Jitter,
		IdempotentOnly: r.IdempotentOnly == nil || *r.IdempotentOnly,
	}
}

func breakerConfig(b, d *config.CircuitBreaker, name string, m *observ.Metrics) proxy.BreakerConfig {
	c := b
	if c == nil {
		c = d
	}
	if c == nil {
		return proxy.BreakerConfig{}
	}
	return proxy.BreakerConfig{
		FailureRatio: c.FailureRatio,
		MinRequests:  c.MinRequests,
		OpenFor:      c.OpenFor.Std(),
	}
}

// proxyHandler is the terminal handler of every chain.
func (rt *Runtime) proxyHandler(br *BoundRoute, m *observ.Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := reqctx.From(r.Context())
		upstreamPath := br.Route.UpstreamPath
		if info != nil && len(info.Params) > 0 {
			upstreamPath = substituteParams(upstreamPath, info.Params)
		}

		resp, result, err := br.Backend.Do(r, upstreamPath)
		if result != nil && m != nil && result.Upstream > 0 {
			m.UpstreamDuration.WithLabelValues(br.Route.Backend.Name).Observe(result.Upstream.Seconds())
		}
		if info != nil && result != nil {
			info.UpstreamAttempt = result.Attempts
			info.UpstreamLatency = result.Upstream
			if result.Host != nil {
				info.UpstreamHost = result.Host.URL.Host
			}
		}
		if err != nil {
			var pe *proxy.Error
			status := http.StatusBadGateway
			kind := "unknown"
			if asProxyError(err, &pe) {
				status, kind = pe.StatusCode(), string(pe.Kind)
			}
			if m != nil {
				m.UpstreamErrors.WithLabelValues(br.Route.Backend.Name, kind).Inc()
			}
			rt.log.Warn("upstream request failed",
				"route", br.Route.OperationID, "backend", br.Route.Backend.Name,
				"kind", kind, "error", err)
			if status == 499 {
				// The client hung up; there is nobody left to answer.
				return
			}
			writeUpstreamError(w, r, status, kind)
			return
		}
		defer resp.Body.Close()
		if info != nil {
			info.UpstreamStatus = resp.StatusCode
		}
		if _, cerr := proxy.CopyResponse(w, resp); cerr != nil {
			rt.log.Debug("response copy ended early", "route", br.Route.OperationID, "error", cerr)
		}
	})
}

// substituteParams rebuilds the upstream path from the matched parameters.
//
// It rewrites segments in place rather than reassembling the string, because an
// off-by-one on the separator here produces a leading "//", which upstream
// routers answer with a redirect rather than the response the caller expected.
func substituteParams(tmpl string, params router.Params) string {
	segs := strings.Split(tmpl, "/")
	for i, seg := range segs {
		if len(seg) < 2 || seg[0] != '{' || seg[len(seg)-1] != '}' {
			continue
		}
		if v, ok := params.Get(seg[1 : len(seg)-1]); ok {
			// Re-escape: the value was decoded at match time, and a value
			// containing a slash must not create a new path segment.
			segs[i] = url.PathEscape(v)
		}
	}
	out := strings.Join(segs, "/")
	if out == "" {
		return "/"
	}
	return out
}

func asProxyError(err error, target **proxy.Error) bool {
	for err != nil {
		if pe, ok := err.(*proxy.Error); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// SpecHash returns the last fetched spec hash for a backend.
func (rt *Runtime) SpecHash(backend string) string { return rt.specHashes[backend] }

// Acquire pins the runtime for the duration of a request. It fails once the
// runtime has been retired, telling the caller to re-read the current pointer.
func (rt *Runtime) Acquire() bool {
	rt.refs.Add(1)
	if rt.retired.Load() {
		rt.Release()
		return false
	}
	return true
}

func (rt *Runtime) Release() {
	if rt.refs.Add(-1) == 0 && rt.retired.Load() {
		rt.close()
	}
}

// Retire marks the runtime as replaced and closes it once in-flight requests
// finish, or after grace expires.
func (rt *Runtime) Retire(grace time.Duration) {
	rt.retired.Store(true)
	if rt.refs.Load() == 0 {
		rt.close()
		return
	}
	go func() {
		deadline := time.After(grace)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if rt.refs.Load() <= 0 {
					rt.close()
					return
				}
			case <-deadline:
				rt.log.Warn("drain grace expired; closing runtime with requests still in flight",
					"generation", rt.Generation, "in_flight", rt.refs.Load())
				rt.close()
				return
			}
		}
	}()
}

func (rt *Runtime) close() {
	rt.closed.Do(func() {
		rt.cancel()
		chain.CloseAll(rt.instances)
		for _, b := range rt.backends {
			b.Close()
		}
		_ = rt.stores.Close()
		rt.log.Debug("runtime closed", "generation", rt.Generation)
	})
}

// Backends returns the runtime's upstream backends, sorted by name, for the
// admin status view.
func (rt *Runtime) Backends() []*proxy.Backend {
	names := make([]string, 0, len(rt.backends))
	for n := range rt.backends {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*proxy.Backend, 0, len(names))
	for _, n := range names {
		out = append(out, rt.backends[n])
	}
	return out
}
