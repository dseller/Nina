package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// httptest.NewTLSServer presents a certificate signed by an untrusted test CA,
// which is exactly the shape of the self-signed internal service this setting
// exists for.
func newTLSUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secure hello")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The default has to be verification ON. If this test ever passes without the
// flag, the gateway is silently trusting any certificate.
func TestUntrustedCertificateIsRejectedByDefault(t *testing.T) {
	srv := newTLSUpstream(t)
	b := newTestBackend(t, Options{Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second})

	_, _, err := get(t, b, "/x")
	if err == nil {
		t.Fatal("an untrusted upstream certificate was accepted by default")
	}
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindConnect {
		t.Fatalf("error = %v, want a connect Error", err)
	}
	if pe.StatusCode() != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", pe.StatusCode())
	}
}

func TestInsecureSkipVerifyAcceptsUntrustedCertificate(t *testing.T) {
	srv := newTLSUpstream(t)
	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second,
		InsecureSkipVerify: true,
	})

	resp, _, err := get(t, b, "/x")
	if err != nil {
		t.Fatalf("insecure_skip_verify did not take effect: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "secure hello" {
		t.Errorf("body = %q", body)
	}
	if !b.InsecureTLS() {
		t.Error("InsecureTLS() should report the setting for auditing")
	}
}

// The setting is per backend: one lax backend must not weaken another.
func TestInsecureSkipVerifyIsPerBackend(t *testing.T) {
	srv := newTLSUpstream(t)

	lax := newTestBackend(t, Options{
		Name: "lax", Hosts: []string{srv.URL}, Timeout: 5 * time.Second,
		InsecureSkipVerify: true,
	})
	strict := newTestBackend(t, Options{
		Name: "strict", Hosts: []string{srv.URL}, Timeout: 5 * time.Second,
	})

	resp, _, err := get(t, lax, "/x")
	if err != nil {
		t.Fatalf("lax backend failed: %v", err)
	}
	resp.Body.Close()

	if _, _, err := get(t, strict, "/x"); err == nil {
		t.Error("the strict backend accepted an untrusted certificate")
	}
	if lax.InsecureTLS() == strict.InsecureTLS() {
		t.Error("the two backends should not report the same TLS posture")
	}
}

// Enabling it must not silently downgrade the connection to HTTP/1.1: setting
// TLSClientConfig suppresses the transport's automatic HTTP/2 upgrade unless
// ForceAttemptHTTP2 is set, which is easy to get wrong.
func TestInsecureSkipVerifyKeepsHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second,
		InsecureSkipVerify: true,
	})
	resp, _, err := get(t, b, "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "HTTP/2.0" {
		t.Errorf("upstream saw %s, want HTTP/2.0; the TLS config suppressed the HTTP/2 upgrade", body)
	}
}

// Guard the wiring itself: the transport must actually carry the flag.
func TestTransportCarriesTheFlag(t *testing.T) {
	b, err := NewBackend(Options{Name: "t", Hosts: []string{"https://x.internal"}, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := b.transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", b.transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify did not reach the transport's TLS config")
	}

	secure, err := NewBackend(Options{Name: "t", Hosts: []string{"https://x.internal"}})
	if err != nil {
		t.Fatal(err)
	}
	str := secure.transport.(*http.Transport)
	if str.TLSClientConfig != nil && str.TLSClientConfig.InsecureSkipVerify {
		t.Error("verification was disabled without being asked for")
	}
}
