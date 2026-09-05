// Package router implements path matching for OpenAPI path templates.
//
// It is hand-rolled rather than borrowed because the gateway needs three things
// a general-purpose router will not agree to all at once: exact OpenAPI
// `{param}` semantics, a static-beats-parameter priority rule, and the matched
// route object handed straight back so the middleware chain can be selected
// without a second lookup.
package router

import (
	"net/url"
	"sort"
	"strings"
)

// Param is one captured path parameter.
type Param struct {
	Name  string
	Value string
}

// Params is the set captured by a match. It is a slice rather than a map: routes
// have a handful of parameters at most, and this keeps the hot path allocation
// free apart from the slice itself.
type Params []Param

func (p Params) Get(name string) (string, bool) {
	for _, kv := range p {
		if kv.Name == name {
			return kv.Value, true
		}
	}
	return "", false
}

// Router matches (method, path) to a payload of type T.
type Router[T any] struct {
	root *node[T]
	// templates records registered templates so duplicate registration can be
	// reported with both offenders.
	templates map[string]string
}

type node[T any] struct {
	// static children by literal segment.
	static map[string]*node[T]
	// param is the single wildcard child. Matching is name-agnostic: a
	// parameter segment matches anything, so one child is enough regardless of
	// what the various templates call it.
	param *node[T]
	// handlers by upper-case HTTP method, set on terminal nodes.
	handlers map[string]routeEntry[T]
	methods  []string // sorted, for the Allow header
}

// routeEntry pairs a payload with the parameter names of the template it was
// registered under.
//
// Names live here rather than on the node because two templates can share a
// position and disagree on the name: /enums/{id}/ and /enums/{enum_pk}/values/
// are different paths, and nested-resource generators emit that pattern
// constantly. Only the values are positional; the names come from whichever
// route actually matched.
type routeEntry[T any] struct {
	payload    T
	paramNames []string
}

func newNode[T any]() *node[T] {
	return &node[T]{static: map[string]*node[T]{}}
}

func New[T any]() *Router[T] {
	return &Router[T]{root: newNode[T](), templates: map[string]string{}}
}

// ConflictError reports two routes that cannot coexist.
type ConflictError struct {
	Method, Path, Existing string
}

func (e *ConflictError) Error() string {
	if e.Existing != "" {
		return "route conflict: " + e.Method + " " + e.Path + " conflicts with " + e.Existing
	}
	return "route conflict: " + e.Method + " " + e.Path + " is registered twice"
}

// Add registers a route. Template is an OpenAPI path such as /users/{id}.
func (r *Router[T]) Add(method, template string, payload T) error {
	method = strings.ToUpper(method)
	segs := splitPath(template)

	cur := r.root
	var paramNames []string
	for _, seg := range segs {
		if name, ok := paramName(seg); ok {
			if cur.param == nil {
				cur.param = newNode[T]()
			}
			paramNames = append(paramNames, name)
			cur = cur.param
			continue
		}
		next, ok := cur.static[seg]
		if !ok {
			next = newNode[T]()
			cur.static[seg] = next
		}
		cur = next
	}

	if cur.handlers == nil {
		cur.handlers = map[string]routeEntry[T]{}
	}
	// A genuine duplicate is two templates of the same shape for one method,
	// which normalise() detects regardless of how the parameters are spelled.
	if _, dup := cur.handlers[method]; dup {
		return &ConflictError{Method: method, Path: template, Existing: r.templates[method+" "+normalise(template)]}
	}
	cur.handlers[method] = routeEntry[T]{payload: payload, paramNames: paramNames}
	cur.methods = append(cur.methods, method)
	sort.Strings(cur.methods)
	r.templates[method+" "+normalise(template)] = method + " " + template
	return nil
}

// Result is the outcome of a match.
type Result[T any] struct {
	Payload T
	Params  Params
	// Found is true when a route matched both path and method.
	Found bool
	// PathMatched is true when the path exists but not for this method, in which
	// case Allow lists the methods that would work. This is what separates a
	// correct 405 from a lazy 404.
	PathMatched bool
	Allow       []string
}

// Match resolves a request. escapedPath should be the raw, still-encoded path
// (net/url's EscapedPath), so that an encoded %2F inside a parameter value does
// not split a segment.
func (r *Router[T]) Match(method, escapedPath string) Result[T] {
	var res Result[T]
	segs := splitPath(escapedPath)
	values := make([]string, 0, 4)

	n, values, ok := r.walk(r.root, segs, values)
	if !ok || n.handlers == nil {
		return res
	}
	res.PathMatched = true
	res.Allow = n.methods
	method = strings.ToUpper(method)
	entry, ok := n.handlers[method]
	if !ok {
		// HEAD falls back to GET, which is what every HTTP client expects.
		if method == "HEAD" {
			if entry, ok = n.handlers["GET"]; !ok {
				return res
			}
		} else {
			return res
		}
	}
	// Zip the positional values with the names of the route that matched.
	params := make(Params, 0, len(values))
	for i, v := range values {
		if i >= len(entry.paramNames) {
			break
		}
		if dec, err := url.PathUnescape(v); err == nil {
			v = dec
		}
		params = append(params, Param{Name: entry.paramNames[i], Value: v})
	}
	res.Payload = entry.payload
	res.Params = params
	res.Found = true
	return res
}

// walk descends the tree, preferring a static child and backtracking to the
// parameter child only when the static branch dead-ends. Without the backtrack,
// /users/me would shadow /users/{id} for every path below it.
func (r *Router[T]) walk(n *node[T], segs []string, values []string) (*node[T], []string, bool) {
	if len(segs) == 0 {
		if n.handlers == nil {
			return nil, values, false
		}
		return n, values, true
	}
	seg := segs[0]
	if child, ok := n.static[seg]; ok {
		if got, v, ok := r.walk(child, segs[1:], values); ok {
			return got, v, true
		}
	}
	if n.param != nil && seg != "" {
		if got, v, ok := r.walk(n.param, segs[1:], append(values, seg)); ok {
			return got, v, true
		}
	}
	return nil, values, false
}

// splitPath breaks a path into segments, ignoring leading and trailing slashes
// so that /users and /users/ address the same route.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// paramName reports whether a segment is a template parameter such as {id}.
func paramName(seg string) (string, bool) {
	if len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
		return seg[1 : len(seg)-1], true
	}
	return "", false
}

// normalise rewrites parameter names to a placeholder so that /users/{id} and
// /users/{userId} are recognised as the same route shape.
func normalise(template string) string {
	segs := splitPath(template)
	for i, s := range segs {
		if _, ok := paramName(s); ok {
			segs[i] = "{}"
		}
	}
	return "/" + strings.Join(segs, "/")
}
