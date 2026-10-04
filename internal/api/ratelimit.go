package api

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	loginMaxFailures = 5
	loginWindow      = 15 * time.Minute
	loginLockout     = 15 * time.Minute
	// loginMaxTracked bounds the limiter's memory under a flood of distinct
	// source addresses; past it, expired entries are pruned on every write.
	loginMaxTracked = 10_000
)

// loginLimiter locks a client address out of /login after loginMaxFailures
// failed attempts within loginWindow, for loginLockout. Keyed on the TCP peer
// address (X-Forwarded-For is spoofable), so behind a reverse proxy the limit
// applies to the proxy as a whole.
type loginLimiter struct {
	mu      sync.Mutex
	clients map[string]*loginState
	now     func() time.Time
}

type loginState struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{clients: make(map[string]*loginState), now: time.Now}
}

// allow reports whether ip may attempt a login now; if not, it also returns
// how long until it may.
func (l *loginLimiter) allow(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.clients[ip]
	if !ok {
		return 0, true
	}
	if wait := st.lockedUntil.Sub(l.now()); wait > 0 {
		return wait, false
	}
	return 0, true
}

// fail records a failed attempt from ip, locking it out once it reaches
// loginMaxFailures within the window.
func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.clients) >= loginMaxTracked {
		l.prune(now)
	}
	st, ok := l.clients[ip]
	if !ok || now.Sub(st.windowStart) > loginWindow {
		st = &loginState{windowStart: now}
		l.clients[ip] = st
	}
	st.failures++
	if st.failures >= loginMaxFailures {
		st.lockedUntil = now.Add(loginLockout)
		st.failures = 0
		st.windowStart = now
	}
}

// success forgets ip's failures.
func (l *loginLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, ip)
}

func (l *loginLimiter) prune(now time.Time) {
	for ip, st := range l.clients {
		if now.After(st.lockedUntil) && now.Sub(st.windowStart) > loginWindow {
			delete(l.clients, ip)
		}
	}
}

// clientIP returns the request's TCP peer address without the port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
