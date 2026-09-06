package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Errors is a collection of validation problems. Reporting every problem at once
// beats making an operator fix one typo per restart.
type Errors []error

func (e Errors) Error() string {
	if len(e) == 1 {
		return e[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d configuration problems:", len(e))
	for _, err := range e {
		b.WriteString("\n  - ")
		b.WriteString(err.Error())
	}
	return b.String()
}

type problems struct{ errs Errors }

func (p *problems) addf(path, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

var nameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// componentNameRe is the character set OpenAPI permits in a components key. A
// name outside it produces a document tools reject, so it is worth catching
// here rather than in whatever consumes the published spec.
var componentNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// applyDefaults fills in the values the rest of the system assumes are present.
func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = Duration(5 * time.Second)
	}
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = Duration(60 * time.Second)
	}
	if c.Server.MaxRequestBody == 0 {
		c.Server.MaxRequestBody = 8 << 20 // 8 MiB
	}
	if c.Defaults.Timeout == 0 {
		c.Defaults.Timeout = Duration(10 * time.Second)
	}
	if c.Spec.Title == "" {
		c.Spec.Title = "API Gateway"
	}
	if c.Spec.Version == "" {
		c.Spec.Version = "1.0.0"
	}
	if c.Spec.Dedupe == nil {
		t := true
		c.Spec.Dedupe = &t
	}
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.Namespace == "" {
			b.Namespace = namespaceFor(b.Name)
		}
		if b.LoadBalance == "" {
			b.LoadBalance = "round_robin"
		}
		if b.Spec.OnError == "" {
			b.Spec.OnError = "fail"
		}
		if b.HealthCheck != nil {
			hc := b.HealthCheck
			if hc.Path == "" {
				hc.Path = "/healthz"
			}
			if hc.Interval == 0 {
				hc.Interval = Duration(10 * time.Second)
			}
			if hc.Timeout == 0 {
				hc.Timeout = Duration(2 * time.Second)
			}
			if hc.UnhealthyAfter == 0 {
				hc.UnhealthyAfter = 3
			}
			if hc.HealthyAfter == 0 {
				hc.HealthyAfter = 2
			}
		}
	}
	if c.Defaults.Retry != nil {
		normaliseRetry(c.Defaults.Retry)
	}
	for i := range c.Backends {
		if c.Backends[i].Retry != nil {
			normaliseRetry(c.Backends[i].Retry)
		}
	}
}

func normaliseRetry(r *Retry) {
	if r.Backoff == 0 {
		r.Backoff = Duration(50 * time.Millisecond)
	}
	if r.IdempotentOnly == nil {
		t := true
		r.IdempotentOnly = &t
	}
}

