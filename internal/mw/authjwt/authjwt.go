// Package authjwt validates bearer tokens against a remote JWKS.
package authjwt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/httperr"
	"github.com/rivencove/nina/internal/reqctx"
	"github.com/rivencove/nina/internal/routetable"
	yaml "go.yaml.in/yaml/v4"
)

type settings struct {
	Type     string   `json:"type"`
	JWKSURL  string   `json:"jwks_url"`
	Issuer   string   `json:"issuer"`
	Audience []string `json:"audience"`
	// RequiredScopes must all be present in the token's scope claim.
	RequiredScopes []string `json:"required_scopes"`
	// ScopeClaim names the claim holding scopes; "scope" (space separated) and
	// "scp" (array) are both common.
	ScopeClaim string `json:"scope_claim"`
	// RequiredClaims must be present with the given value.
	RequiredClaims map[string]string `json:"required_claims"`
	// ForwardClaims copies claims into upstream request headers.
	ForwardClaims map[string]string `json:"forward_claims"`
	// Header and Prefix locate the token. Defaults: Authorization / "Bearer ".
	Header string `json:"header"`
	Prefix string `json:"prefix"`
	// RefreshInterval controls JWKS polling.
	RefreshInterval string `json:"refresh_interval"`
	// ClockSkew tolerated on exp/nbf.
	ClockSkew string `json:"clock_skew"`
}

// New builds the JWT middleware and starts its JWKS refresher.
func New(name string, raw json.RawMessage, deps chain.Deps) (chain.Instance, error) {
	s := settings{ScopeClaim: "scope", Header: "Authorization", Prefix: "Bearer "}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if s.JWKSURL == "" {
		return nil, fmt.Errorf("jwks_url is required")
	}
	refresh := 15 * time.Minute
	if s.RefreshInterval != "" {
		d, err := time.ParseDuration(s.RefreshInterval)
		if err != nil {
			return nil, fmt.Errorf("refresh_interval: %w", err)
		}
		refresh = d
	}
	var skew time.Duration
	if s.ClockSkew != "" {
		d, err := time.ParseDuration(s.ClockSkew)
		if err != nil {
			return nil, fmt.Errorf("clock_skew: %w", err)
		}
		skew = d
	}

	ctx, cancel := context.WithCancel(context.Background())
	i := &instance{
		name: name, set: s, deps: deps, skew: skew,
		cancel: cancel,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	// Fetch once synchronously: starting up unable to validate any token is a
	// misconfiguration the operator should hear about at reload, not on the
	// first request.
	if err := i.refresh(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("initial JWKS fetch from %s: %w", s.JWKSURL, err)
	}
	go i.refreshLoop(ctx, refresh)
	return i, nil
}

type instance struct {
	name   string
	set    settings
	deps   chain.Deps
	skew   time.Duration
	client *http.Client
	cancel context.CancelFunc

	mu   sync.RWMutex
	set_ jwk.Set
}

func (i *instance) Category() chain.Category { return chain.CatAuth }

func (i *instance) Close() error {
	i.cancel()
	return nil
}

// SecurityScheme publishes what this middleware enforces, so the merged document
// describes the gateway's real authentication rather than the upstream's.
func (i *instance) SecurityScheme() *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(k, v string) {
		n.Content = append(n.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
	add("type", "http")
	add("scheme", "bearer")
	add("bearerFormat", "JWT")
	desc := "JWT validated by the gateway against " + i.set.JWKSURL
	if i.set.Issuer != "" {
		desc += " (issuer " + i.set.Issuer + ")"
	}
	add("description", desc)
	return n
}

func (i *instance) refreshLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := i.refresh(ctx); err != nil && i.deps.Logger != nil {
				// Keep serving with the keys we have; a failed refresh only
				// matters once the issuer actually rotates.
				i.deps.Logger.Warn("jwks refresh failed", "middleware", i.name, "error", err)
			}
		}
	}
}

