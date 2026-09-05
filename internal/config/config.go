// Package config loads and validates the gateway configuration.
//
// YAML is converted to JSON and decoded from there, so a .json config works with
// no extra code path and both formats share one set of validation rules.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the whole gateway configuration.
type Config struct {
	Version    int                      `json:"version"`
	Server     Server                   `json:"server"`
	Spec       SpecPublish              `json:"spec"`
	Defaults   Defaults                 `json:"defaults"`
	Backends   []Backend                `json:"backends"`
	Expose     []Expose                 `json:"expose"`
	Middleware map[string]MiddlewareDef `json:"middleware"`
	Stores     Stores                   `json:"stores"`

	// Path and Dir are set by Load and are not part of the document.
	Path string `json:"-"`
	Dir  string `json:"-"`
}

type Server struct {
	Listen            string   `json:"listen"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	MaxRequestBody    int64    `json:"max_request_body"`
	Admin             Admin    `json:"admin"`
	// TrustedProxyCIDRs lists networks whose X-Forwarded-For we believe when
	// deriving the client IP. Empty means trust nothing and use the peer address.
	TrustedProxyCIDRs []string `json:"trusted_proxy_cidrs"`
}

type Admin struct {
	Listen string `json:"listen"`
	Docs   bool   `json:"docs"`
}

// SpecPublish controls the merged document the gateway emits.
type SpecPublish struct {
	Title       string   `json:"title"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Servers     []string `json:"servers"`
	// Dedupe collapses structurally identical components sharing a base name.
	// Defaults to true.
	Dedupe *bool `json:"dedupe"`
}

type Defaults struct {
	Timeout        Duration        `json:"timeout"`
	Retry          *Retry          `json:"retry"`
	CircuitBreaker *CircuitBreaker `json:"circuit_breaker"`
}

type Retry struct {
	Attempts       int      `json:"attempts"`
	Backoff        Duration `json:"backoff"`
	Jitter         bool     `json:"jitter"`
	IdempotentOnly *bool    `json:"idempotent_only"`
}

type CircuitBreaker struct {
	FailureRatio float64  `json:"failure_ratio"`
	MinRequests  int      `json:"min_requests"`
	OpenFor      Duration `json:"open_for"`
}

type Backend struct {
	Name           string          `json:"name"`
	Spec           SpecSource      `json:"spec"`
	Hosts          []string        `json:"hosts"`
	LoadBalance    string          `json:"load_balance"`
	HealthCheck    *HealthCheck    `json:"health_check"`
	Timeout        Duration        `json:"timeout"`
	Retry          *Retry          `json:"retry"`
	CircuitBreaker *CircuitBreaker `json:"circuit_breaker"`
	// Namespace overrides the component-name prefix in the published document.
	// Defaults to a capitalised form of Name.
	Namespace string `json:"namespace"`
	// StripPrefix is removed from the front of each upstream path before the
	// gateway prefix is applied.
	StripPrefix string `json:"strip_prefix"`
	// ForwardHeaders are added to every upstream request from this backend.
	ForwardHeaders map[string]string `json:"forward_headers"`
}

// SpecSource points at one upstream OpenAPI document.
type SpecSource struct {
	File string `json:"file"`
	URL  string `json:"url"`
	// Refresh re-fetches the document on an interval. Zero disables polling.
	Refresh Duration `json:"refresh"`
	// OnError is "fail" (refuse to build a runtime) or "stale" (keep the cached
	// copy and mark the backend degraded). Defaults to "fail".
	OnError string `json:"on_error"`
}

type HealthCheck struct {
	Path           string   `json:"path"`
	Interval       Duration `json:"interval"`
	Timeout        Duration `json:"timeout"`
	UnhealthyAfter int      `json:"unhealthy_after"`
	HealthyAfter   int      `json:"healthy_after"`
}

type Expose struct {
	Backend    string              `json:"backend"`
	Prefix     string              `json:"prefix"`
	Include    []string            `json:"include"`
	Exclude    []string            `json:"exclude"`
	Middleware []string            `json:"middleware"`
	Overrides  map[string]Override `json:"overrides"`
}

type Override struct {
	Path       string   `json:"path"`
	Middleware []string `json:"middleware"`
	Timeout    Duration `json:"timeout"`
	Disabled   bool     `json:"disabled"`
}

type Stores struct {
	Redis *RedisStore `json:"redis"`
}

type RedisStore struct {
	Addr     string   `json:"addr"`
	Password string   `json:"password"`
	DB       int      `json:"db"`
	Timeout  Duration `json:"timeout"`
}

// MiddlewareDef is a named middleware instance. Type selects the implementation;
// the remaining fields stay raw and are decoded by that implementation, so adding
// a middleware never means touching this package.
type MiddlewareDef struct {
	Type string
	Raw  json.RawMessage
}

func (m *MiddlewareDef) UnmarshalJSON(b []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	m.Type = probe.Type
	m.Raw = append(json.RawMessage(nil), b...)
	return nil
}

func (m MiddlewareDef) MarshalJSON() ([]byte, error) {
	if m.Raw == nil {
		return []byte("null"), nil
	}
	return m.Raw, nil
}

// Duration accepts either a duration string ("3s") or a number of nanoseconds.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Or returns d, or def when d is zero. Used to layer route over backend over
// global defaults.
func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*d = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"3s\" or a number of nanoseconds")
	}
	*d = Duration(n)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Parse decodes a configuration document. name is used in error messages.
//
// Unknown fields are rejected: a silently ignored typo in a gateway config is a
// production incident waiting to happen.
func Parse(name string, data []byte) (*Config, error) {
	expanded, err := expandEnv(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	jsonData, err := yaml.YAMLToJSON(expanded)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	dec := json.NewDecoder(bytes.NewReader(jsonData))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %s", name, humaniseDecodeError(err))
	}

	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &c, nil
}

// humaniseDecodeError turns encoding/json's terser messages into something an
// operator can act on without reading our struct tags.
func humaniseDecodeError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "unknown field") {
		return msg + " (check for a typo; unknown keys are rejected on purpose)"
	}
	var ute *json.UnmarshalTypeError
	if errorsAs(err, &ute) && ute.Field != "" {
		return fmt.Sprintf("field %q: expected %s, got %s", ute.Field, ute.Type, ute.Value)
	}
	return msg
}

func errorsAs(err error, target **json.UnmarshalTypeError) bool {
	for err != nil {
		if e, ok := err.(*json.UnmarshalTypeError); ok {
			*target = e
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
