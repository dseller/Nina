// Package headers adds, sets and removes request and response headers.
package headers

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/routetable"
)

type headerRules struct {
	Set    map[string]string `json:"set"`
	Add    map[string]string `json:"add"`
	Remove []string          `json:"remove"`
}

type settings struct {
	Type     string      `json:"type"`
	Request  headerRules `json:"request"`
	Response headerRules `json:"response"`
}

func New(_ string, raw json.RawMessage, _ chain.Deps) (chain.Instance, error) {
	var s settings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	return &instance{set: s}, nil
}

type instance struct{ set settings }

func (i *instance) Category() chain.Category { return chain.CatTransform }

func (i *instance) ForRoute(_ *routetable.Route) (chain.Middleware, error) {
	req, resp := i.set.Request, i.set.Response
	noResponseWork := len(resp.Set) == 0 && len(resp.Add) == 0 && len(resp.Remove) == 0

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apply(r.Header, req)
			if noResponseWork {
				next.ServeHTTP(w, r)
				return
			}
			// Response headers have to be written before the status line, so the
			// rules are applied at WriteHeader time rather than after the fact.
			next.ServeHTTP(&responseWriter{ResponseWriter: w, rules: resp}, r)
		})
	}, nil
}

func apply(h http.Header, rules headerRules) {
	for k, v := range rules.Set {
		h.Set(k, v)
	}
	for k, v := range rules.Add {
		h.Add(k, v)
	}
	for _, k := range rules.Remove {
		h.Del(k)
	}
}

type responseWriter struct {
	http.ResponseWriter
	rules   headerRules
	written bool
}

func (w *responseWriter) WriteHeader(code int) {
	if !w.written {
		w.written = true
		apply(w.Header(), w.rules)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps streaming responses working through the wrapper.
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
