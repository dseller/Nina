// Package proxy forwards requests to upstream backends with timeouts, bounded
// retries, load balancing and circuit breaking.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// hopByHop headers must not be forwarded. Anything named in the Connection
// header joins them.
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// RetryPolicy bounds retries against an upstream.
type RetryPolicy struct {
	Attempts       int // additional attempts beyond the first
	Backoff        time.Duration
	Jitter         bool
	IdempotentOnly bool
}

// Options configures a Backend.
type Options struct {
	Name           string
	Hosts          []string
	LoadBalance    string
	Timeout        time.Duration
	Retry          RetryPolicy
	Breaker        BreakerConfig
	ForwardHeaders map[string]string
	// MaxRetryBody bounds how much of a request body will be buffered so a retry
	// can replay it. Larger bodies simply are not retried.
	MaxRetryBody int64
	// Transport overrides the default; used by tests.
	Transport http.RoundTripper
}

// Backend is one upstream service.
type Backend struct {
	Name    string
	pool    *Pool
	breaker *Breaker

	transport      http.RoundTripper
	closeTransport func()
	timeout        time.Duration
	retry          RetryPolicy
	forwardHeaders map[string]string
	maxRetryBody   int64
}

// NewBackend builds a backend with its own connection pool. Each backend gets a
// dedicated transport so that one slow upstream cannot exhaust the connections
// another one needs.
func NewBackend(o Options) (*Backend, error) {
	var urls []*url.URL
	for _, h := range o.Hosts {
		u, err := url.Parse(strings.TrimSuffix(h, "/"))
		if err != nil {
			return nil, fmt.Errorf("backend %s: bad host %q: %w", o.Name, h, err)
		}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("backend %s: no hosts", o.Name)
	}

	b := &Backend{
		Name:           o.Name,
		pool:           NewPool(urls, o.LoadBalance),
		breaker:        NewBreaker(o.Breaker),
		timeout:        o.Timeout,
		retry:          o.Retry,
		forwardHeaders: o.ForwardHeaders,
		maxRetryBody:   o.MaxRetryBody,
	}
	if b.maxRetryBody == 0 {
		b.maxRetryBody = 1 << 20
	}

	if o.Transport != nil {
		b.transport = o.Transport
		b.closeTransport = func() {}
	} else {
		t := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		}
		b.transport = t
		b.closeTransport = t.CloseIdleConnections
	}
	return b, nil
}

func (b *Backend) Pool() *Pool       { return b.pool }
func (b *Backend) Breaker() *Breaker { return b.breaker }

// Close releases idle connections. In-flight requests are unaffected; the caller
// is expected to have drained them first.
func (b *Backend) Close() {
	if b.closeTransport != nil {
		b.closeTransport()
	}
}

// StartHealthChecks begins active checking if configured.
func (b *Backend) StartHealthChecks(ctx context.Context, cfg HealthCheckConfig, onChange func(*Host, bool)) {
	client := &http.Client{Transport: b.transport, Timeout: cfg.Timeout}
	b.pool.StartHealthChecks(ctx, cfg, client, onChange)
}

// Kind classifies an upstream failure so the caller can pick a status code.
type Kind string

const (
	KindBreakerOpen Kind = "breaker_open"
	KindNoHost      Kind = "no_healthy_host"
	KindTimeout     Kind = "timeout"
	KindConnect     Kind = "connect"
	KindCanceled    Kind = "canceled"
)

// Error is an upstream failure with enough structure to map onto a status code
// and a metric label.
type Error struct {
	Kind    Kind
	Backend string
	Err     error
}

func (e *Error) Error() string {
	return fmt.Sprintf("backend %s: %s: %v", e.Backend, e.Kind, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

// StatusCode maps a failure onto the response the client should see.
func (e *Error) StatusCode() int {
	switch e.Kind {
	case KindTimeout:
		return http.StatusGatewayTimeout
	case KindBreakerOpen, KindNoHost:
		return http.StatusServiceUnavailable
	case KindCanceled:
		// The client went away; nothing will read this, but 499 records why.
		return 499
	default:
		return http.StatusBadGateway
	}
}

// Result carries what happened, for metrics and logging.
type Result struct {
	Host     *Host
	Attempts int
	Upstream time.Duration
}

// Do forwards req to the backend and returns the upstream response. The caller
// owns the response body.
//
// upstreamPath is the fully resolved path (parameters already substituted).
func (b *Backend) Do(req *http.Request, upstreamPath string) (*http.Response, *Result, error) {
	ctx := req.Context()
	cancel := context.CancelFunc(func() {})
	if b.timeout > 0 {
		// The timeout has to cover reading the response body, not just getting
		// the headers, so cancellation is tied to the body being closed rather
		// than to this function returning.
		ctx, cancel = context.WithTimeout(ctx, b.timeout)
		req = req.WithContext(ctx)
	}
	failed := true
	defer func() {
		if failed {
			cancel()
		}
	}()

	body, replayable, err := b.snapshotBody(req)
	if err != nil {
		return nil, nil, &Error{Kind: KindConnect, Backend: b.Name, Err: err}
	}

	attempts := 1 + b.retry.Attempts
	if !b.retryable(req, replayable) {
		attempts = 1
	}

	res := &Result{}
	var lastErr error
	var lastHost *Host

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := b.sleepBackoff(ctx, attempt); err != nil {
				return nil, res, classify(b.Name, err)
			}
		}
		res.Attempts = attempt + 1

		host, err := b.pool.Pick(lastHost)
		if err != nil {
			return nil, res, &Error{Kind: KindNoHost, Backend: b.Name, Err: err}
		}
		lastHost = host
		res.Host = host

		done, err := b.breaker.Allow()
		if err != nil {
			return nil, res, &Error{Kind: KindBreakerOpen, Backend: b.Name, Err: err}
		}

		out := b.buildUpstreamRequest(req, host, upstreamPath, body)
		host.inflight.Add(1)
		start := time.Now()
		resp, err := b.transport.RoundTrip(out)
		host.inflight.Add(-1)
		res.Upstream = time.Since(start)

		if err != nil {
			done(false)
			lastErr = err
			if ctx.Err() != nil {
				return nil, res, classify(b.Name, ctx.Err())
			}
			continue
		}
		// A 5xx the upstream produced deliberately is still a failure for the
		// breaker, but only the three "try elsewhere" codes justify a retry.
		success := resp.StatusCode < 500
		done(success)
		if !success && attempt < attempts-1 && retryableStatus(resp.StatusCode) {
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream returned %s", resp.Status)
			continue
		}
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		failed = false
		return resp, res, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no attempt was made")
	}
	return nil, res, classify(b.Name, lastErr)
}

