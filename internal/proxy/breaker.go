package proxy

import (
	"errors"
	"sync"
	"time"
)

// State is a circuit breaker's state.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// ErrBreakerOpen is returned when the breaker is refusing calls.
var ErrBreakerOpen = errors.New("circuit breaker is open")

// Breaker is a per-backend circuit breaker.
//
// It is written here rather than taken from a library because the gateway needs
// to publish its state as a metric and to control exactly when a half-open probe
// is admitted; wrapping a library to get both back is more code than this.
type Breaker struct {
	failureRatio float64
	minRequests  int
	openFor      time.Duration
	now          func() time.Time // injectable for tests

	mu        sync.Mutex
	state     State
	successes int
	failures  int
	openedAt  time.Time
	probing   bool // a half-open probe is in flight
	onChange  func(State)
}

// BreakerConfig configures a Breaker. A zero FailureRatio disables the breaker.
type BreakerConfig struct {
	FailureRatio float64
	MinRequests  int
	OpenFor      time.Duration
	OnChange     func(State)
}

func NewBreaker(c BreakerConfig) *Breaker {
	if c.MinRequests <= 0 {
		c.MinRequests = 20
	}
	if c.OpenFor <= 0 {
		c.OpenFor = 10 * time.Second
	}
	return &Breaker{
		failureRatio: c.FailureRatio,
		minRequests:  c.MinRequests,
		openFor:      c.OpenFor,
		now:          time.Now,
		onChange:     c.OnChange,
	}
}

// State reports the current state.
func (b *Breaker) State() State {
	if b == nil {
		return StateClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpen()
	return b.state
}

// Allow reserves a slot. The returned done function must be called with the
// outcome. When the breaker is open, Allow returns ErrBreakerOpen.
func (b *Breaker) Allow() (done func(success bool), err error) {
	if b == nil || b.failureRatio <= 0 {
		return func(bool) {}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpen()

	switch b.state {
	case StateOpen:
		return nil, ErrBreakerOpen
	case StateHalfOpen:
		if b.probing {
			// Exactly one probe at a time: a thundering herd against a recovering
			// upstream is how a brief outage becomes a long one.
			return nil, ErrBreakerOpen
		}
		b.probing = true
		return func(success bool) { b.reportProbe(success) }, nil
	default:
		return func(success bool) { b.report(success) }, nil
	}
}

// maybeHalfOpen promotes an open breaker to half-open once its timer expires.
// Callers must hold the lock.
func (b *Breaker) maybeHalfOpen() {
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.openFor {
		b.setState(StateHalfOpen)
		b.probing = false
	}
}

func (b *Breaker) report(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if success {
		b.successes++
	} else {
		b.failures++
	}
	total := b.successes + b.failures
	if total < b.minRequests {
		return
	}
	if float64(b.failures)/float64(total) >= b.failureRatio {
		b.trip()
		return
	}
	// Reset the window once it is decisively healthy, so old failures cannot
	// accumulate indefinitely and trip the breaker much later.
	if total >= b.minRequests*2 {
		b.successes, b.failures = 0, 0
	}
}

func (b *Breaker) reportProbe(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if success {
		b.successes, b.failures = 0, 0
		b.setState(StateClosed)
		return
	}
	b.trip()
}

func (b *Breaker) trip() {
	b.openedAt = b.now()
	b.successes, b.failures = 0, 0
	b.setState(StateOpen)
}

func (b *Breaker) setState(s State) {
	if b.state == s {
		return
	}
	b.state = s
	if b.onChange != nil {
		b.onChange(s)
	}
}
