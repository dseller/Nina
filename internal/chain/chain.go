// Package chain assembles per-route middleware chains from named configuration.
package chain

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/routetable"
	"github.com/rivencove/nina/internal/store"
	yaml "go.yaml.in/yaml/v4"
)

// Category fixes where a middleware sits in the chain.
//
// Ordering is a property of the category, not of configuration order, so an
// author cannot accidentally place rate limiting before authentication and
// meter requests that were about to be rejected anyway.
type Category int

const (
	CatCORS Category = iota
	CatAuth
	CatRateLimit
	CatValidate
	CatCache
	CatTransform
	CatScript
)

func (c Category) String() string {
	switch c {
	case CatCORS:
		return "cors"
	case CatAuth:
		return "auth"
	case CatRateLimit:
		return "ratelimit"
	case CatValidate:
		return "validate"
	case CatCache:
		return "cache"
	case CatTransform:
		return "transform"
	case CatScript:
		return "script"
	}
	return "unknown"
}

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Instance is a configured middleware, shared across every route that names it.
// Expensive state such as a JWKS cache or a Redis client lives here and is built
// once per runtime.
type Instance interface {
	Category() Category
	// ForRoute returns the wrapper for one route, or nil to skip this route.
	ForRoute(r *routetable.Route) (Middleware, error)
}

// Closer is implemented by instances holding resources.
type Closer interface{ Close() error }

// SecurityContributor is implemented by auth middleware that should appear in
// the published document's securitySchemes. This is what keeps the spec honest
// about the authentication the gateway actually enforces.
type SecurityContributor interface {
	SecurityScheme() *yaml.Node
}

// Deps are the shared services handed to every constructor.
type Deps struct {
	Logger  *slog.Logger
	Stores  *store.Registry
	Metrics Metrics
}

// Metrics is the subset of instrumentation middleware needs. Keeping it an
// interface here means internal/mw never imports Prometheus.
type Metrics interface {
	CountAuth(middleware, result string)
	CountRateLimit(middleware, decision string)
	CountValidation(route, result string)
	CountCache(route, result string)
}

// Constructor builds an Instance from its configuration block.
type Constructor func(name string, raw json.RawMessage, deps Deps) (Instance, error)

// Registry maps a middleware `type` to its constructor.
type Registry struct {
	ctors map[string]Constructor
}

func NewRegistry() *Registry {
	return &Registry{ctors: map[string]Constructor{}}
}

func (r *Registry) Register(typ string, c Constructor) {
	r.ctors[typ] = c
}

func (r *Registry) Has(typ string) bool {
	_, ok := r.ctors[typ]
	return ok
}

// Types lists registered middleware types, sorted.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.ctors))
	for t := range r.ctors {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Build instantiates every middleware named in the configuration.
func (r *Registry) Build(cfg *config.Config, deps Deps) (map[string]Instance, error) {
	out := map[string]Instance{}
	names := make([]string, 0, len(cfg.Middleware))
	for n := range cfg.Middleware {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		def := cfg.Middleware[name]
		ctor, ok := r.ctors[def.Type]
		if !ok {
			// Close what we already built before giving up, or a failed reload
			// leaks a Redis client per attempt.
			CloseAll(out)
			return nil, fmt.Errorf("middleware %q: unknown type %q (known types: %v)", name, def.Type, r.Types())
		}
		inst, err := ctor(name, def.Raw, deps)
		if err != nil {
			CloseAll(out)
			return nil, fmt.Errorf("middleware %q (%s): %w", name, def.Type, err)
		}
		out[name] = inst
	}
	return out, nil
}

// CloseAll releases every instance that holds resources.
func CloseAll(instances map[string]Instance) {
	for _, i := range instances {
		if c, ok := i.(Closer); ok {
			_ = c.Close()
		}
	}
}

// entry pairs an instance with its position in the configured list, so that
// ordering within a category stays stable and predictable.
type entry struct {
	inst  Instance
	order int
	name  string
}

// Assemble wraps final with the named middleware, ordered by category.
//
// The returned handler is built once per route at runtime-build time; nothing
// here runs per request.
func Assemble(names []string, instances map[string]Instance, r *routetable.Route, final http.Handler) (http.Handler, error) {
	entries := make([]entry, 0, len(names))
	for i, n := range names {
		inst, ok := instances[n]
		if !ok {
			return nil, fmt.Errorf("route %s: no middleware named %q", r, n)
		}
		entries = append(entries, entry{inst: inst, order: i, name: n})
	}
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].inst.Category() != entries[b].inst.Category() {
			return entries[a].inst.Category() < entries[b].inst.Category()
		}
		return entries[a].order < entries[b].order
	})

	h := final
	// Wrap in reverse so the first entry ends up outermost.
	for i := len(entries) - 1; i >= 0; i-- {
		mw, err := entries[i].inst.ForRoute(r)
		if err != nil {
			return nil, fmt.Errorf("route %s: middleware %q: %w", r, entries[i].name, err)
		}
		if mw == nil {
			continue
		}
		h = mw(h)
	}
	return h, nil
}
