package scanguard

import (
	"context"
	"sort"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// memoryKeys bounds each of the memory store's tables. Keys come from client
// addresses, so without a bound a scanner rotating addresses would turn the
// guard into a memory leak. Under that kind of churn the oldest entries go
// first, which is the right thing to lose.
const memoryKeys = 100_000

// MemoryStore keeps the guard's state in this process.
//
// It is meant to be created once and shared by every runtime, so bans and
// counters survive a reload.
type MemoryStore struct {
	mu      sync.Mutex
	misses  *lru.Cache[string, *missWindow]
	bans    *lru.Cache[string, Ban]
	strikes *lru.Cache[string, strike]
	now     func() time.Time
}

type missWindow struct {
	start time.Time
	count int
}

type strike struct {
	count   int
	expires time.Time
}

func NewMemoryStore() *MemoryStore {
	m, _ := lru.New[string, *missWindow](memoryKeys)
	b, _ := lru.New[string, Ban](memoryKeys)
	s, _ := lru.New[string, strike](memoryKeys)
	return &MemoryStore{misses: m, bans: b, strikes: s, now: time.Now}
}

func (m *MemoryStore) BannedFor(_ context.Context, key string) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bans.Peek(key)
	if !ok {
		return 0, nil
	}
	left := b.Until.Sub(m.now())
	if left <= 0 {
		m.bans.Remove(key)
		return 0, nil
	}
	return left, nil
}

// Miss counts in fixed windows. A scanner sits far above any sensible
// threshold, so the precision of a sliding window buys nothing here.
func (m *MemoryStore) Miss(_ context.Context, key string, window time.Duration) (int, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.misses.Get(key)
	if !ok || now.Sub(w.start) >= window {
		w = &missWindow{start: now}
		m.misses.Add(key, w)
	}
	w.count++
	return w.count, nil
}

func (m *MemoryStore) Ban(_ context.Context, b Ban, base, max time.Duration) (Ban, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.strikes.Get(b.Client)
	if !ok || !now.Before(s.expires) {
		s = strike{}
	}
	s.count++
	b.Strikes = s.count
	b.Since = now
	b.Until = now.Add(banLength(s.count, base, max))
	s.expires = b.Until.Add(max)
	m.strikes.Add(b.Client, s)
	m.bans.Add(b.Client, b)
	m.misses.Remove(b.Client)
	return b, nil
}

func (m *MemoryStore) Unban(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.strikes.Remove(key)
	m.misses.Remove(key)
	b, ok := m.bans.Peek(key)
	m.bans.Remove(key)
	return ok && b.Until.After(m.now()), nil
}

func (m *MemoryStore) List(_ context.Context) ([]Ban, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Ban{}
	for _, k := range m.bans.Keys() {
		if b, ok := m.bans.Peek(k); ok && b.Until.After(now) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out, nil
}
