package config

import (
	"strings"
	"testing"
)

func withServer(block string) string {
	return strings.Replace(minimal, "version: 1", "version: 1\nserver:\n  listen: \":8080\"\n"+block, 1)
}

func TestSubdomainParsing(t *testing.T) {
	c, err := Parse("test.yaml", []byte(withServer(`  subdomain:
    hosts: ["{subdomain}.example.com"]
    exclude: [www]
`)))
	if err != nil {
		t.Fatal(err)
	}
	tn := c.Server.Subdomain
	if tn == nil || len(tn.Hosts) != 1 || tn.Exclude[0] != "www" || tn.TrustForwardedHost {
		t.Fatalf("subdomain = %+v", tn)
	}
}

func TestSubdomainOffByDefault(t *testing.T) {
	c, err := Parse("test.yaml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Subdomain != nil {
		t.Error("subdomain extraction is on without being configured")
	}
}

func TestSubdomainValidation(t *testing.T) {
	cases := map[string]string{
		"no hosts": `  subdomain: { hosts: [] }
`,
		"no placeholder": `  subdomain: { hosts: ["example.com"] }
`,
		"partial label": `  subdomain: { hosts: ["t-{subdomain}.example.com"] }
`,
		"bad exclude": `  subdomain: { hosts: ["{subdomain}.example.com"], exclude: ["w w"] }
`,
		"forwarded without proxies": `  subdomain: { hosts: ["{subdomain}.example.com"], trust_forwarded_host: true }
`,
	}
	for name, block := range cases {
		if _, err := Parse("test.yaml", []byte(withServer(block))); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "server.subdomain") {
			t.Errorf("%s: error does not name the field: %v", name, err)
		}
	}

	ok := withServer(`  trusted_proxy_cidrs: ["10.0.0.0/8"]
  subdomain: { hosts: ["{subdomain}.example.com"], trust_forwarded_host: true }
`)
	if _, err := Parse("test.yaml", []byte(ok)); err != nil {
		t.Errorf("forwarded host with proxies rejected: %v", err)
	}
}
