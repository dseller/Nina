package config

import (
	"strings"
	"testing"
)

func withServer(block string) string {
	return strings.Replace(minimal, "version: 1", "version: 1\nserver:\n  listen: \":8080\"\n"+block, 1)
}

func TestTenantParsing(t *testing.T) {
	c, err := Parse("test.yaml", []byte(withServer(`  tenant:
    hosts: ["{tenant}.example.com"]
    exclude: [www]
`)))
	if err != nil {
		t.Fatal(err)
	}
	tn := c.Server.Tenant
	if tn == nil || len(tn.Hosts) != 1 || tn.Exclude[0] != "www" || tn.TrustForwardedHost {
		t.Fatalf("tenant = %+v", tn)
	}
}

func TestTenantOffByDefault(t *testing.T) {
	c, err := Parse("test.yaml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Tenant != nil {
		t.Error("tenant extraction is on without being configured")
	}
}

func TestTenantValidation(t *testing.T) {
	cases := map[string]string{
		"no hosts": `  tenant: { hosts: [] }
`,
		"no placeholder": `  tenant: { hosts: ["example.com"] }
`,
		"partial label": `  tenant: { hosts: ["t-{tenant}.example.com"] }
`,
		"bad exclude": `  tenant: { hosts: ["{tenant}.example.com"], exclude: ["w w"] }
`,
		"forwarded without proxies": `  tenant: { hosts: ["{tenant}.example.com"], trust_forwarded_host: true }
`,
	}
	for name, block := range cases {
		if _, err := Parse("test.yaml", []byte(withServer(block))); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "server.tenant") {
			t.Errorf("%s: error does not name the field: %v", name, err)
		}
	}

	ok := withServer(`  trusted_proxy_cidrs: ["10.0.0.0/8"]
  tenant: { hosts: ["{tenant}.example.com"], trust_forwarded_host: true }
`)
	if _, err := Parse("test.yaml", []byte(ok)); err != nil {
		t.Errorf("forwarded host with proxies rejected: %v", err)
	}
}