// cancelOnClose releases a request's timeout context once the caller is done
// reading the response body.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}

func retryableStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// retryable reports whether this request may be retried at all. Non-idempotent
// methods are excluded by default: replaying a POST can create two orders.
func (b *Backend) retryable(req *http.Request, replayable bool) bool {
	if b.retry.Attempts <= 0 {
		return false
	}
	if !replayable {
		return false
	}
	if !b.retry.IdempotentOnly {
		return true
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	default:
		return req.Header.Get("Idempotency-Key") != ""
	}
}

// snapshotBody buffers the request body when it is small enough to replay.
func (b *Backend) snapshotBody(req *http.Request) (buf []byte, replayable bool, err error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true, nil
	}
	if b.retry.Attempts <= 0 {
		return nil, false, nil
	}
	limited := io.LimitReader(req.Body, b.maxRetryBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > b.maxRetryBody {
		// Too large to hold: put it back as a stream and give up on retrying.
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), req.Body))
		return nil, false, nil
	}
	req.Body.Close()
	return data, true, nil
}

func (b *Backend) sleepBackoff(ctx context.Context, attempt int) error {
	d := b.retry.Backoff * time.Duration(1<<(attempt-1))
	if b.retry.Jitter && d > 0 {
		// Full jitter: spreads a retry storm instead of synchronising it.
		d = time.Duration(rand.Int63n(int64(d)) + int64(d)/2)
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// buildUpstreamRequest clones the inbound request onto the chosen host.
func (b *Backend) buildUpstreamRequest(req *http.Request, host *Host, upstreamPath string, body []byte) *http.Request {
	u := *host.URL
	u.Path = singleSlashJoin(host.URL.Path, upstreamPath)
	u.RawQuery = req.URL.RawQuery

	out := req.Clone(req.Context())
	out.URL = &u
	out.Host = "" // let the transport derive Host from the URL
	out.RequestURI = ""
	out.Close = false

	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}

	stripHopByHop(out.Header)
	setForwardedHeaders(out, req)
	for k, v := range b.forwardHeaders {
		out.Header.Set(k, v)
	}
	return out
}

// stripHopByHop removes connection-scoped headers, including any the Connection
// header names.
func stripHopByHop(h http.Header) {
	for _, c := range h.Values("Connection") {
		for _, name := range strings.Split(c, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHop {
		h.Del(name)
	}
}

func setForwardedHeaders(out, in *http.Request) {
	if ip, _, err := net.SplitHostPort(in.RemoteAddr); err == nil {
		if prior := in.Header.Get("X-Forwarded-For"); prior != "" {
			out.Header.Set("X-Forwarded-For", prior+", "+ip)
		} else {
			out.Header.Set("X-Forwarded-For", ip)
		}
	}
	proto := "http"
	if in.TLS != nil {
		proto = "https"
	}
	if xf := in.Header.Get("X-Forwarded-Proto"); xf != "" {
		proto = xf
	}
	out.Header.Set("X-Forwarded-Proto", proto)
	if in.Host != "" {
		out.Header.Set("X-Forwarded-Host", in.Host)
	}
}

// singleSlashJoin joins a host base path with a request path without doubling or
// dropping the separator.
func singleSlashJoin(base, p string) string {
	base = strings.TrimSuffix(base, "/")
	if p == "" {
		if base == "" {
			return "/"
		}
		return base
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return base + p
}

func classify(backend string, err error) *Error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Kind: KindTimeout, Backend: backend, Err: err}
	case errors.Is(err, context.Canceled):
		return &Error{Kind: KindCanceled, Backend: backend, Err: err}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Kind: KindTimeout, Backend: backend, Err: err}
	}
	return &Error{Kind: KindConnect, Backend: backend, Err: err}
}

// CopyResponse streams an upstream response to the client. The body is never
// buffered, so large downloads and chunked responses pass straight through.
func CopyResponse(w http.ResponseWriter, resp *http.Response) (int64, error) {
	h := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	stripHopByHop(h)
	w.WriteHeader(resp.StatusCode)
	n, err := io.Copy(w, resp.Body)
	// Flush any buffered bytes so a client waiting on a slow trickle is not left
	// hanging on the server's write buffer.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}
