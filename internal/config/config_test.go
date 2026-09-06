package config

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
version: 1
backends:
  - name: users
    spec: { file: ./users.yaml }
    hosts: ["https://users.internal"]
expose:
  - backend: users
    prefix: /users
`

func TestParseMinimal(t *testing.T) {
	c, err := Parse("test.yaml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8080" {
		t.Errorf("default listen = %q", c.Server.Listen)
	}
	if c.Defaults.Timeout.Std() != 10*time.Second {
		t.Errorf("default timeout = %v", c.Defaults.Timeout.Std())
	}
	if c.Backends[0].Namespace != "Users" {
		t.Errorf("derived namespace = %q, want Users", c.Backends[0].Namespace)
	}
	if c.Backends[0].LoadBalance != "round_robin" {
		t.Errorf("default load_balance = %q", c.Backends[0].LoadBalance)
	}
	if c.Backends[0].Spec.OnError != "fail" {
		t.Errorf("default on_error = %q, want fail", c.Backends[0].Spec.OnError)
	}
	if c.Spec.Dedupe == nil || !*c.Spec.Dedupe {
		t.Error("dedupe should default to true")
	}
}

// JSON must work with no extra code path, since YAML is decoded through it.
func TestParseJSON(t *testing.T) {
	const j = `{
	  "version": 1,
	  "backends": [{"name":"users","spec":{"file":"./u.yaml"},"hosts":["https://u.internal"]}],
	  "expose": [{"backend":"users","prefix":"/users"}]
	}`
	c, err := Parse("test.json", []byte(j))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Backends) != 1 || c.Backends[0].Name != "users" {
		t.Errorf("backends = %+v", c.Backends)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	bad := strings.Replace(minimal, "version: 1", "version: 1\nlisten: \":9000\"", 1)
	_, err := Parse("test.yaml", []byte(bad))
	if err == nil {
		t.Fatal("expected an error for an unknown top-level field")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

func TestDurationParsing(t *testing.T) {
	src := strings.Replace(minimal, "version: 1", "version: 1\ndefaults:\n  timeout: 250ms", 1)
	c, err := Parse("test.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.Timeout.Std() != 250*time.Millisecond {
		t.Errorf("timeout = %v, want 250ms", c.Defaults.Timeout.Std())
	}

	bad := strings.Replace(minimal, "version: 1", "version: 1\ndefaults:\n  timeout: soon", 1)
	if _, err := Parse("test.yaml", []byte(bad)); err == nil {
		t.Error("expected an error for an unparseable duration")
	}
}

func TestValidationCollectsEveryProblem(t *testing.T) {
	const bad = `
version: 1
backends:
  - name: "1bad"
    spec: {}
    hosts: []
    load_balance: sideways
expose:
  - backend: nosuch
    prefix: trailing/
`
	_, err := Parse("test.yaml", []byte(bad))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	msg := err.Error()
	for _, want := range []string{
		"name",             // invalid backend name
		"needs either",     // missing spec source
		"hosts",            // no hosts
		"load_balance",     // bad strategy
		"no backend named", // dangling expose
		"must start with /",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q; got:\n%s", want, msg)
		}
	}
}

func TestDuplicateBackendName(t *testing.T) {
	const dup = `
version: 1
backends:
  - name: users
    spec: { file: ./a.yaml }
    hosts: ["https://a"]
  - name: users
    spec: { file: ./b.yaml }
    hosts: ["https://b"]
expose:
  - backend: users
`
	_, err := Parse("test.yaml", []byte(dup))
	if err == nil || !strings.Contains(err.Error(), "duplicate backend name") {
		t.Errorf("expected a duplicate name error, got %v", err)
	}
}

func TestMiddlewareReferenceMustExist(t *testing.T) {
	src := strings.Replace(minimal, "    prefix: /users", "    prefix: /users\n    middleware: [ghost]", 1)
	_, err := Parse("test.yaml", []byte(src))
	if err == nil || !strings.Contains(err.Error(), `no middleware named "ghost"`) {
		t.Errorf("expected a dangling middleware error, got %v", err)
	}
}

func TestMiddlewareRawIsPreserved(t *testing.T) {
	src := minimal + `
middleware:
  limiter:
    type: ratelimit
    limit: 42
    window: 30s
