package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// limiter is a token bucket per client address. Idle buckets are swept
// every minute, and the map is capped: when it is full of active clients,
// new addresses are refused rather than letting memory grow.
type limiter struct {
	rate, burst float64
	maxKeys     int

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(rate, burst float64, maxKeys int) *limiter {
	return &limiter{rate: rate, burst: burst, maxKeys: maxKeys, buckets: make(map[string]*bucket), lastSweep: time.Now()}
}

func (l *limiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		// A bucket idle long enough to have refilled is the same as none.
		if now.Sub(b.at).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
	l.lastSweep = now
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute {
		l.sweepLocked(now)
	}
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= l.maxKeys {
			l.sweepLocked(now)
			if len(l.buckets) >= l.maxKeys {
				return false
			}
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *Server) clientAddr(r *http.Request) string {
	if s.cfg.ProxyHops > 0 {
		var hops []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			for part := range strings.SplitSeq(h, ",") {
				hops = append(hops, strings.TrimSpace(part))
			}
		}
		if i := len(hops) - s.cfg.ProxyHops; i >= 0 && i < len(hops) && hops[i] != "" {
			return hops[i]
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) limited(h http.HandlerFunc) http.HandlerFunc {
	if s.limiter == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(s.clientAddr(r), time.Now()) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		h(w, r)
	}
}
