// Package reqctx carries per-request state across the middleware chain.
package reqctx

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rivencove/nina/internal/router"
)

type ctxKey struct{}

// Info is the mutable per-request record. It is created once at the top of the
// chain and filled in as the request descends, so later middleware can key on
// what earlier middleware established (rate limiting on a JWT subject, logging
// on the resolved consumer).
//
// Exactly one goroutine touches an Info at a time, so it needs no locking.
type Info struct {
	RequestID string
	Start     time.Time
	ClientIP  string

	// Route identification, set by the dispatcher.
	OperationID string
	Backend     string
	Template    string // gateway path template, safe as a metric label
	Params      router.Params

	// Identity, set by auth middleware.
	Consumer string
	Subject  string
	Scopes   []string
	Claims   map[string]any

	// Upstream outcome, filled in by the proxy handler for the access log.
	UpstreamStatus  int
	UpstreamAttempt int
	UpstreamHost    string
	UpstreamLatency time.Duration
}

func With(ctx context.Context, i *Info) context.Context {
	return context.WithValue(ctx, ctxKey{}, i)
}

// From returns the request's Info, or nil.
func From(ctx context.Context) *Info {
	i, _ := ctx.Value(ctxKey{}).(*Info)
	return i
}

// ClientIP derives the caller's address. X-Forwarded-For is believed only when
// the immediate peer is a trusted proxy; otherwise any client could spoof its
// own identity and defeat per-IP rate limiting.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if len(trusted) == 0 {
		return peer
	}
	ip := net.ParseIP(peer)
	if ip == nil || !inAny(ip, trusted) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	// Walk right to left and take the first address that is not itself a trusted
	// proxy; that is the closest thing to the real client we can justify.
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		cand := net.ParseIP(strings.TrimSpace(parts[i]))
		if cand == nil {
			continue
		}
		if !inAny(cand, trusted) {
			return cand.String()
		}
	}
	return peer
}

func inAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseCIDRs parses trusted proxy networks, accepting bare IPs too.
func ParseCIDRs(in []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, s := range in {
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				return nil, &net.ParseError{Type: "IP address", Text: s}
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
