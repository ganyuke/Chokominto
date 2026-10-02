package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Limiter counts failures per key (client IP) in a sliding window.
type Limiter struct {
	Max    int
	Window time.Duration

	mu    sync.Mutex
	fails map[string][]time.Time
	now   func() time.Time
}

func NewLoginLimiter() *Limiter {
	return &Limiter{Max: 5, Window: 15 * time.Minute, fails: map[string][]time.Time{}, now: time.Now}
}

func (l *Limiter) prune(key string, t time.Time) []time.Time {
	fs := l.fails[key]
	i := 0
	for i < len(fs) && t.Sub(fs[i]) >= l.Window {
		i++
	}
	fs = fs[i:]
	if len(fs) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = fs
	}
	return fs
}

// Allowed reports whether key may try again.
func (l *Limiter) Allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, l.now())) < l.Max
}

func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	l.fails[key] = append(l.prune(key, t), t)
	// Keep the map from growing without bound under a spray of addresses.
	if len(l.fails) > 10000 {
		for k := range l.fails {
			l.prune(k, t)
		}
	}
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// ClientIP returns the client address. X-Forwarded-For is only believed
// when the connection comes from a trusted proxy, and then the rightmost
// address that isn't itself a trusted proxy is used.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if !isTrusted(peer, trusted) {
		return peer.String()
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !isTrusted(a, trusted) {
			return a.String()
		}
	}
	return peer.String()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
