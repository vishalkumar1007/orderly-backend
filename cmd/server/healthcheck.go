package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// runHealthcheck implements `server healthcheck`, the container's HEALTHCHECK.
//
// The runtime image is distroless — no shell, no curl — so the binary probes
// itself: GET /health/ready on the loopback address it listens on. Exit 0 when
// the API answers 200, 1 otherwise, so Docker marks the container unhealthy
// when the API (or its database) is down.
func runHealthcheck() int {
	return probe(listenURL(os.Getenv("HTTP_ADDR")), 3*time.Second, os.Stderr)
}

// listenURL turns a listen address (":8080", "0.0.0.0:9000", "") into the
// loopback URL to probe.
func listenURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		host, port = "", "8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/health/ready"
}

func probe(url string, timeout time.Duration, out io.Writer) int {
	resp, err := (&http.Client{Timeout: timeout}).Get(url)
	if err != nil {
		fmt.Fprintf(out, "unhealthy: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(out, "unhealthy: %s answered %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
