// Package apikey authenticates callers by a shared key mapped to a consumer.
package apikey

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/httperr"
	"github.com/rivencove/nina/internal/reqctx"
	"github.com/rivencove/nina/internal/routetable"
	yaml "go.yaml.in/yaml/v4"
	sigsyaml "sigs.k8s.io/yaml"
)

type settings struct {
	Type string `json:"type"`
	// In is "header" or "query".
	In   string `json:"in"`
	Name string `json:"name"`
	// Keys maps a consumer name to its key.
	Keys map[string]string `json:"keys"`
	// KeysFile holds the same mapping outside the main config, so keys need not
	// live in the same file (or the same secret) as the routing rules.
	KeysFile string `json:"keys_file"`
	// Scopes grants scopes per consumer, so routes can require them.
	Scopes map[string][]string `json:"scopes"`
	// RequiredScopes must all be granted to the calling consumer.
	RequiredScopes []string `json:"required_scopes"`
	// ForwardConsumerHeader receives the resolved consumer name upstream.
	ForwardConsumerHeader string `json:"forward_consumer_header"`
	// StripCredential removes the key from the upstream request. On by default:
	// an upstream should not be able to replay a client's gateway credential.
	StripCredential *bool `json:"strip_credential"`
}

func New(name string, raw json.RawMessage, deps chain.Deps) (chain.Instance, error) {
	s := settings{In: "header", Name: "X-API-Key"}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if s.In != "header" && s.In != "query" {
		return nil, fmt.Errorf("in must be header or query, got %q", s.In)
	}
	if s.KeysFile != "" {
		loaded, err := loadKeysFile(s.KeysFile)
		if err != nil {
			return nil, err
		}
		if s.Keys == nil {
			s.Keys = map[string]string{}
		}
		for consumer, key := range loaded {
			s.Keys[consumer] = key
		}
	}
	if len(s.Keys) == 0 {
		return nil, fmt.Errorf("no keys configured (set `keys` or `keys_file`)")
	}

	i := &instance{name: name, set: s, deps: deps, byHash: map[[32]byte]string{}}
	for consumer, key := range s.Keys {
		if key == "" {
			return nil, fmt.Errorf("consumer %q has an empty key", consumer)
		}
		h := sha256.Sum256([]byte(key))
		if prev, dup := i.byHash[h]; dup {
			return nil, fmt.Errorf("consumers %q and %q share the same key", prev, consumer)
		}
		i.byHash[h] = consumer
	}
	i.strip = s.StripCredential == nil || *s.StripCredential
	return i, nil
}

type instance struct {
	name  string
	set   settings
	deps  chain.Deps
	strip bool
	// byHash indexes consumers by a hash of their key, so lookup is a constant
	// time map probe on a fixed-size value rather than a walk over every key.
	byHash map[[32]byte]string
}

func (i *instance) Category() chain.Category { return chain.CatAuth }

func (i *instance) SecurityScheme() *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(k, v string) {
		n.Content = append(n.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
	add("type", "apiKey")
	add("in", i.set.In)
	add("name", i.set.Name)
	add("description", "API key issued and verified by the gateway")
	return n
}

func (i *instance) ForRoute(_ *routetable.Route) (chain.Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			i.serve(w, r, next)
		})
	}, nil
}

func (i *instance) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	var presented string
	if i.set.In == "header" {
		presented = r.Header.Get(i.set.Name)
	} else {
		presented = r.URL.Query().Get(i.set.Name)
	}
	if presented == "" {
		i.reject(w, r, "missing_key", http.StatusUnauthorized, "an API key is required")
		return
	}

	h := sha256.Sum256([]byte(presented))
	consumer, ok := i.byHash[h]
	if !ok {
		// Burn a comparison anyway so a miss costs roughly what a hit does.
		subtle.ConstantTimeCompare(h[:], h[:])
		i.reject(w, r, "invalid_key", http.StatusUnauthorized, "the API key is not recognised")
		return
	}

	scopes := i.set.Scopes[consumer]
	if missing := missingScopes(scopes, i.set.RequiredScopes); len(missing) > 0 {
		i.reject(w, r, "insufficient_scope", http.StatusForbidden,
			"consumer is missing scope(s): "+strings.Join(missing, ", "))
		return
	}

	if info := reqctx.From(r.Context()); info != nil {
		info.Consumer = consumer
		info.Subject = consumer
		info.Scopes = scopes
	}
	if i.strip {
		if i.set.In == "header" {
			r.Header.Del(i.set.Name)
		} else {
			q := r.URL.Query()
			q.Del(i.set.Name)
			r.URL.RawQuery = q.Encode()
		}
	}
	if i.set.ForwardConsumerHeader != "" {
		r.Header.Set(i.set.ForwardConsumerHeader, consumer)
	}
	if i.deps.Metrics != nil {
		i.deps.Metrics.CountAuth(i.name, "allow")
	}
	next.ServeHTTP(w, r)
}

func (i *instance) reject(w http.ResponseWriter, r *http.Request, reason string, status int, detail string) {
	if i.deps.Metrics != nil {
		i.deps.Metrics.CountAuth(i.name, reason)
	}
	httperr.Write(w, r, httperr.Problem{
		Status: status,
		Title:  "Authentication failed",
		Detail: detail,
	})
}

func loadKeysFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("keys_file: %w", err)
	}
	var out map[string]string
	if err := sigsyaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("keys_file %s: %w", path, err)
	}
	return out, nil
}

func missingScopes(have, want []string) []string {
	if len(want) == 0 {
		return nil
	}
	set := make(map[string]bool, len(have))
	for _, s := range have {
		set[s] = true
	}
	var missing []string
	for _, s := range want {
		if !set[s] {
			missing = append(missing, s)
		}
	}
	return missing
}
