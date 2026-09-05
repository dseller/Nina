// Package specsrc fetches upstream OpenAPI documents from files or URLs and
// caches the last good copy so a momentarily unreachable upstream cannot empty
// the route table.
package specsrc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Source identifies one upstream document.
type Source struct {
	// Backend is used for cache filenames and error messages.
	Backend string
	File    string // absolute path, or empty
	URL     string // http(s) URL, or empty
	// OnError is "fail" or "stale".
	OnError string
	// InsecureSkipVerify disables TLS certificate verification when fetching
	// this document, matching the backend's own setting.
	InsecureSkipVerify bool
}

func (s Source) String() string {
	if s.File != "" {
		return s.File
	}
	return s.URL
}

// Result is a fetched document.
type Result struct {
	Data []byte
	// Hash is a content fingerprint; an unchanged hash lets the caller skip a
	// full runtime rebuild.
	Hash string
	// Stale is true when the upstream could not be reached and a cached copy was
	// used instead.
	Stale bool
	// NotModified is true when the server answered 304 and Data came from cache.
	NotModified bool
}

// Fetcher retrieves documents, remembering validators and caching bodies on disk.
type Fetcher struct {
	Client   *http.Client
	CacheDir string

	mu    sync.Mutex
	conds map[string]condition // keyed by URL

	// insecure is built on first use by a source that opted out of certificate
	// verification. It is separate from Client so that one lax backend cannot
	// weaken fetches for every other one.
	insecureOnce sync.Once
	insecure     *http.Client
}

// clientFor picks the HTTP client a source should be fetched with.
//
// Note that skipping verification applies to the whole redirect chain, so a
// spec URL that redirects elsewhere is fetched without verification too. That
// is inherent to the setting rather than specific to this implementation.
func (f *Fetcher) clientFor(src Source) *http.Client {
	if !src.InsecureSkipVerify {
		return f.Client
	}
	f.insecureOnce.Do(func() {
		f.insecure = &http.Client{
			Timeout:       f.Client.Timeout,
			CheckRedirect: f.Client.CheckRedirect,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				TLSHandshakeTimeout: 10 * time.Second,
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- opt-in, see config.TLSConfig
			},
		}
	})
	return f.insecure
}

type condition struct {
	etag         string
	lastModified string
}

func New(cacheDir string) *Fetcher {
	return &Fetcher{
		Client: &http.Client{
			Timeout: 30 * time.Second,
			// Spec endpoints sometimes redirect to a CDN; allow a couple of hops
			// but not an open-ended chain.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		CacheDir: cacheDir,
		conds:    map[string]condition{},
	}
}

// Fetch retrieves a document, applying the source's failure policy.
func (f *Fetcher) Fetch(ctx context.Context, src Source) (*Result, error) {
	if src.File != "" {
		data, err := os.ReadFile(src.File)
		if err != nil {
			// A missing local file is always fatal: unlike a network blip, it will
			// not fix itself, and "stale" would hide a deployment mistake.
			return nil, fmt.Errorf("backend %q: read spec %s: %w", src.Backend, src.File, err)
		}
		return &Result{Data: data, Hash: hash(data)}, nil
	}

	res, err := f.fetchURL(ctx, src)
	if err == nil {
		return res, nil
	}

	cached, cacheErr := f.readCache(src)
	if cacheErr != nil || len(cached) == 0 {
		return nil, fmt.Errorf("backend %q: fetch spec %s: %w", src.Backend, src.URL, err)
	}
	if src.OnError != "stale" {
		return nil, fmt.Errorf("backend %q: fetch spec %s: %w (a cached copy exists; set spec.on_error: stale to use it)",
			src.Backend, src.URL, err)
	}
	return &Result{Data: cached, Hash: hash(cached), Stale: true}, nil
}

func (f *Fetcher) fetchURL(ctx context.Context, src Source) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/yaml, text/yaml;q=0.9, */*;q=0.5")
	req.Header.Set("User-Agent", "nina-gateway")

	f.mu.Lock()
	c := f.conds[src.URL]
	f.mu.Unlock()
	if c.etag != "" {
		req.Header.Set("If-None-Match", c.etag)
	}
	if c.lastModified != "" {
		req.Header.Set("If-Modified-Since", c.lastModified)
	}

	resp, err := f.clientFor(src).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		cached, err := f.readCache(src)
		if err != nil {
			// We claimed to have a copy but do not; drop the validators so the
			// next attempt is unconditional.
			f.mu.Lock()
			delete(f.conds, src.URL)
			f.mu.Unlock()
			return nil, fmt.Errorf("server returned 304 but no cached copy is available")
		}
		return &Result{Data: cached, Hash: hash(cached), NotModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	// Cap the read: a spec endpoint that starts streaming something enormous
	// should not take the gateway's memory with it.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	f.conds[src.URL] = condition{
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
	}
	f.mu.Unlock()

	if err := f.writeCache(src, data); err != nil {
		// A cache write failure must not fail the fetch; we simply lose the
		// stale-copy safety net until it succeeds.
		_ = err
	}
	return &Result{Data: data, Hash: hash(data)}, nil
}

func (f *Fetcher) cachePath(src Source) string {
	if f.CacheDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(src.URL))
	return filepath.Join(f.CacheDir, src.Backend+"-"+hex.EncodeToString(sum[:6])+".spec")
}

func (f *Fetcher) readCache(src Source) ([]byte, error) {
	p := f.cachePath(src)
	if p == "" {
		return nil, fmt.Errorf("no cache directory configured")
	}
	return os.ReadFile(p)
}

func (f *Fetcher) writeCache(src Source, data []byte) error {
	p := f.cachePath(src)
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// Write-and-rename so a crash mid-write cannot leave a truncated cache that
	// would later be served as a "good" stale copy.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
