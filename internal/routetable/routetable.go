// Package routetable builds the gateway's intermediate representation.
//
// This is the load-bearing idea of the whole gateway: configuration and upstream
// specs are resolved once into a Table, and both the HTTP router and the
// published OpenAPI document are then derived from that same Table. A route that
// is not served cannot appear in the document, and a route that is served always
// does unless it was explicitly hidden by tag (see Route.Hidden), which is the
// one direction the invariant can be relaxed in.
package routetable

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/oas"
	yaml "go.yaml.in/yaml/v4"
)

// Route is one published gateway endpoint, mapped 1:1 onto an upstream operation.
type Route struct {
	Method      string // upper-case
	GatewayPath string // OpenAPI template, e.g. /api/users/{id}
	OperationID string // gateway-level, e.g. users_getUser

	Backend             *config.Backend
	UpstreamPath        string
	UpstreamOperationID string

	// Middleware names, in configuration order. The chain builder imposes
	// category ordering on top of this.
	Middleware []string
	Timeout    time.Duration

	// AllowedMethods is every method served at this gateway path, so CORS can
	// advertise the truth rather than guessing from a single operation.
	AllowedMethods []string

	// Hidden means the operation carried one of spec.hide_tags and was left out
	// of the published document. It is served exactly like any other route: the
	// same middleware chain, the same request validation. Only the documentation
	// is withheld.
	Hidden bool

	// Op and Spec are the upstream operation and its document, retained so that
	// request validation compiles the very schemas that were published.
	Op   *oas.Operation
	Spec *oas.Spec
}

func (r *Route) String() string { return r.Method + " " + r.GatewayPath }

// Table is the resolved gateway.
type Table struct {
	Routes []*Route
	// Spec is the merged OpenAPI 3.1 document rendered from Routes, minus any
	// route whose Hidden flag is set.
	Spec *yaml.Node
	// SpecYAML and SpecJSON are the serialised forms, built once at load.
	SpecYAML []byte
	SpecJSON []byte
	// Degraded lists backends serving a stale cached spec.
	Degraded []string
}

// HiddenRoutes returns the routes served but kept out of the published document.
func (t *Table) HiddenRoutes() []*Route {
	var out []*Route
	for _, r := range t.Routes {
		if r.Hidden {
			out = append(out, r)
		}
	}
	return out
}

// SecuritySchemeFunc lets the caller contribute securitySchemes derived from the
// auth middleware actually configured, without this package having to know what
// a JWT is.
type SecuritySchemeFunc func(name string, def config.MiddlewareDef) (scheme *yaml.Node, ok bool)

// BuildInput carries everything Build needs.
type BuildInput struct {
	Config *config.Config
	// Specs is keyed by backend name.
	Specs map[string]*oas.Spec
	// Degraded lists backends whose spec came from cache.
	Degraded []string
	// SecurityScheme is optional.
	SecurityScheme SecuritySchemeFunc
}