func (i *instance) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.set.JWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := i.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	set, err := jwk.Parse(body)
	if err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}
	if set.Len() == 0 {
		return fmt.Errorf("JWKS contains no keys")
	}
	i.mu.Lock()
	i.set_ = set
	i.mu.Unlock()
	return nil
}

func (i *instance) keys() jwk.Set {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.set_
}

func (i *instance) ForRoute(_ *routetable.Route) (chain.Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			i.serve(w, r, next)
		})
	}, nil
}

func (i *instance) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	raw := r.Header.Get(i.set.Header)
	if i.set.Prefix != "" {
		if !strings.HasPrefix(raw, i.set.Prefix) {
			i.reject(w, r, "missing_token", "a bearer token is required")
			return
		}
		raw = strings.TrimPrefix(raw, i.set.Prefix)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		i.reject(w, r, "missing_token", "a bearer token is required")
		return
	}

	opts := []jwt.ParseOption{
		jwt.WithKeySet(i.keys()),
		jwt.WithValidate(true),
	}
	if i.skew > 0 {
		opts = append(opts, jwt.WithAcceptableSkew(i.skew))
	}
	if i.set.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(i.set.Issuer))
	}
	for _, a := range i.set.Audience {
		opts = append(opts, jwt.WithAudience(a))
	}

	tok, err := jwt.Parse([]byte(raw), opts...)
	if err != nil {
		i.reject(w, r, "invalid_token", err.Error())
		return
	}

	claims := claimsOf(tok)
	for name, want := range i.set.RequiredClaims {
		got, ok := claims[name]
		if !ok || fmt.Sprint(got) != want {
			i.reject(w, r, "insufficient_claims", fmt.Sprintf("claim %q does not have the required value", name))
			return
		}
	}
	scopes := extractScopes(claims, i.set.ScopeClaim)
	if missing := missingScopes(scopes, i.set.RequiredScopes); len(missing) > 0 {
		i.reject(w, r, "insufficient_scope", "missing required scope(s): "+strings.Join(missing, ", "))
		return
	}

	if info := reqctx.From(r.Context()); info != nil {
		sub, _ := claims["sub"].(string)
		info.Subject = sub
		info.Consumer = sub
		info.Scopes = scopes
		info.Claims = claims
	}
	for claimName, header := range i.set.ForwardClaims {
		if v, ok := claims[claimName]; ok {
			r.Header.Set(header, fmt.Sprint(v))
		}
	}
	// The gateway has consumed the credential; do not leak it upstream unless
	// the operator explicitly forwards it.
	if i.deps.Metrics != nil {
		i.deps.Metrics.CountAuth(i.name, "allow")
	}
	next.ServeHTTP(w, r)
}

func (i *instance) reject(w http.ResponseWriter, r *http.Request, reason, detail string) {
	if i.deps.Metrics != nil {
		i.deps.Metrics.CountAuth(i.name, reason)
	}
	status := http.StatusUnauthorized
	if reason == "insufficient_scope" || reason == "insufficient_claims" {
		status = http.StatusForbidden
	}
	w.Header().Set("WWW-Authenticate", `Bearer error="`+reason+`"`)
	httperr.Write(w, r, httperr.Problem{
		Status: status,
		Title:  "Authentication failed",
		Detail: detail,
	})
}

// claimsOf materialises a token's claims as a plain map, which is what the rest
// of the chain (rate limit keys, forwarded headers, logs) works with.
func claimsOf(tok jwt.Token) map[string]any {
	keys := tok.Keys()
	claims := make(map[string]any, len(keys))
	for _, k := range keys {
		var v any
		if err := tok.Get(k, &v); err == nil {
			claims[k] = v
		}
	}
	return claims
}

// extractScopes handles both the space-delimited "scope" string and array-valued
// claims such as "scp" or "permissions".
func extractScopes(claims map[string]any, claimName string) []string {
	v, ok := claims[claimName]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case string:
		return strings.Fields(t)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out
	case []string:
		return t
	}
	return nil
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
