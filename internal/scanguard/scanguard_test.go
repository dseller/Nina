package scanguard

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/rivencove/nina/internal/config"
)

// clock is a settable time source for the memory store.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newGuard(t *testing.T, mutate func(*config.ScanGuard)) (*Guard, *clock) {
	t.Helper()
	cfg := &config.ScanGuard{
		MaxMisses:  3,
		Window:     config.Duration(time.Minute),
		Ban:        config.Duration(10 * time.Minute),
		MaxBan:     config.Duration(time.Hour),
		TrapPaths:  config.DefaultTrapPaths,
		IPv6Prefix: 64,
	}
	if mutate != nil {
		mutate(cfg)
	}
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := NewMemoryStore()
	store.now = clk.now
	g, err := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return g, clk
}

var ctx = context.Background()

func blocked(g *Guard, ip string) bool {
	_, b := g.Blocked(ctx, ip)
	return b
}

func TestThresholdBans(t *testing.T) {
	g, _ := newGuard(t, nil)
	g.Miss(ctx, "203.0.113.7", "/nope")
	g.Miss(ctx, "203.0.113.7", "/nope")
	if blocked(g, "203.0.113.7") {
		t.Fatal("banned below the threshold")
	}
	g.Miss(ctx, "203.0.113.7", "/nope")
	if !blocked(g, "203.0.113.7") {
		t.Fatal("not banned at the threshold")
	}
	if blocked(g, "203.0.113.8") {
		t.Error("a ban leaked to another client")
	}
}

func TestMissesOutsideTheWindowDoNotAccumulate(t *testing.T) {
	g, clk := newGuard(t, nil)
	for i := 0; i < 5; i++ {
		g.Miss(ctx, "203.0.113.7", "/nope")
		g.Miss(ctx, "203.0.113.7", "/nope")
		clk.advance(time.Minute)
	}
	if blocked(g, "203.0.113.7") {
		t.Error("misses spread over many windows earned a ban")
	}
}

func TestTrapPathBansOnFirstHit(t *testing.T) {
	g, _ := newGuard(t, nil)
	g.Miss(ctx, "203.0.113.7", "/.env")
	if !blocked(g, "203.0.113.7") {
		t.Fatal("a trap path did not ban")
	}
	bans, _ := g.Bans(ctx)
	if len(bans) != 1 || bans[0].Reason != ReasonTrap || bans[0].Path != "/.env" {
		t.Errorf("bans = %+v", bans)
	}
}

func TestTrapsCanBeTurnedOff(t *testing.T) {
	g, _ := newGuard(t, func(c *config.ScanGuard) { c.TrapPaths = []string{} })
	g.Miss(ctx, "203.0.113.7", "/.env")
	if blocked(g, "203.0.113.7") {
		t.Error("banned on a trap path with traps off")
	}
}

func TestTrapMatching(t *testing.T) {
	re, err := compileTraps(config.DefaultTrapPaths)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/.env", "/.env.local", "/app/.env", "/.git/config", "/wp-login.php",
		"/wp-admin/install.php", "/index.php", "/PHPMYADMIN/", "/cgi-bin/luci", "/.aws/credentials",
		"/WP-LOGIN.PHP",
	} {
		if !re.MatchString(p) {
			t.Errorf("%s should be a trap", p)
		}
	}
	for _, p := range []string{"/api/users", "/env", "/phpinfo", "/docs/wp", "/php"} {
		if re.MatchString(p) {
			t.Errorf("%s should not be a trap", p)
		}
	}
}

func TestBanExpires(t *testing.T) {
	g, clk := newGuard(t, nil)
	g.Miss(ctx, "203.0.113.7", "/.env")
	left, _ := g.Blocked(ctx, "203.0.113.7")
	if left != 10*time.Minute {
		t.Errorf("first ban = %v, want 10m", left)
	}
	clk.advance(10 * time.Minute)
	if blocked(g, "203.0.113.7") {
		t.Error("ban outlived its length")
	}
}

func TestRepeatBansDoubleUpToTheCap(t *testing.T) {
	g, clk := newGuard(t, nil)
	want := []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		g.Miss(ctx, "203.0.113.7", "/.env")
		left, _ := g.Blocked(ctx, "203.0.113.7")
		if left != w {
			t.Errorf("ban %d = %v, want %v", i+1, left, w)
		}
		clk.advance(left)
	}
}

func TestStrikesAreForgottenMaxBanAfterTheBanEnds(t *testing.T) {
	g, clk := newGuard(t, nil)
	g.Miss(ctx, "203.0.113.7", "/.env")
	clk.advance(10*time.Minute + time.Hour)
	g.Miss(ctx, "203.0.113.7", "/.env")
	if left, _ := g.Blocked(ctx, "203.0.113.7"); left != 10*time.Minute {
		t.Errorf("ban after a clean hour = %v, want the base 10m", left)
	}
}

func TestAllowlistIsNeverBanned(t *testing.T) {
	g, _ := newGuard(t, func(c *config.ScanGuard) {
		c.AllowCIDRs = []string{"10.0.0.0/8", "198.51.100.9"}
	})
	for _, ip := range []string{"10.1.2.3", "198.51.100.9"} {
		g.Miss(ctx, ip, "/.env")
		if blocked(g, ip) {
			t.Errorf("allowlisted %s was banned", ip)
		}
	}
}

func TestIPv6IsGroupedByPrefix(t *testing.T) {
	g, _ := newGuard(t, nil)
	g.Miss(ctx, "2001:db8:1:2::1", "/.env")
	if !blocked(g, "2001:db8:1:2:ffff::9") {
		t.Error("another address in the same /64 was not banned")
	}
	if blocked(g, "2001:db8:1:3::1") {
		t.Error("a different /64 was banned")
	}
	bans, _ := g.Bans(ctx)
	if len(bans) != 1 || bans[0].Client != "2001:db8:1:2::/64" {
		t.Errorf("bans = %+v", bans)
	}
}

func TestUnban(t *testing.T) {
	g, _ := newGuard(t, nil)
	g.Miss(ctx, "203.0.113.7", "/.env")
	g.Miss(ctx, "2001:db8:1:2::1", "/.env")

	if ok, err := g.Unban(ctx, "203.0.113.7"); !ok || err != nil {
		t.Fatalf("unban v4 = %v, %v", ok, err)
	}
	// Any address in the banned network, or the network as listed, works.
	if ok, err := g.Unban(ctx, "2001:db8:1:2::42"); !ok || err != nil {
		t.Fatalf("unban v6 = %v, %v", ok, err)
	}
	if blocked(g, "203.0.113.7") || blocked(g, "2001:db8:1:2::1") {
		t.Error("still blocked after unban")
	}
	if ok, _ := g.Unban(ctx, "203.0.113.7"); ok {
		t.Error("unbanning twice reported a lifted ban")
	}
	if _, err := g.Unban(ctx, "not-an-ip"); err == nil {
		t.Error("expected an error for a malformed client")
	}

	// Strikes were forgotten too, so the next ban starts from the base again.
	g.Miss(ctx, "203.0.113.7", "/.env")
	if left, _ := g.Blocked(ctx, "203.0.113.7"); left != 10*time.Minute {
		t.Errorf("ban after unban = %v, want 10m", left)
	}
}

func TestUnparseableClientIsIgnored(t *testing.T) {
	g, _ := newGuard(t, nil)
	g.Miss(ctx, "", "/.env")
	g.Miss(ctx, "garbage", "/.env")
	if bans, _ := g.Bans(ctx); len(bans) != 0 {
		t.Errorf("bans = %+v", bans)
	}
}