`
	c, err := Parse("test.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	def := c.Middleware["limiter"]
	if def.Type != "ratelimit" {
		t.Errorf("type = %q", def.Type)
	}
	// The raw block is what the middleware constructor decodes; it must survive
	// the round trip through YAML->JSON intact.
	if !strings.Contains(string(def.Raw), `"limit":42`) {
		t.Errorf("raw config lost its fields: %s", def.Raw)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("NINA_TEST_HOST", "https://from-env.internal")
	src := strings.Replace(minimal, `"https://users.internal"`, `"${NINA_TEST_HOST}"`, 1)
	c, err := Parse("test.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backends[0].Hosts[0] != "https://from-env.internal" {
		t.Errorf("host = %q", c.Backends[0].Hosts[0])
	}
}

func TestEnvDefault(t *testing.T) {
	src := strings.Replace(minimal, `"https://users.internal"`,
		`"${NINA_UNSET_HOST:-https://fallback.internal}"`, 1)
	c, err := Parse("test.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backends[0].Hosts[0] != "https://fallback.internal" {
		t.Errorf("host = %q", c.Backends[0].Hosts[0])
	}
}

// An unset variable with no default must fail loudly. Substituting an empty
// string would silently blank an upstream host or a JWKS URL.
func TestUndefinedEnvIsAnError(t *testing.T) {
	src := strings.Replace(minimal, `"https://users.internal"`, `"${NINA_DEFINITELY_UNSET}"`, 1)
	_, err := Parse("test.yaml", []byte(src))
	if err == nil || !strings.Contains(err.Error(), "NINA_DEFINITELY_UNSET") {
		t.Errorf("expected an error naming the variable, got %v", err)
	}
}

func TestNamespaceDerivation(t *testing.T) {
	tests := map[string]string{
		"users":         "Users",
		"user-profiles": "UserProfiles",
		"order_items":   "OrderItems",
		"a":             "A",
	}
	for in, want := range tests {
		if got := namespaceFor(in); got != want {
			t.Errorf("namespaceFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRetryValidation(t *testing.T) {
	src := strings.Replace(minimal, "version: 1",
		"version: 1\ndefaults:\n  retry:\n    attempts: 50", 1)
	_, err := Parse("test.yaml", []byte(src))
	if err == nil || !strings.Contains(err.Error(), "unreasonably high") {
		t.Errorf("expected a guard against absurd retry counts, got %v", err)
	}
}

func TestTLSBlockParsing(t *testing.T) {
	src := strings.Replace(minimal,
		`    hosts: ["https://users.internal"]`,
		"    hosts: [\"https://users.internal\"]\n    tls: { insecure_skip_verify: true }", 1)
	c, err := Parse("test.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Backends[0].InsecureTLS() {
		t.Error("insecure_skip_verify did not survive parsing")
	}
	if !c.Backends[0].UsesTLS() {
		t.Error("UsesTLS should be true for an https host")
	}
}

// Verification must be on unless it is explicitly turned off.
func TestTLSVerificationOnByDefault(t *testing.T) {
	c, err := Parse("test.yaml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backends[0].InsecureTLS() {
		t.Fatal("certificate verification was disabled without being asked for")
	}
	// An explicit false is also honoured.
	src := strings.Replace(minimal,
		`    hosts: ["https://users.internal"]`,
		"    hosts: [\"https://users.internal\"]\n    tls: { insecure_skip_verify: false }", 1)
	c2, err := Parse("test.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if c2.Backends[0].InsecureTLS() {
		t.Error("an explicit false was read as true")
	}
}

func TestUnknownTLSFieldIsRejected(t *testing.T) {
	src := strings.Replace(minimal,
		`    hosts: ["https://users.internal"]`,
		"    hosts: [\"https://users.internal\"]\n    tls: { insecure_skip_verfy: true }", 1)
	_, err := Parse("test.yaml", []byte(src))
	if err == nil {
		t.Fatal("a misspelled TLS field should be rejected, not silently ignored")
	}
}

func TestUsesTLS(t *testing.T) {
	tests := []struct {
		name  string
		hosts []string
		spec  string
		want  bool
	}{
		{"plain http", []string{"http://a.internal"}, "", false},
		{"https host", []string{"https://a.internal"}, "", true},
		{"https spec only", []string{"http://a.internal"}, "https://a.internal/o.json", true},
		{"mixed hosts", []string{"http://a.internal", "https://b.internal"}, "", true},
	}
	for _, tc := range tests {
		b := Backend{Hosts: tc.hosts, Spec: SpecSource{URL: tc.spec}}
		if got := b.UsesTLS(); got != tc.want {
			t.Errorf("%s: UsesTLS() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSecuritySchemeNamesParsing(t *testing.T) {
	src := strings.Replace(minimal,
		`    hosts: ["https://users.internal"]`,
		"    hosts: [\"https://users.internal\"]\n    security_scheme_names:\n      jwtAuth: Bearer", 1)
	c, err := Parse("test.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Backends[0].SecuritySchemeNames["jwtAuth"]; got != "Bearer" {
		t.Errorf("security_scheme_names[jwtAuth] = %q, want Bearer", got)
	}
}

func TestSecuritySchemeNameValidation(t *testing.T) {
	tests := map[string]struct{ entries, want string }{
		"unusable character": {"jwtAuth: My Token", "not a usable component name"},
		"empty name":         {`jwtAuth: ""`, "must not be empty"},
		"two schemes, one name": {
			"jwtAuth: Auth\n      cookieAuth: Auth", "cannot share one name",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(minimal,
				`    hosts: ["https://users.internal"]`,
				"    hosts: [\"https://users.internal\"]\n    security_scheme_names:\n      "+tc.entries, 1)
			_, err := Parse("test.yaml", []byte(src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}
