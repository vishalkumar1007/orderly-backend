package configsvc

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP is a minimal SMTP server for exercising the provider. It speaks just
// enough of the protocol to complete a session, and records the message body so
// a test can assert on the rendered MIME.
type fakeSMTP struct {
	ln       net.Listener
	host     string
	port     int
	mu       sync.Mutex
	captured string
}

// startFakeSMTP starts the server and registers cleanup.
func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	s := &fakeSMTP{ln: ln, host: "127.0.0.1", port: addr.Port}
	go s.serve()
	return s
}

func (s *fakeSMTP) close() { _ = s.ln.Close() }

func (s *fakeSMTP) message() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captured
}

func (s *fakeSMTP) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	write := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\r\n", args...)
		_ = w.Flush()
	}

	write("220 fake.smtp ESMTP ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			write("250-fake.smtp greets you")
			// Deliberately no AUTH advertised: this server is for unauthenticated
			// sessions, and the transport-security tests rely on that.
			write("250 8BITMIME")
		case strings.HasPrefix(cmd, "AUTH"):
			// This server never advertises AUTH, so a well-behaved client never
			// sends it. Rejecting loudly means a test that reaches this branch
			// has found a real credential leak rather than passing silently.
			write("503 5.5.1 Authentication not enabled")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			write("250 2.1.0 Ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			write("250 2.1.5 Ok")
		case strings.HasPrefix(cmd, "DATA"):
			write("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				dataLine, rerr := r.ReadString('\n')
				if rerr != nil {
					return
				}
				if strings.TrimRight(dataLine, "\r\n") == "." {
					break
				}
				body.WriteString(dataLine)
			}
			s.mu.Lock()
			s.captured = body.String()
			s.mu.Unlock()
			write("250 2.0.0 Ok: queued")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 2.0.0 Bye")
			return
		case strings.HasPrefix(cmd, "STARTTLS"):
			write("454 4.7.0 TLS not available")
		default:
			write("250 2.0.0 Ok")
		}
	}
}
