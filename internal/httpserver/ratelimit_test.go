package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestLimiterCountsPerClientAndResetsEachWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := newLimiter(3, time.Minute)
	l.now = func() time.Time { return now }

	for i := 1; i <= 3; i++ {
		if ok, _ := l.allow("1.1.1.1"); !ok {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	ok, wait := l.allow("1.1.1.1")
	if ok || wait <= 0 || wait > time.Minute {
		t.Fatalf("4th request: ok=%v wait=%v, want blocked with a retry time inside the window", ok, wait)
	}
	// Another client is unaffected.
	if ok, _ := l.allow("2.2.2.2"); !ok {
		t.Error("one client's burst must not throttle another")
	}
	// A new window starts fresh.
	now = now.Add(61 * time.Second)
	if ok, _ := l.allow("1.1.1.1"); !ok {
		t.Error("the limit should reset after the window")
	}
}

func TestLimiterForgetsFinishedWindows(t *testing.T) {
	now := time.Now()
	l := newLimiter(1, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 500; i++ {
		l.allow("client-" + strconv.Itoa(i))
	}
	now = now.Add(3 * time.Minute)
	l.allow("someone-new")
	if n := len(l.buckets); n != 1 {
		t.Errorf("buckets = %d after the windows ended, want 1: the map must not grow forever", n)
	}
}

func TestLimiterMiddleware(t *testing.T) {
	hits := 0
	h := newLimiter(2, time.Minute).middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "9.9.9.9:5555"
		h.ServeHTTP(rec, req)
		if i < 2 && rec.Code != http.StatusOK {
			t.Errorf("request %d: %d, want 200", i, rec.Code)
		}
		if i >= 2 {
			if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
				t.Errorf("request %d: %d retry-after=%q, want 429 with Retry-After", i, rec.Code, rec.Header().Get("Retry-After"))
			}
		}
	}
	if hits != 2 {
		t.Errorf("handler ran %d times, want 2: a blocked request must never reach it", hits)
	}
	// A nil limiter is a pass-through, which is how "off" works.
	var off *limiter
	called := false
	off.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("a nil limiter must let requests through")
	}
}
