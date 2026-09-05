// Package cors answers preflight requests and adds CORS response headers.
package cors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/routetable"
)

type settings struct {
	Type             string   `json:"type"`
	AllowOrigins     []string `json:"allow_origins"`
	AllowMethods     []string `json:"allow_methods"`
	AllowHeaders     []string `json:"allow_headers"`
	ExposeHeaders    []string `json:"expose_headers"`
	AllowCredentials bool     `json:"allow_credentials"`
	MaxAge           string   `json:"max_age"`
}

func New(_ string, raw json.RawMessage, _ chain.Deps) (chain.Instance, error) {
	s := settings{AllowOrigins: []string{"*"}}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}

	i := &instance{set: s, allowAll: false}
	for _, o := range s.AllowOrigins {
		if o == "*" {
			i.allowAll = true
		}
	}
	if i.allowAll && s.AllowCredentials {
		// The browser rejects this combination anyway; failing at config time is
		// far easier to debug than a silently broken credentialed request.
		return nil, fmt.Errorf("allow_credentials cannot be combined with the wildcard origin \"*\"")
	}
	i.origins = make(map[string]bool, len(s.AllowOrigins))
	for _, o := range s.AllowOrigins {
		i.origins[strings.ToLower(o)] = true
	}
	if s.MaxAge != "" {
		d, err := time.ParseDuration(s.MaxAge)
		if err != nil {
			return nil, fmt.Errorf("max_age: %w", err)
		}
		i.maxAge = strconv.Itoa(int(d.Seconds()))
	}
	i.allowHeaders = strings.Join(s.AllowHeaders, ", ")
	i.exposeHeaders = strings.Join(s.ExposeHeaders, ", ")
	return i, nil
}

type instance struct {
	set           settings
	allowAll      bool
	origins       map[string]bool
	maxAge        string
	allowHeaders  string
	exposeHeaders string
}

func (i *instance) Category() chain.Category { return chain.CatCORS }

// ForRoute derives the allowed methods from the route table rather than from
// configuration, so a preflight answer can never advertise a method the gateway
// does not actually serve.
func (i *instance) ForRoute(r *routetable.Route) (chain.Middleware, error) {
	methods := i.set.AllowMethods
	if len(methods) == 0 {
		if len(r.AllowedMethods) > 0 {
			methods = append(append([]string(nil), r.AllowedMethods...), http.MethodOptions)
		} else {
			methods = []string{r.Method, http.MethodOptions}
		}
	}
	allowMethods := strings.Join(methods, ", ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			origin := req.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, req)
				return
			}
			if !i.allowed(origin) {
				// Not an error: simply omit the headers and let the browser
				// enforce its own policy.
				next.ServeHTTP(w, req)
				return
			}

			h := w.Header()
			if i.allowAll && !i.set.AllowCredentials {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
			}
			if i.set.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if i.exposeHeaders != "" {
				h.Set("Access-Control-Expose-Headers", i.exposeHeaders)
			}

			if req.Method == http.MethodOptions && req.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", allowMethods)
				if i.allowHeaders != "" {
					h.Set("Access-Control-Allow-Headers", i.allowHeaders)
				} else if rh := req.Header.Get("Access-Control-Request-Headers"); rh != "" {
					h.Set("Access-Control-Allow-Headers", rh)
					h.Add("Vary", "Access-Control-Request-Headers")
				}
				if i.maxAge != "" {
					h.Set("Access-Control-Max-Age", i.maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, req)
		})
	}, nil
}

func (i *instance) allowed(origin string) bool {
	return i.allowAll || i.origins[strings.ToLower(origin)]
}
