package httpserver

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/orderly/orderly-backend/pkg/response"
)

// limiter is a fixed-window request counter per client address. Deliberately
// tiny: it is in memory, so each API process counts on its own.
//
// ponytail: per-process counters. If the API is ever scaled to several
// instances behind one address, move the counts to the database or a shared
// store so the limit is global.
type limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	count int
	reset time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, now: time.Now, buckets: map[string]*bucket{}}
}

// allow records one request from key. It reports whether it is within the
// limit and, when not, how long until the window resets.
func (l *limiter) allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.swept) > l.window {
		// Forget finished windows so the map cannot grow without bound.
		for k, b := range l.buckets {
			if now.After(b.reset) {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b := l.buckets[key]
	if b == nil || now.After(b.reset) {
		b = &bucket{reset: now.Add(l.window)}
		l.buckets[key] = b
	}
	b.count++
	if b.count > l.max {
		return false, b.reset.Sub(now)
	}
	return true, 0
}

// middleware answers 429 once a client address exceeds the limit. A nil limiter
// is a pass-through, so callers need no branching when limits are off.
func (l *limiter) middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ok, wait := l.allow(host); !ok {
			secs := int(wait.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			response.Error(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please wait a moment and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// payLimits are the throttles on the money routes. The numbers are per client
// address per minute: generous for a real customer (one checkout is a handful of
// calls, polling is one per few seconds) and tight against scripts.
type payLimits struct {
	start, confirm, status, webhook *limiter
}

func newPayLimits(enabled bool) payLimits {
	if !enabled {
		return payLimits{}
	}
	return payLimits{
		start:   newLimiter(20, time.Minute),
		confirm: newLimiter(30, time.Minute),
		status:  newLimiter(120, time.Minute),
		// Gateways deliver from a few addresses and retry in bursts.
		webhook: newLimiter(600, time.Minute),
	}
}

// wrap adapts a limiter to chi's With().
func wrap(l *limiter) func(http.Handler) http.Handler { return l.middleware }
