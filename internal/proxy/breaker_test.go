package proxy

import (
	"testing"
	"time"
)

func newTestBreaker(ratio float64, minReq int, openFor time.Duration) (*Breaker, *time.Time) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{FailureRatio: ratio, MinRequests: minReq, OpenFor: openFor})
	b.now = func() time.Time { return now }
	return b, &now
}

func report(t *testing.T, b *Breaker, success bool) {
	t.Helper()
	done, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	done(success)
}

func TestBreakerStaysClosedBelowMinRequests(t *testing.T) {
	b, _ := newTestBreaker(0.5, 10, time.Second)
	// All failing, but not enough traffic to judge.
	for i := 0; i < 9; i++ {
		report(t, b, false)
	}
	if b.State() != StateClosed {
		t.Errorf("state = %v, want closed below min_requests", b.State())
	}
}

func TestBreakerTripsAtThreshold(t *testing.T) {
	b, _ := newTestBreaker(0.5, 10, time.Second)
	for i := 0; i < 10; i++ {
		report(t, b, false)
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}
	if _, err := b.Allow(); err != ErrBreakerOpen {
		t.Errorf("Allow err = %v, want ErrBreakerOpen", err)
	}
}

func TestBreakerDoesNotTripBelowRatio(t *testing.T) {
	b, _ := newTestBreaker(0.5, 10, time.Second)
	// 4 failures out of 10 is under the ratio.
	for i := 0; i < 6; i++ {
		report(t, b, true)
	}
	for i := 0; i < 4; i++ {
		report(t, b, false)
	}
	if b.State() != StateClosed {
		t.Errorf("state = %v, want closed at 40%% failures", b.State())
	}
}

func TestBreakerHalfOpensAndRecovers(t *testing.T) {
	b, now := newTestBreaker(0.5, 4, time.Second)
	for i := 0; i < 4; i++ {
		report(t, b, false)
	}
	if b.State() != StateOpen {
		t.Fatal("expected open")
	}

	*now = now.Add(2 * time.Second)
	if b.State() != StateHalfOpen {
		t.Fatalf("state = %v, want half-open after open_for", b.State())
	}

	done, err := b.Allow()
	if err != nil {
		t.Fatalf("half-open probe was refused: %v", err)
	}
	// Only one probe at a time.
	if _, err := b.Allow(); err != ErrBreakerOpen {
		t.Error("a second concurrent probe should be refused")
	}
	done(true)
	if b.State() != StateClosed {
		t.Errorf("state = %v, want closed after a successful probe", b.State())
	}
}

func TestBreakerReopensOnFailedProbe(t *testing.T) {
	b, now := newTestBreaker(0.5, 4, time.Second)
	for i := 0; i < 4; i++ {
		report(t, b, false)
	}
	*now = now.Add(2 * time.Second)

	done, err := b.Allow()
	if err != nil {
		t.Fatal(err)
	}
	done(false)
	if b.State() != StateOpen {
		t.Errorf("state = %v, want open again after a failed probe", b.State())
	}
}

func TestBreakerDisabledWhenRatioIsZero(t *testing.T) {
	b := NewBreaker(BreakerConfig{})
	for i := 0; i < 100; i++ {
		done, err := b.Allow()
		if err != nil {
			t.Fatalf("a disabled breaker refused a call: %v", err)
		}
		done(false)
	}
	if b.State() != StateClosed {
		t.Errorf("state = %v, want closed", b.State())
	}
}

func TestBreakerNotifiesStateChanges(t *testing.T) {
	var seen []State
	b := NewBreaker(BreakerConfig{
		FailureRatio: 0.5, MinRequests: 2, OpenFor: time.Second,
		OnChange: func(s State) { seen = append(seen, s) },
	})
	now := time.Now()
	b.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		report(t, b, false)
	}
	now = now.Add(2 * time.Second)
	done, _ := b.Allow()
	done(true)

	want := []State{StateOpen, StateHalfOpen, StateClosed}
	if len(seen) != len(want) {
		t.Fatalf("transitions = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("transitions = %v, want %v", seen, want)
			break
		}
	}
}
