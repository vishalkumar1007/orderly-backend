package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListenURL(t *testing.T) {
	cases := map[string]string{
		"":              "http://127.0.0.1:8080/health/ready",
		":8080":         "http://127.0.0.1:8080/health/ready",
		":9000":         "http://127.0.0.1:9000/health/ready",
		"0.0.0.0:8081":  "http://127.0.0.1:8081/health/ready",
		"[::]:8082":     "http://127.0.0.1:8082/health/ready",
		"10.1.2.3:7000": "http://10.1.2.3:7000/health/ready",
		"garbage":       "http://127.0.0.1:8080/health/ready",
	}
	for in, want := range cases {
		if got := listenURL(in); got != want {
			t.Errorf("listenURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbeReportsHealthyOnlyOn200(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()

	var out bytes.Buffer
	if code := probe(srv.URL, time.Second, &out); code != 0 {
		t.Errorf("200 → exit %d (%s), want 0", code, out.String())
	}
	status = http.StatusServiceUnavailable
	out.Reset()
	if code := probe(srv.URL, time.Second, &out); code != 1 || !strings.Contains(out.String(), "503") {
		t.Errorf("503 → exit %d (%q), want 1 naming the status", code, out.String())
	}
}

func TestProbeFailsWhenNothingIsListening(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing answers any more
	var out bytes.Buffer
	if code := probe(url, 500*time.Millisecond, &out); code != 1 || !strings.Contains(out.String(), "unhealthy") {
		t.Errorf("a dead API → exit %d (%q), want 1", code, out.String())
	}
}
