package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestBackend(t *testing.T, o Options) *Backend {
	t.Helper()
	b, err := NewBackend(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func get(t *testing.T, b *Backend, path string) (*http.Response, *Result, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil)
	req.RemoteAddr = "10.0.0.1:1234"
	return b.Do(req, path)
}

func TestForwardsAndStripsHopByHop(t *testing.T) {
	var gotConnection, gotCustom, gotXFF string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotConnection = r.Header.Get("Connection")
		gotCustom = r.Header.Get("X-Secret-Hop")
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.Header().Set("Connection", "close")
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()

	b := newTestBackend(t, Options{Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second})

	req := httptest.NewRequest(http.MethodGet, "http://gateway/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	// A header named by Connection must be dropped along with Connection itself.
	req.Header.Set("Connection", "X-Secret-Hop")
	req.Header.Set("X-Secret-Hop", "leaked")

	resp, _, err := b.Do(req, "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotConnection != "" {
		t.Errorf("Connection header reached upstream: %q", gotConnection)
	}
	if gotCustom != "" {
		t.Errorf("header named by Connection reached upstream: %q", gotCustom)
	}
	if gotXFF != "10.0.0.1" {
		t.Errorf("X-Forwarded-For = %q, want 10.0.0.1", gotXFF)
	}

	rec := httptest.NewRecorder()
	if _, err := CopyResponse(rec, resp); err != nil {
		t.Fatal(err)
	}
	if rec.Header().Get("Connection") != "" {
		t.Error("Connection header was copied back to the client")
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Error("upstream response header was lost")
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestRetryOnServerError(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "recovered")
	}))
	defer srv.Close()

	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second,
		Retry: RetryPolicy{Attempts: 3, Backoff: time.Millisecond, IdempotentOnly: true},
	})

	resp, res, err := get(t, b, "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d after retries", resp.StatusCode)
	}
	if res.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", res.Attempts)
	}
}

// A POST must not be replayed by default: retrying a create can produce two
// orders.
func TestNoRetryForNonIdempotentMethod(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second,
		Retry: RetryPolicy{Attempts: 3, Backoff: time.Millisecond, IdempotentOnly: true},
	})

	req := httptest.NewRequest(http.MethodPost, "http://gateway/x", strings.NewReader(`{}`))
	req.RemoteAddr = "10.0.0.1:1"
	resp, _, err := b.Do(req, "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 1 {
		t.Errorf("POST was attempted %d times, want 1", calls.Load())
	}
}

// A 4xx is the upstream's considered answer and must be returned as-is.
func TestNoRetryOnClientError(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second,
		Retry: RetryPolicy{Attempts: 3, Backoff: time.Millisecond, IdempotentOnly: true},
	})
	resp, _, err := get(t, b, "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 || calls.Load() != 1 {
		t.Errorf("status %d after %d calls, want 404 after 1", resp.StatusCode, calls.Load())
	}
}

func TestRoundRobinAcrossHosts(t *testing.T) {
	var a, bcount atomic.Int64
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.Add(1) }))
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bcount.Add(1) }))
	defer s2.Close()

	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{s1.URL, s2.URL}, LoadBalance: "round_robin", Timeout: 5 * time.Second,
	})
	for i := 0; i < 10; i++ {
		resp, _, err := get(t, b, "/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if a.Load() != 5 || bcount.Load() != 5 {
		t.Errorf("distribution = %d/%d, want 5/5", a.Load(), bcount.Load())
	}
}

func TestUnhealthyHostIsSkipped(t *testing.T) {
	var good atomic.Int64
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { good.Add(1) }))
	defer s2.Close()

	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{s1.URL, s2.URL}, Timeout: 5 * time.Second,
	})
	b.Pool().Hosts()[0].healthy.Store(false)

	for i := 0; i < 6; i++ {
		resp, _, err := get(t, b, "/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if good.Load() != 6 {
		t.Errorf("healthy host served %d of 6", good.Load())
	}
}

func TestNoHealthyHostIsReported(t *testing.T) {
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer s1.Close()
	b := newTestBackend(t, Options{Name: "t", Hosts: []string{s1.URL}, Timeout: time.Second})
	b.Pool().Hosts()[0].healthy.Store(false)

	_, _, err := get(t, b, "/x")
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindNoHost {
		t.Fatalf("error = %v, want a no_healthy_host Error", err)
	}
	if pe.StatusCode() != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", pe.StatusCode())
	}
}

func TestTimeoutIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	b := newTestBackend(t, Options{Name: "t", Hosts: []string{srv.URL}, Timeout: 40 * time.Millisecond})
	_, _, err := get(t, b, "/slow")
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindTimeout {
		t.Fatalf("error = %v, want a timeout Error", err)
	}
	if pe.StatusCode() != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", pe.StatusCode())
	}
}

// The response body must remain readable after Do returns: the timeout context
// is released on Close, not when Do exits.
func TestBodyReadableAfterDoReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, "late payload")
	}))
	defer srv.Close()

	b := newTestBackend(t, Options{Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second})
	resp, _, err := get(t, b, "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body after Do returned: %v", err)
	}
	if string(body) != "late payload" {
		t.Errorf("body = %q", body)
	}
}

func TestSingleSlashJoin(t *testing.T) {
	tests := []struct{ base, p, want string }{
		{"", "/users", "/users"},
		{"/", "/users", "/users"},
		{"/api", "/users", "/api/users"},
		{"/api/", "/users", "/api/users"},
		{"/api", "users", "/api/users"},
		{"", "", "/"},
	}
	for _, tc := range tests {
		if got := singleSlashJoin(tc.base, tc.p); got != tc.want {
			t.Errorf("singleSlashJoin(%q, %q) = %q, want %q", tc.base, tc.p, got, tc.want)
		}
	}
}