// Build resolves configuration plus specs into a Table.
func Build(in BuildInput) (*Table, error) {
	cfg := in.Config
	var routes []*Route
	var exposed []oas.ExposedOp
	sources := map[string]*oas.SpecSource{}

	for i, e := range cfg.Expose {
		backend := cfg.BackendByName(e.Backend)
		if backend == nil {
			return nil, fmt.Errorf("expose[%d]: no backend named %q", i, e.Backend)
		}
		spec := in.Specs[e.Backend]
		if spec == nil {
			return nil, fmt.Errorf("expose[%d]: no spec loaded for backend %q", i, e.Backend)
		}
		src, ok := sources[e.Backend]
		if !ok {
			src = &oas.SpecSource{Spec: spec, Backend: backend.Name, Namespace: backend.Namespace}
			sources[e.Backend] = src
		}

		matched := 0
		for _, op := range spec.Operations() {
			include, err := selects(op.OperationID, e.Include, e.Exclude)
			if err != nil {
				return nil, fmt.Errorf("expose[%d] (backend %s): %w", i, e.Backend, err)
			}
			if !include {
				continue
			}
			ov, hasOverride := e.Overrides[op.OperationID]
			if hasOverride && ov.Disabled {
				continue
			}
			matched++

			gatewayPath := ov.Path
			if gatewayPath == "" {
				gatewayPath = joinPath(e.Prefix, stripPrefix(op.Path, backend.StripPrefix))
			}

			mw := e.Middleware
			if hasOverride && ov.Middleware != nil {
				// An override replaces rather than extends: partial inheritance of
				// a security chain is exactly the kind of subtlety that produces
				// an unauthenticated endpoint nobody noticed.
				mw = ov.Middleware
			}

			timeout := backend.Timeout.Or(cfg.Defaults.Timeout.Std())
			if hasOverride && ov.Timeout != 0 {
				timeout = ov.Timeout.Std()
			}

			opID := operationID(backend.Name, op.OperationID)
			hidden := cfg.Spec.Hidden(op.Tags)
			routes = append(routes, &Route{
				Method:              op.Method,
				GatewayPath:         gatewayPath,
				OperationID:         opID,
				Backend:             backend,
				UpstreamPath:        op.Path,
				UpstreamOperationID: op.OperationID,
				Middleware:          append([]string(nil), mw...),
				Timeout:             timeout,
				Op:                  op,
				Spec:                spec,
				Hidden:              hidden,
			})
			if hidden {
				// Nothing about the route changes; it simply never reaches the
				// merger, so neither it nor the components only it references
				// appear in the published document.
				continue
			}
			exposed = append(exposed, oas.ExposedOp{
				Source:      src,
				Op:          op,
				GatewayPath: gatewayPath,
				OperationID: opID,
			})
		}

		if matched == 0 {
			// Silently serving nothing for a configured backend is nearly always a
			// mistake in include/exclude globs.
			return nil, fmt.Errorf("expose[%d] (backend %s): include/exclude selected no operations out of %d; check the globs",
				i, e.Backend, len(spec.Operations()))
		}
	}

	// Overrides can point a route anywhere, so verify parameter agreement before
	// the merge: a gateway path must declare exactly the parameters its upstream
	// path does, or the proxy could not rebuild the upstream URL.
	for _, r := range routes {
		if err := checkParams(r); err != nil {
			return nil, err
		}
	}

	// Collisions are detected here rather than only in the merger because a
	// hidden route never reaches the merger. Two routes claiming one slot must
	// fail the build whether or not either of them is documented.
	claimed := map[string]*Route{}
	for _, r := range routes {
		key := r.Method + " " + r.GatewayPath
		if prev, dup := claimed[key]; dup {
			return nil, &oas.CollisionError{
				Method: r.Method,
				Path:   r.GatewayPath,
				A:      prev.Backend.Name + "." + prev.UpstreamOperationID,
				B:      r.Backend.Name + "." + r.UpstreamOperationID,
			}
		}
		claimed[key] = r
	}

	merged, err := oas.Merge(exposed, oas.MergeOptions{
		Title:           cfg.Spec.Title,
		Version:         cfg.Spec.Version,
		Description:     cfg.Spec.Description,
		Servers:         cfg.Spec.Servers,
		Dedupe:          cfg.Spec.Dedupe == nil || *cfg.Spec.Dedupe,
		SecuritySchemes: securitySchemes(cfg, in.SecurityScheme),
	})
	if err != nil {
		return nil, err
	}

	yamlBytes, err := oas.Render(merged)
	if err != nil {
		return nil, fmt.Errorf("render merged spec: %w", err)
	}
	jsonBytes, err := oas.RenderJSON(merged)
	if err != nil {
		return nil, fmt.Errorf("render merged spec as JSON: %w", err)
	}

	sort.SliceStable(routes, func(a, b int) bool {
		if routes[a].GatewayPath != routes[b].GatewayPath {
			return routes[a].GatewayPath < routes[b].GatewayPath
		}
		return routes[a].Method < routes[b].Method
	})

	byPath := map[string][]string{}
	for _, r := range routes {
		byPath[r.GatewayPath] = append(byPath[r.GatewayPath], r.Method)
	}
	for _, r := range routes {
		methods := append([]string(nil), byPath[r.GatewayPath]...)
		sort.Strings(methods)
		r.AllowedMethods = methods
	}

	return &Table{
		Routes:   routes,
		Spec:     merged,
		SpecYAML: yamlBytes,
		SpecJSON: jsonBytes,
		Degraded: in.Degraded,
	}, nil
}

func securitySchemes(cfg *config.Config, fn SecuritySchemeFunc) map[string]*yaml.Node {
	if fn == nil {
		return nil
	}
	out := map[string]*yaml.Node{}
	names := make([]string, 0, len(cfg.Middleware))
	for n := range cfg.Middleware {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if scheme, ok := fn(n, cfg.Middleware[n]); ok {
			out[n] = scheme
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// operationID namespaces the upstream id so gateway ids stay unique.
func operationID(backend, upstream string) string {
	return sanitise(backend) + "_" + upstream
}

func sanitise(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// selects applies include then exclude globs to an operationId.
func selects(id string, include, exclude []string) (bool, error) {
	if len(include) > 0 {
		any := false
		for _, g := range include {
			ok, err := path.Match(g, id)
			if err != nil {
				return false, fmt.Errorf("invalid include pattern %q: %w", g, err)
			}
			if ok {
				any = true
				break
			}
		}
		if !any {
			return false, nil
		}
	}
	for _, g := range exclude {
		ok, err := path.Match(g, id)
		if err != nil {
			return false, fmt.Errorf("invalid exclude pattern %q: %w", g, err)
		}
		if ok {
			return false, nil
		}
	}
	return true, nil
}

// stripPrefix removes a leading path prefix from an upstream path, so a backend
// that serves /v1/users can be mounted at /users without doubling the segment.
func stripPrefix(p, prefix string) string {
	if prefix == "" || prefix == "/" {
		return p
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if p == prefix {
		return "/"
	}
	if strings.HasPrefix(p, prefix+"/") {
		return p[len(prefix):]
	}
	return p
}

func joinPath(prefix, p string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	if p == "" || p == "/" {
		if prefix == "" {
			return "/"
		}
		return prefix
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return prefix + p
}

// checkParams verifies that the gateway path and upstream path declare the same
// set of template parameters.
func checkParams(r *Route) error {
	up := templateParams(r.UpstreamPath)
	gw := templateParams(r.GatewayPath)
	for name := range up {
		if !gw[name] {
			return fmt.Errorf("route %s (%s.%s): upstream path %s needs parameter {%s}, which the gateway path %s does not provide",
				r, r.Backend.Name, r.UpstreamOperationID, r.UpstreamPath, name, r.GatewayPath)
		}
	}
	return nil
}

func templateParams(p string) map[string]bool {
	out := map[string]bool{}
	for _, seg := range strings.Split(p, "/") {
		if len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			out[seg[1:len(seg)-1]] = true
		}
	}
	return out
}
