// Package netutil reads who is calling: for login history and session lists.
package netutil

import (
	"context"
	"net"
	"net/http"
)

// ClientIP is the caller's address without the port. chi's RealIP middleware
// has already swapped in X-Forwarded-For / X-Real-IP when present.
func ClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// UserAgent is the request's User-Agent, capped so a hostile header cannot
// bloat the log tables.
func UserAgent(r *http.Request) string {
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	return ua
}

type clientKey struct{}

// Client is who sent the request, carried in the context so code without the
// *http.Request (token issuing, audit writes) can still record it.
type Client struct {
	IP        string
	UserAgent string
}

// Middleware stores the caller's Client on the request context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := Client{IP: ClientIP(r), UserAgent: UserAgent(r)}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, c)))
	})
}

// ClientFrom returns the Client stored by Middleware, or a zero Client.
func ClientFrom(ctx context.Context) Client {
	c, _ := ctx.Value(clientKey{}).(Client)
	return c
}