// namespaceFor turns a backend name into a component-name prefix:
// "user-profiles" becomes "UserProfiles".
func namespaceFor(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		switch {
		case r == '-' || r == '_' || r == ' ':
			upper = true
		case upper:
			b.WriteString(strings.ToUpper(string(r)))
			upper = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Validate checks the configuration for problems that would otherwise surface as
// confusing runtime behaviour.
func (c *Config) Validate() error {
	var p problems

	if c.Version != 1 {
		p.addf("version", "unsupported config version %d, expected 1", c.Version)
	}
	if c.Server.Listen == "" {
		p.addf("server.listen", "must not be empty")
	}

	backends := map[string]*Backend{}
	for i := range c.Backends {
		b := &c.Backends[i]
		path := fmt.Sprintf("backends[%d]", i)
		if b.Name == "" {
			p.addf(path+".name", "must not be empty")
		} else if !nameRe.MatchString(b.Name) {
			p.addf(path+".name", "%q must start with a letter and contain only letters, digits, - and _", b.Name)
		} else if _, dup := backends[b.Name]; dup {
			p.addf(path+".name", "duplicate backend name %q", b.Name)
		} else {
			backends[b.Name] = b
			path = "backends." + b.Name
		}

		switch {
		case b.Spec.File == "" && b.Spec.URL == "":
			p.addf(path+".spec", "needs either `file` or `url`")
		case b.Spec.File != "" && b.Spec.URL != "":
			p.addf(path+".spec", "set only one of `file` or `url`")
		}
		if b.Spec.URL != "" {
			if u, err := url.Parse(b.Spec.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				p.addf(path+".spec.url", "must be an http(s) URL, got %q", b.Spec.URL)
			}
		}
		if b.Spec.OnError != "fail" && b.Spec.OnError != "stale" {
			p.addf(path+".spec.on_error", "must be \"fail\" or \"stale\", got %q", b.Spec.OnError)
		}
		if b.Spec.Refresh != 0 && b.Spec.Refresh.Std() < time.Second {
			p.addf(path+".spec.refresh", "must be at least 1s, got %s", b.Spec.Refresh.Std())
		}

		if len(b.Hosts) == 0 {
			p.addf(path+".hosts", "at least one upstream host is required")
		}
		for j, h := range b.Hosts {
			u, err := url.Parse(h)
			if err != nil || u.Scheme == "" || u.Host == "" {
				p.addf(fmt.Sprintf("%s.hosts[%d]", path, j), "must be an absolute URL like https://svc.internal, got %q", h)
				continue
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				p.addf(fmt.Sprintf("%s.hosts[%d]", path, j), "unsupported scheme %q", u.Scheme)
			}
		}
		switch b.LoadBalance {
		case "round_robin", "least_conn", "random":
		default:
			p.addf(path+".load_balance", "must be round_robin, least_conn or random, got %q", b.LoadBalance)
		}
		if b.StripPrefix != "" && !strings.HasPrefix(b.StripPrefix, "/") {
			p.addf(path+".strip_prefix", "must start with /, got %q", b.StripPrefix)
		}
		validateSchemeNames(&p, path, b.SecuritySchemeNames)
		if b.CircuitBreaker != nil {
			validateBreaker(&p, path+".circuit_breaker", b.CircuitBreaker)
		}
		if b.Retry != nil {
			validateRetry(&p, path+".retry", b.Retry)
		}
	}
	if c.Defaults.CircuitBreaker != nil {
		validateBreaker(&p, "defaults.circuit_breaker", c.Defaults.CircuitBreaker)
	}
	if c.Defaults.Retry != nil {
		validateRetry(&p, "defaults.retry", c.Defaults.Retry)
	}

	for name, def := range c.Middleware {
		path := "middleware." + name
		if !nameRe.MatchString(name) {
			p.addf(path, "invalid middleware name %q", name)
		}
		if def.Type == "" {
			p.addf(path+".type", "must not be empty")
		}
	}

	if len(c.Expose) == 0 {
		p.addf("expose", "at least one expose entry is required, otherwise the gateway serves nothing")
	}
	seenPrefix := map[string]string{}
	for i, e := range c.Expose {
		path := fmt.Sprintf("expose[%d]", i)
		if e.Backend == "" {
			p.addf(path+".backend", "must not be empty")
		} else if _, ok := backends[e.Backend]; !ok {
			p.addf(path+".backend", "no backend named %q is defined", e.Backend)
		}
		if e.Prefix != "" {
			if !strings.HasPrefix(e.Prefix, "/") {
				p.addf(path+".prefix", "must start with /, got %q", e.Prefix)
			}
			if strings.HasSuffix(e.Prefix, "/") {
				p.addf(path+".prefix", "must not end with /, got %q", e.Prefix)
			}
		}
		key := e.Backend + "\x00" + e.Prefix
		if prev, dup := seenPrefix[key]; dup {
			p.addf(path, "backend %q is already exposed at prefix %q by %s", e.Backend, e.Prefix, prev)
		} else {
			seenPrefix[key] = path
		}
		for _, m := range e.Middleware {
			if _, ok := c.Middleware[m]; !ok {
				p.addf(path+".middleware", "no middleware named %q is defined", m)
			}
		}
		for opID, ov := range e.Overrides {
			opath := fmt.Sprintf("%s.overrides.%s", path, opID)
			if ov.Path != "" && !strings.HasPrefix(ov.Path, "/") {
				p.addf(opath+".path", "must start with /, got %q", ov.Path)
			}
			for _, m := range ov.Middleware {
				if _, ok := c.Middleware[m]; !ok {
					p.addf(opath+".middleware", "no middleware named %q is defined", m)
				}
			}
		}
	}

	if len(p.errs) > 0 {
		return p.errs
	}
	return nil
}

// validateSchemeNames checks the published names a backend chooses for its
// security schemes. Sorted iteration keeps the reported problems in a stable
// order across runs.
func validateSchemeNames(p *problems, path string, names map[string]string) {
	if len(names) == 0 {
		return
	}
	base := path + ".security_scheme_names"
	from := make([]string, 0, len(names))
	for k := range names {
		from = append(from, k)
	}
	sort.Strings(from)

	claimed := map[string]string{}
	for _, k := range from {
		to := names[k]
		switch {
		case k == "":
			p.addf(base, "an entry has an empty key; the key is the scheme name the backend's own document declares")
			continue
		case to == "":
			p.addf(base+"."+k, "must not be empty")
			continue
		case !componentNameRe.MatchString(to):
			p.addf(base+"."+k, "%q is not a usable component name; use only letters, digits, and . - _", to)
			continue
		}
		if prev, dup := claimed[to]; dup {
			p.addf(base+"."+k, "%q is already the published name for %q; two schemes cannot share one name", to, prev)
			continue
		}
		claimed[to] = k
	}
}

func validateBreaker(p *problems, path string, cb *CircuitBreaker) {
	if cb.FailureRatio <= 0 || cb.FailureRatio > 1 {
		p.addf(path+".failure_ratio", "must be between 0 (exclusive) and 1, got %v", cb.FailureRatio)
	}
	if cb.MinRequests < 1 {
		p.addf(path+".min_requests", "must be at least 1, got %d", cb.MinRequests)
	}
	if cb.OpenFor <= 0 {
		p.addf(path+".open_for", "must be positive")
	}
}

func validateRetry(p *problems, path string, r *Retry) {
	if r.Attempts < 0 {
		p.addf(path+".attempts", "must not be negative")
	}
	if r.Attempts > 10 {
		p.addf(path+".attempts", "%d is unreasonably high; retries amplify load on a struggling upstream", r.Attempts)
	}
	if r.Backoff < 0 {
		p.addf(path+".backoff", "must not be negative")
	}
}

// envRe matches ${NAME} and ${NAME:-default}.
var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv substitutes environment variables. A variable that is neither set nor
// given a default is an error rather than an empty string, because silently
// blanking an upstream host or a JWKS URL fails in confusing ways much later.
func expandEnv(data []byte) ([]byte, error) {
	var missing []string
	out := envRe.ReplaceAllFunc(data, func(m []byte) []byte {
		g := envRe.FindSubmatch(m)
		name := string(g[1])
		if v, ok := os.LookupEnv(name); ok {
			return []byte(v)
		}
		if len(g[2]) > 0 {
			return g[3]
		}
		missing = append(missing, name)
		return nil
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined environment variable(s): %s (use ${NAME:-default} to make one optional)",
			strings.Join(dedupeStrings(missing), ", "))
	}
	return out, nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
