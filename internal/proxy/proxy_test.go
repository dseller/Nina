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

// countingReader records how much of a request body has been consumed.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// transportFunc lets a test stand in for the upstream at the RoundTrip level.
type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse(r *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}, Request: r}
}

// A request that can never be retried must reach the upstream as a stream, not
// after being read into memory. Large uploads used to be buffered whole when
// retries were configured, which ran the gateway out of memory.
func TestNonRetryableBodyIsStreamed(t *testing.T) {
	const size = 4 << 20
	src := &countingReader{r: strings.NewReader(strings.Repeat("x", size))}

	var readBeforeSend int64 = -1
	var received int64
	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{"http://upstream"}, Timeout: 5 * time.Second,
		Retry:        RetryPolicy{Attempts: 2, Backoff: time.Millisecond, IdempotentOnly: true},
		MaxRetryBody: 8 << 20, // larger than the body: only retryability stops buffering
		Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			readBeforeSend = src.n.Load()
			n, err := io.Copy(io.Discard, r.Body)
			received = n
			return okResponse(r), err
		}),
	})

	req := httptest.NewRequest(http.MethodPost, "http://gateway/upload", src)
	req.RemoteAddr = "10.0.0.1:1"
	resp, _, err := b.Do(req, "/upload")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if readBeforeSend != 0 {
		t.Errorf("%d bytes of a non-retryable body were read before sending, want 0", readBeforeSend)
	}
	if received != size {
		t.Errorf("upstream received %d bytes, want %d", received, size)
	}
}

// A retryable body larger than MaxRetryBody is streamed, so memory per request
// stays bounded by MaxRetryBody rather than the upload size.
func TestOversizedRetryableBodyIsStreamed(t *testing.T) {
	const size = 4 << 20
	src := &countingReader{r: strings.NewReader(strings.Repeat("x", size))}

	var readBeforeSend int64 = -1
	var received int64
	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{"http://upstream"}, Timeout: 5 * time.Second,
		Retry: RetryPolicy{Attempts: 2, Backoff: time.Millisecond, IdempotentOnly: true},
		Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			readBeforeSend = src.n.Load()
			n, err := io.Copy(io.Discard, r.Body)
			received = n
			return okResponse(r), err
		}),
	})

	req := httptest.NewRequest(http.MethodPut, "http://gateway/upload", src)
	req.RemoteAddr = "10.0.0.1:1"
	resp, _, err := b.Do(req, "/upload")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if readBeforeSend > 1<<20+1<<16 {
		t.Errorf("%d bytes were buffered before sending, want at most about the 1 MiB default", readBeforeSend)
	}
	if received != size {
		t.Errorf("upstream received %d bytes, want %d", received, size)
	}
}

// Small idempotent bodies are still buffered and replayed intact on retry.
func TestRetryReplaysSmallBody(t *testing.T) {
	var calls atomic.Int64
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newTestBackend(t, Options{
		Name: "t", Hosts: []string{srv.URL}, Timeout: 5 * time.Second,
		Retry: RetryPolicy{Attempts: 2, Backoff: time.Millisecond, IdempotentOnly: true},
	})

	req := httptest.NewRequest(http.MethodPut, "http://gateway/x", strings.NewReader(`{"a":1}`))
	req.RemoteAddr = "10.0.0.1:1"
	resp, res, err := b.Do(req, "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || res.Attempts != 2 {
		t.Fatalf("status %d after %d attempts, want 200 after 2", resp.StatusCode, res.Attempts)
	}
	for i, got := range bodies {
		if got != `{"a":1}` {
			t.Errorf("attempt %d body = %q", i+1, got)
		}
	}
}
