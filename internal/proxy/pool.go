package proxy

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoHealthyHost is returned when every upstream host is failing its health
// check.
var ErrNoHealthyHost = errors.New("no healthy upstream host")

// Host is one upstream origin.
type Host struct {
	URL *url.URL

	healthy  atomic.Bool
	inflight atomic.Int64
	// consecutive successes/failures of the active health check.
	okStreak   int
	failStreak int
}

func (h *Host) Healthy() bool { return h.healthy.Load() }

// Pool selects among a backend's hosts.
type Pool struct {
	hosts    []*Host
	strategy string
	rr       atomic.Uint64
}

func NewPool(urls []*url.URL, strategy string) *Pool {
	p := &Pool{strategy: strategy}
	for _, u := range urls {
		h := &Host{URL: u}
		// Assume healthy until a check says otherwise; the alternative is
		// refusing all traffic for the first health-check interval after a
		// deploy.
		h.healthy.Store(true)
		p.hosts = append(p.hosts, h)
	}
	return p
}

func (p *Pool) Hosts() []*Host { return p.hosts }

// Pick returns a healthy host. exclude lets a retry avoid the host that just
// failed. If every healthy host is excluded, it falls back to any healthy host
// rather than failing the request outright.
func (p *Pool) Pick(exclude *Host) (*Host, error) {
	healthy := make([]*Host, 0, len(p.hosts))
	for _, h := range p.hosts {
		if h.Healthy() {
			healthy = append(healthy, h)
		}
	}
	if len(healthy) == 0 {
		return nil, ErrNoHealthyHost
	}
	candidates := healthy
	if exclude != nil && len(healthy) > 1 {
		filtered := make([]*Host, 0, len(healthy))
		for _, h := range healthy {
			if h != exclude {
				filtered = append(filtered, h)
			}
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}

	switch p.strategy {
	case "random":
		return candidates[rand.Intn(len(candidates))], nil
	case "least_conn":
		best := candidates[0]
		for _, h := range candidates[1:] {
			if h.inflight.Load() < best.inflight.Load() {
				best = h
			}
		}
		return best, nil
	default: // round_robin
		i := p.rr.Add(1)
		return candidates[int(i-1)%len(candidates)], nil
	}
}

// HealthCheckConfig configures active checking.
type HealthCheckConfig struct {
	Path           string
	Interval       time.Duration
	Timeout        time.Duration
	UnhealthyAfter int
	HealthyAfter   int
}

// StartHealthChecks polls each host until ctx is cancelled. Transitions are
// hysteretic: a host has to fail (or pass) repeatedly before its state flips, so
// one unlucky probe does not shed traffic.
func (p *Pool) StartHealthChecks(ctx context.Context, cfg HealthCheckConfig, client *http.Client, onChange func(*Host, bool)) {
	var mu sync.Mutex
	check := func(h *Host) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL.String()+cfg.Path, nil)
		var ok bool
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			req = req.WithContext(cctx)
			resp, rerr := client.Do(req)
			if rerr == nil {
				ok = resp.StatusCode >= 200 && resp.StatusCode < 400
				resp.Body.Close()
			}
			cancel()
		}

		mu.Lock()
		defer mu.Unlock()
		was := h.Healthy()
		if ok {
			h.failStreak = 0
			h.okStreak++
			if !was && h.okStreak >= cfg.HealthyAfter {
				h.healthy.Store(true)
				if onChange != nil {
					onChange(h, true)
				}
			}
			return
		}
		h.okStreak = 0
		h.failStreak++
		if was && h.failStreak >= cfg.UnhealthyAfter {
			h.healthy.Store(false)
			if onChange != nil {
				onChange(h, false)
			}
		}
	}

	for _, h := range p.hosts {
		go func(h *Host) {
			t := time.NewTicker(cfg.Interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					check(h)
				}
			}
		}(h)
	}
}
