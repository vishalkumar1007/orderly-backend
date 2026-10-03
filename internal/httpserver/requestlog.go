package httpserver

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// requestLogger logs one structured line per HTTP request (access log).
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
			"host", r.Host,
			"remote", r.RemoteAddr,
		}
		if q := r.URL.RawQuery; q != "" {
			attrs = append(attrs, "query", q)
		}

		switch {
		case status >= 500:
			s.log.Error("request", attrs...)
		case status >= 400:
			s.log.Warn("request", attrs...)
		default:
			s.log.Info("request", attrs...)
		}
	})
}
