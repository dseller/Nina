// Package ratelimit meters requests per key against a shared or local store.
package ratelimit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/httperr"
	"github.com/rivencove/nina/internal/reqctx"
	"github.com/rivencove/nina/internal/routetable"
	"github.com/rivencove/nina/internal/store"
)

type settings struct {
	Type  string `json:"type"`
	Store string `json:"store"` // memory (default) or redis
	Limit int    `json:"limit"`
	// Window is a duration string such as "1m".
	Window string `json:"window"`
	// Key is a small expression over request context. Supported forms:
	//   ip, consumer, jwt.<claim>, header.<Name>, query.<name>, route
	// Several may be combined with "+".
	Key string `json:"key"`
	// MaxKeys bounds the in-memory key set.
	MaxKeys int `json:"max_keys"`
	// FailOpen decides what happens when the store is unreachable.
	FailOpen *bool `json:"fail_open"`
	// Headers emits X-RateLimit-* response headers.
	Headers *bool `json:"headers"`
}

func New(name string, raw json.RawMessage, deps chain.Deps) (chain.Instance, error) {
	s := settings{Store: "memory", Key: "ip"}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if s.Limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}
	window := time.Minute
	if s.Window != "" {
		d, err := time.ParseDuration(s.Window)
		if err != nil {
			return nil, fmt.Errorf("window: %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("window must be positive")
		}
		window = d
	}
	extractors, err := parseKey(s.Key)
	if err != nil {
		return nil, err
	}
	failOpen := s.FailOpen == nil || *s.FailOpen
	lim, err := deps.Stores.Limiter(name, s.Store, s.MaxKeys, failOpen)
	if err != nil {
		return nil, err
	}
	return &instance{
		name: name, set: s, deps: deps,
		limiter: lim, window: window, keyFns: extractors,
		headers: s.Headers == nil || *s.Headers,
	}, nil
}

type instance struct {
	name    string
	set     settings
	deps    chain.Deps
	limiter store.Limiter
	window  time.Duration
	keyFns  []extractor
	headers bool
}

func (i *instance) Category() chain.Category { return chain.CatRateLimit }

type extractor struct {
	kind string
	arg  string
}

// parseKey compiles the key expression once, at build time.
func parseKey(expr string) ([]extractor, error) {
	if strings.TrimSpace(expr) == "" {
		expr = "ip"
	}
	var out []extractor
	for _, part := range strings.Split(expr, "+") {
		part = strings.TrimSpace(part)
		kind, arg, _ := strings.Cut(part, ".")
		switch kind {
		case "ip", "consumer", "route":
			if arg != "" {
				return nil, fmt.Errorf("key %q does not take an argument", kind)
			}
		case "jwt", "header", "query":
			if arg == "" {
				return nil, fmt.Errorf("key %q needs an argument, e.g. %s.name", kind, kind)
			}
		default:
			return nil, fmt.Errorf("unknown key component %q (expected ip, consumer, route, jwt.<claim>, header.<Name> or query.<name>)", part)
		}
		out = append(out, extractor{kind: kind, arg: arg})
	}
	return out, nil
}

func (i *instance) buildKey(r *http.Request, info *reqctx.Info) string {
	var b strings.Builder
	b.WriteString(i.name)
	for _, e := range i.keyFns {
		b.WriteByte('|')
		switch e.kind {
		case "ip":
			if info != nil {
				b.WriteString(info.ClientIP)
			}
		case "consumer":
			if info != nil {
				b.WriteString(info.Consumer)
			}
		case "route":
			if info != nil {
				b.WriteString(info.OperationID)
			}
		case "jwt":
			if info != nil && info.Claims != nil {
				if v, ok := info.Claims[e.arg]; ok {
					fmt.Fprint(&b, v)
				}
			}
		case "header":
			b.WriteString(r.Header.Get(e.arg))
		case "query":
			b.WriteString(r.URL.Query().Get(e.arg))
		}
	}
	return b.String()
}

func (i *instance) ForRoute(_ *routetable.Route) (chain.Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := reqctx.From(r.Context())
			key := i.buildKey(r, info)

			d, err := i.limiter.Allow(r.Context(), key, i.set.Limit, i.window)
			if err != nil {
				if i.deps.Metrics != nil {
					i.deps.Metrics.CountRateLimit(i.name, "error")
				}
				httperr.Write(w, r, httperr.Problem{
					Status: http.StatusServiceUnavailable,
					Title:  "Rate limiting is unavailable",
					Detail: "the rate limit store could not be reached",
				})
				return
			}
			if d.Degraded && i.deps.Metrics != nil {
				i.deps.Metrics.CountRateLimit(i.name, "degraded")
			}
			if i.headers {
				h := w.Header()
				h.Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
				h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
			}
			if !d.Allowed {
				if i.deps.Metrics != nil {
					i.deps.Metrics.CountRateLimit(i.name, "block")
				}
				if d.RetryAfter > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(int(d.RetryAfter.Seconds()+0.999)))
				}
				httperr.Write(w, r, httperr.Problem{
					Status: http.StatusTooManyRequests,
					Title:  "Rate limit exceeded",
					Detail: fmt.Sprintf("more than %d requests per %s", i.set.Limit, i.window),
				})
				return
			}
			if i.deps.Metrics != nil {
				i.deps.Metrics.CountRateLimit(i.name, "allow")
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}
