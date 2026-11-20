package tenant

import "testing"

func TestMatch(t *testing.T) {
	m, err := Compile(
		[]string{"{tenant}.example.com", "api.{tenant}.example.org"},
		[]string{"www", "API"},
	)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ host, want string }{
		{"acme.example.com", "acme"},
		{"ACME.Example.COM", "acme"},
		{"acme.example.com:8443", "acme"},
		{"acme.example.com.", "acme"},
		{"api.globex.example.org", "globex"},
		{"www.example.com", ""},          // excluded
		{"api.example.com", ""},          // excluded, case-insensitively
		{"example.com", ""},              // no label at all
		{".example.com", ""},             // empty label
		{"a.b.example.com", ""},          // two labels for one placeholder
		{"acme.example.com.evil.io", ""}, // suffix must end the host
		{"acmeexample.com", ""},          // the dot belongs to the suffix
		{"globex.example.org", ""},       // second pattern needs its api. prefix
		{"-x.example.com", ""},           // not a valid label
		{"x_y.example.com", ""},          // not a valid label
		{"127.0.0.1:80", ""},
		{"[::1]:80", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := m.Match(c.host); got != c.want {
			t.Errorf("Match(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

func TestNilMatcher(t *testing.T) {
	var m *Matcher
	if got := m.Match("acme.example.com"); got != "" {
		t.Errorf("nil matcher returned %q", got)
	}
}

func TestCheckPattern(t *testing.T) {
	good := []string{"{tenant}.example.com", "api.{tenant}.example.com", "{tenant}.localhost", "x.{tenant}"}
	for _, p := range good {
		if err := CheckPattern(p); err != nil {
			t.Errorf("CheckPattern(%q) = %v", p, err)
		}
	}
	bad := []string{
		"example.com",              // no placeholder
		"{tenant}.{tenant}.com",    // two
		"{tenant}",                 // matches everything
		"t-{tenant}.example.com",   // partial label
		"{tenant}.exa_mple.com",    // invalid fixed label
		"{tenant}..com",            // empty label
		"{tenant}.example.com:443", // port
	}
	for _, p := range bad {
		if err := CheckPattern(p); err == nil {
			t.Errorf("CheckPattern(%q) accepted a bad pattern", p)
		}
	}
}
