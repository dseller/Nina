// Package scanguard bans clients that probe for routes the gateway does not
// serve.
//
// Every route Nina serves comes from the merged spec, so a request for an
// unknown path is a request for something the published API says does not
// exist. A real client makes that mistake a handful of times; a scanner makes
// it hundreds of times a minute. The guard counts those misses per client and
// bans a client that crosses the threshold, or that asks for a path only a
// scanner would ask for.
package scanguard

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rivencove/nina/internal/config"
)

// Reasons a client was banned.
const (
	ReasonTrap      = "trap"
	ReasonThreshold = "threshold"
)

// Ban describes one banned client.
type Ban struct {
	Client  string    `json:"client"`
	Reason  string    `json:"reason"`
	Path    string    `json:"path"`
	Strikes int       `json:"strikes"`
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
}

// Store keeps miss counters and bans. Keys are already-normalised clients.
type Store interface {
	// BannedFor reports how much longer key is banned; zero means not banned.
	BannedFor(ctx context.Context, key string) (time.Duration, error)
	// Miss records one miss and returns the count in the current window.
	Miss(ctx context.Context, key string, window time.Duration) (int, error)
	// Ban bans key and clears its misses. The length doubles with each strike
	// from base, capped at max; strikes are forgotten max after the last ban
	// ends.
	// It returns the ban as stored.
	Ban(ctx context.Context, b Ban, base, max time.Duration) (Ban, error)
	// Unban lifts a ban and forgets the client's strikes.
	Unban(ctx context.Context, key string) (bool, error)
	// List returns the bans in force.
	List(ctx context.Context) ([]Ban, error)
}

// banLength is base doubled once per strike after the first, capped at max.
func banLength(strikes int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < strikes && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// Guard applies one configuration to a store. It is immutable once built; a
// reload builds a new one over the same store.
type Guard struct {
	store     Store
	maxMisses int
	window    time.Duration
	ban       time.Duration
	maxBan    time.Duration
	traps     *regexp.Regexp // nil when traps are off
	allow     []*net.IPNet
	v6Mask    net.IPMask
	log       *slog.Logger

	// lastErr throttles store error logging to one line a minute, so a dead
	// Redis does not add a log line to every request.
	lastErr atomic.Int64
}

// New builds a guard from configuration that has already been validated.
func New(cfg *config.ScanGuard, store Store, log *slog.Logger) (*Guard, error) {
	g := &Guard{
		store:     store,
		maxMisses: cfg.MaxMisses,
		window:    cfg.Window.Std(),
		ban:       cfg.Ban.Std(),
		maxBan:    cfg.MaxBan.Std(),
		v6Mask:    net.CIDRMask(cfg.IPv6Prefix, 128),
		log:       log,
	}
	if len(cfg.TrapPaths) > 0 {
		re, err := compileTraps(cfg.TrapPaths)
		if err != nil {
			return nil, err
		}
		g.traps = re
	}
	for _, s := range cfg.AllowCIDRs {
		if !strings.Contains(s, "/") {
			if strings.Contains(s, ":") {
				s += "/128"
			} else {
				s += "/32"
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("allow_cidrs: %w", err)
		}
		g.allow = append(g.allow, n)
	}
	return g, nil
}

// compileTraps turns the glob list into one case-insensitive, anchored regexp.
// `*` matches any run of characters, including `/`.
func compileTraps(globs []string) (*regexp.Regexp, error) {
	alts := make([]string, len(globs))
	for i, g := range globs {
		parts := strings.Split(g, "*")
		for j, p := range parts {
			parts[j] = regexp.QuoteMeta(p)
		}
		alts[i] = strings.Join(parts, ".*")
	}
	return regexp.Compile(`(?i)^(?:` + strings.Join(alts, "|") + `)$`)
}

// Key normalises a client IP to the unit the guard bans. It returns "" for a
// client the guard ignores: an allowlisted address, or one it cannot parse.
func (g *Guard) Key(clientIP string) string {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return ""
	}
	for _, n := range g.allow {
		if n.Contains(ip) {
			return ""
		}
	}
	return g.normalise(ip)
}

// Blocked reports whether the client is banned and for how much longer. A
// store failure lets the request through: a ban list is not worth an outage.
func (g *Guard) Blocked(ctx context.Context, clientIP string) (time.Duration, bool) {
	key := g.Key(clientIP)
	if key == "" {
		return 0, false
	}
	d, err := g.store.BannedFor(ctx, key)
	if err != nil {
		g.storeError(err)
		return 0, false
	}
	return d, d > 0
}

// Miss records a request for a path the gateway does not serve.
func (g *Guard) Miss(ctx context.Context, clientIP, path string) {
	key := g.Key(clientIP)
	if key == "" {
		return
	}
	reason := ""
	if g.traps != nil && g.traps.MatchString(path) {
		reason = ReasonTrap
	} else {
		n, err := g.store.Miss(ctx, key, g.window)
		if err != nil {
			g.storeError(err)
			return
		}
		if n < g.maxMisses {
			return
		}
		reason = ReasonThreshold
	}
	b, err := g.store.Ban(ctx, Ban{Client: key, Reason: reason, Path: path}, g.ban, g.maxBan)
	if err != nil {
		g.storeError(err)
		return
	}
	// One line per ban rather than one per blocked request: this is the event
	// an operator wants to see.
	g.log.Warn("scan guard banned client",
		"client", key, "reason", reason, "path", path,
		"strikes", b.Strikes, "ban", b.Until.Sub(b.Since).Round(time.Second))
}

// Bans lists the bans in force.
func (g *Guard) Bans(ctx context.Context) ([]Ban, error) { return g.store.List(ctx) }

// Unban lifts the ban on a client given as an IP or, for IPv6, in the
// network form that Bans lists.
func (g *Guard) Unban(ctx context.Context, client string) (bool, error) {
	key := client
	if ip, _, err := net.ParseCIDR(client); err == nil {
		key = g.normalise(ip)
	} else if ip := net.ParseIP(client); ip != nil {
		key = g.normalise(ip)
	} else {
		return false, fmt.Errorf("%q is not an IP address", client)
	}
	return g.store.Unban(ctx, key)
}

// normalise is Key without the allowlist, so an address allowlisted after it
// was banned can still be unbanned.
func (g *Guard) normalise(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	ones, _ := g.v6Mask.Size()
	return fmt.Sprintf("%s/%d", ip.Mask(g.v6Mask), ones)
}

func (g *Guard) storeError(err error) {
	now := time.Now().UnixNano()
	last := g.lastErr.Load()
	if now-last < int64(time.Minute) || !g.lastErr.CompareAndSwap(last, now) {
		return
	}
	g.log.Error("scan guard store failed; letting requests through", "error", err)
}
