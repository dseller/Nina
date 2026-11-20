// Package tenant derives a tenant name from the request's host.
//
// A pattern is a hostname with one label replaced by {tenant}, such as
// "{tenant}.example.com" or "api.{tenant}.example.com". The placeholder matches
// exactly one DNS label, so "a.b.example.com" is not tenant "a.b" of
// "{tenant}.example.com".
package tenant

import (
	"fmt"
	"net"
	"strings"
)

const placeholder = "{tenant}"

// Matcher is immutable after Compile and safe for concurrent use.
type Matcher struct {
	patterns []pattern
	exclude  map[string]bool
}

type pattern struct {
	prefix, suffix string // lower case, each including its joining dot
}

// Compile checks and compiles host patterns. exclude lists labels that match a
// pattern but are not tenants, such as "www".
func Compile(patterns, exclude []string) (*Matcher, error) {
	m := &Matcher{exclude: map[string]bool{}}
	for _, raw := range patterns {
		p, err := parsePattern(raw)
		if err != nil {
			return nil, err
		}
		m.patterns = append(m.patterns, p)
	}
	for _, e := range exclude {
		if err := CheckLabel(e); err != nil {
			return nil, fmt.Errorf("exclude %q: %w", e, err)
		}
		m.exclude[strings.ToLower(e)] = true
	}
	return m, nil
}

// CheckPattern reports whether raw is a usable host pattern.
func CheckPattern(raw string) error {
	_, err := parsePattern(raw)
	return err
}

func parsePattern(raw string) (pattern, error) {
	s := strings.ToLower(strings.TrimSuffix(raw, "."))
	if strings.Count(s, placeholder) != 1 {
		return pattern{}, fmt.Errorf("%q must contain %s exactly once", raw, placeholder)
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return pattern{}, fmt.Errorf("%q needs at least one fixed label besides %s, or every host would match", raw, placeholder)
	}
	at := -1
	for i, l := range labels {
		if l == placeholder {
			at = i
			continue
		}
		if strings.Contains(l, placeholder) {
			return pattern{}, fmt.Errorf("%q: %s must be a whole label, not part of %q", raw, placeholder, l)
		}
		if err := CheckLabel(l); err != nil {
			return pattern{}, fmt.Errorf("%q: %w", raw, err)
		}
	}
	var p pattern
	if at > 0 {
		p.prefix = strings.Join(labels[:at], ".") + "."
	}
	if at < len(labels)-1 {
		p.suffix = "." + strings.Join(labels[at+1:], ".")
	}
	return p, nil
}

// CheckLabel reports whether l is a valid DNS label.
func CheckLabel(l string) error {
	if l == "" || len(l) > 63 {
		return fmt.Errorf("label %q must be 1 to 63 characters", l)
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return fmt.Errorf("label %q must not start or end with a hyphen", l)
	}
	for i := 0; i < len(l); i++ {
		if !labelByte(l[i]) {
			return fmt.Errorf("label %q may only hold letters, digits and hyphens", l)
		}
	}
	return nil
}

func labelByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// Match returns the lower-case tenant for host, or "" when no pattern matches
// or the label is excluded. host may carry a port. The first matching pattern
// wins.
//
// The host comes from the client, so the result is only ever a well-formed DNS
// label: a hostile Host header cannot put arbitrary text into a log line.
func (m *Matcher) Match(host string) string {
	if m == nil || host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range m.patterns {
		if len(host) <= len(p.prefix)+len(p.suffix) ||
			!strings.HasPrefix(host, p.prefix) || !strings.HasSuffix(host, p.suffix) {
			continue
		}
		label := host[len(p.prefix) : len(host)-len(p.suffix)]
		if CheckLabel(label) != nil || m.exclude[label] {
			continue
		}
		return label
	}
	return ""
}
