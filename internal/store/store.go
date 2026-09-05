// Package store provides the pluggable backing stores middleware needs: rate
// limit counters and response caches, in memory or in Redis.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/redis/go-redis/v9"
)

// Decision is the outcome of a rate limit check.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
	// Degraded is true when the store was unreachable and the limiter failed
	// open. The caller should surface this as a metric.
	Degraded bool
}

// Limiter counts requests per key over a window.
type Limiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error)
}

// Registry holds the configured stores. Redis is optional.
type Registry struct {
	redis *redis.Client
	// memLimiters are shared per (name) so that two routes naming the same
	// middleware share one set of counters.
	mu          sync.Mutex
	memLimiters map[string]*MemoryLimiter
}

type RedisOptions struct {
	Addr     string
	Password string
	DB       int
	Timeout  time.Duration
}

func NewRegistry(ro *RedisOptions) (*Registry, error) {
	r := &Registry{memLimiters: map[string]*MemoryLimiter{}}
	if ro == nil {
		return r, nil
	}
	if ro.Timeout == 0 {
		ro.Timeout = 200 * time.Millisecond
	}
	r.redis = redis.NewClient(&redis.Options{
		Addr:         ro.Addr,
		Password:     ro.Password,
		DB:           ro.DB,
		DialTimeout:  ro.Timeout,
		ReadTimeout:  ro.Timeout,
		WriteTimeout: ro.Timeout,
	})
	return r, nil
}

func (r *Registry) HasRedis() bool { return r != nil && r.redis != nil }

// Ping checks Redis reachability. A failure is reported but is not fatal: the
// gateway should still start, with limiters degraded, rather than refuse to
// serve because a counter store is down.
func (r *Registry) Ping(ctx context.Context) error {
	if r == nil || r.redis == nil {
		return nil
	}
	return r.redis.Ping(ctx).Err()
}

func (r *Registry) Close() error {
	if r != nil && r.redis != nil {
		return r.redis.Close()
	}
	return nil
}

// Limiter returns a limiter for the named middleware. store selects "memory" or
// "redis"; an unavailable Redis is an error at build time so the operator finds
// out at reload rather than under load.
func (r *Registry) Limiter(name, store string, maxKeys int, failOpen bool) (Limiter, error) {
	switch store {
	case "", "memory":
		r.mu.Lock()
		defer r.mu.Unlock()
		if l, ok := r.memLimiters[name]; ok {
			return l, nil
		}
		l, err := NewMemoryLimiter(maxKeys)
		if err != nil {
			return nil, err
		}
		r.memLimiters[name] = l
		return l, nil
	case "redis":
		if r.redis == nil {
			return nil, errors.New("store: redis is not configured (add stores.redis to the config)")
		}
		return &RedisLimiter{client: r.redis, failOpen: failOpen}, nil
	default:
		return nil, fmt.Errorf("store: unknown store %q, expected memory or redis", store)
	}
}

// ---------------------------------------------------------------------------
// Memory limiter
// ---------------------------------------------------------------------------

// MemoryLimiter is a per-process sliding window counter.
//
// Its key set is LRU-bounded: rate limit keys are derived from request data
// (a subject, an API key, a client IP), so an attacker can otherwise mint
// unbounded keys and turn the limiter into a memory leak.
type MemoryLimiter struct {
	mu    sync.Mutex
	cache *lru.Cache[string, *window]
}

type window struct {
	start time.Time
	count int
	prev  int // count in the previous window, for the sliding estimate
}

func NewMemoryLimiter(maxKeys int) (*MemoryLimiter, error) {
	if maxKeys <= 0 {
		maxKeys = 100_000
	}
	c, err := lru.New[string, *window](maxKeys)
	if err != nil {
		return nil, err
	}
	return &MemoryLimiter{cache: c}, nil
}

func (m *MemoryLimiter) Allow(_ context.Context, key string, limit int, dur time.Duration) (Decision, error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	w, ok := m.cache.Get(key)
	if !ok {
		w = &window{start: now}
		m.cache.Add(key, w)
	}
	// Roll the window forward, keeping one generation of history so the estimate
	// slides rather than resetting to zero on a boundary.
	elapsed := now.Sub(w.start)
	switch {
	case elapsed >= 2*dur:
		w.start, w.count, w.prev = now, 0, 0
	case elapsed >= dur:
		w.start, w.prev, w.count = w.start.Add(dur), w.count, 0
	}

	frac := float64(dur-now.Sub(w.start)) / float64(dur)
	if frac < 0 {
		frac = 0
	}
	estimate := float64(w.prev)*frac + float64(w.count)

	if int(estimate) >= limit {
		retry := dur - now.Sub(w.start)
		if retry < 0 {
			retry = dur
		}
		return Decision{Limit: limit, Remaining: 0, RetryAfter: retry}, nil
	}
	w.count++
	remaining := limit - int(estimate) - 1
	if remaining < 0 {
		remaining = 0
	}
	return Decision{Allowed: true, Limit: limit, Remaining: remaining}, nil
}

// ---------------------------------------------------------------------------
// Redis limiter
// ---------------------------------------------------------------------------

// slidingWindow increments a counter for the current window and reports the
// sliding estimate. Running it as one script keeps the read-modify-write atomic
// across replicas, which is the entire point of using Redis here.
var slidingWindow = redis.NewScript(`
local key    = KEYS[1]
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local now    = tonumber(ARGV[3])

local bucket = math.floor(now / window)
local cur_key  = key .. ":" .. bucket
local prev_key = key .. ":" .. (bucket - 1)

local cur  = tonumber(redis.call("GET", cur_key))  or 0
local prev = tonumber(redis.call("GET", prev_key)) or 0

local elapsed = now % window
local frac = (window - elapsed) / window
local estimate = prev * frac + cur

if estimate >= limit then
  return {0, 0, window - elapsed}
end

cur = redis.call("INCR", cur_key)
redis.call("PEXPIRE", cur_key, window * 2)

local remaining = limit - math.floor(estimate) - 1
if remaining < 0 then remaining = 0 end
return {1, remaining, 0}
`)

// RedisLimiter enforces limits across every gateway replica.
type RedisLimiter struct {
	client *redis.Client
	// failOpen decides what happens when Redis is unreachable. Defaulting to
	// open means a dead counter store degrades rate limiting rather than causing
	// a total outage; operators who need the opposite can say so.
	failOpen bool
}

func (r *RedisLimiter) Allow(ctx context.Context, key string, limit int, dur time.Duration) (Decision, error) {
	now := time.Now().UnixMilli()
	res, err := slidingWindow.Run(ctx, r.client,
		[]string{"nina:rl:" + key},
		limit, dur.Milliseconds(), now,
	).Int64Slice()
	if err != nil {
		if r.failOpen {
			return Decision{Allowed: true, Limit: limit, Remaining: limit, Degraded: true}, nil
		}
		return Decision{Degraded: true}, fmt.Errorf("rate limit store unavailable: %w", err)
	}
	if len(res) < 3 {
		return Decision{Allowed: true, Limit: limit, Degraded: true}, nil
	}
	return Decision{
		Allowed:    res[0] == 1,
		Limit:      limit,
		Remaining:  int(res[1]),
		RetryAfter: time.Duration(res[2]) * time.Millisecond,
	}, nil
}
