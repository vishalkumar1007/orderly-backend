package configsvc

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// smtpProvider talks SMTP over a resolved configuration.
type smtpProvider struct {
	cfg    SMTPConfig
	passwd string
}

func newSMTPProvider(r *Resolved) *smtpProvider {
	cfg := smtpFrom(r.Config)
	cfg.applyDefaults()
	return &smtpProvider{cfg: cfg, passwd: r.Secret("password")}
}

func (p *smtpProvider) Provider() string    { return ProviderSMTP }
func (p *smtpProvider) FromAddress() string { return p.cfg.FromEmail }

// TestConnection opens a session, authenticates when credentials were supplied,
// and quits. It deliberately sends no message.
func (p *smtpProvider) TestConnection(ctx context.Context) error {
	client, conn, err := p.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer client.Close()

	if err := p.authenticate(client); err != nil {
		return err
	}
	_ = client.Quit()
	return nil
}

// Send delivers one message.
func (p *smtpProvider) Send(ctx context.Context, msg MailMessage) error {
	if len(msg.To) == 0 {
		return fmt.Errorf("at least one recipient is required")
	}
	client, conn, err := p.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer client.Close()

	if err := p.authenticate(client); err != nil {
		return err
	}
	if err := client.Mail(p.cfg.FromEmail); err != nil {
		return fmt.Errorf("sender rejected: %w", err)
	}
	for _, to := range msg.To {
		if err := client.Rcpt(strings.TrimSpace(to)); err != nil {
			return fmt.Errorf("recipient %s rejected: %w", to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("could not begin message: %w", err)
	}
	if _, err := io.WriteString(w, buildMIME(p.cfg, msg)); err != nil {
		return fmt.Errorf("could not write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("message rejected: %w", err)
	}
	return client.Quit()
}

// dial establishes the transport and returns the client plus the underlying
// connection, so callers can bound the connection lifetime with the context.
func (p *smtpProvider) dial(ctx context.Context) (*smtp.Client, net.Conn, error) {
	addr := net.JoinHostPort(p.cfg.Host, strconv.Itoa(p.cfg.Port))
	timeout := dialTimeout(ctx)
	encryption, _ := ParseEncryption(p.cfg.Encryption)

	var conn net.Conn
	var err error

	if encryption == EncryptionTLS {
		// Implicit TLS: the handshake happens before the SMTP greeting.
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", addr, &tls.Config{
			ServerName: p.cfg.Host,
			MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("tls handshake with %s failed: %w", addr, err)
		}
	} else {
		conn, err = net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("connect to %s failed: %w", addr, err)
		}
	}

	// net/smtp has no context support, so the deadline is enforced on the
	// socket instead. Without this a hung server would pin the goroutine.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, p.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("smtp handshake with %s failed: %w", addr, err)
	}
	if encryption == EncryptionSTARTTLS {
		if err := client.StartTLS(&tls.Config{
			ServerName: p.cfg.Host,
			MinVersion: tls.VersionTLS12,
		}); err != nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("starttls failed: %w", err)
		}
	}
	return client, conn, nil
}

func (p *smtpProvider) authenticate(client *smtp.Client) error {
	if strings.TrimSpace(p.cfg.Username) == "" {
		return nil
	}
	// Refuse to attempt authentication over a cleartext link ourselves.
	//
	// net/smtp's Client.Auth discards the error returned by PlainAuth.Start, so
	// relying on it to block a plaintext login means depending on whatever the
	// server happens to reply. Enforcing the policy here guarantees the
	// credentials are never put on the wire in the clear.
	if p.cfg.Encryption == string(EncryptionNone) {
		return fmt.Errorf(
			"refusing to send credentials over an unencrypted connection: " +
				"set encryption to STARTTLS or TLS, or leave the username empty for an open relay")
	}
	auth := smtp.PlainAuth("", p.cfg.Username, p.passwd, p.cfg.Host)
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	return nil
}

func dialTimeout(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < 30*time.Second {
			return remaining
		}
	}
	return 15 * time.Second
}

// buildMIME renders a message. Headers are sanitised so a tenant-supplied
// display name cannot inject additional headers.
func buildMIME(cfg SMTPConfig, msg MailMessage) string {
	contentType := msg.ContentType
	if contentType == "" {
		contentType = "text/plain; charset=UTF-8"
	}
	from := cfg.FromEmail
	if cfg.FromName != "" {
		from = fmt.Sprintf("%s <%s>", sanitizeMailHeader(cfg.FromName), cfg.FromEmail)
	}
	replyTo := cfg.ReplyTo
	if msg.ReplyTo != "" {
		replyTo = msg.ReplyTo
	}

	var b strings.Builder
	b.WriteString("From: " + sanitizeMailHeader(from) + "\r\n")
	for _, to := range msg.To {
		b.WriteString("To: " + sanitizeMailHeader(strings.TrimSpace(to)) + "\r\n")
	}
	b.WriteString("Subject: " + sanitizeMailHeader(msg.Subject) + "\r\n")
	if replyTo != "" {
		b.WriteString("Reply-To: " + sanitizeMailHeader(replyTo) + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: " + sanitizeMailHeader(contentType) + "\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeNewlines(msg.Body))
	return b.String()
}

// sanitizeMailHeader makes a value safe to place in a header.
//
// Stripping only the newline characters would leave "Evil\r\nBcc: x" as
// "EvilBcc: x", which is harmless but produces a nonsense header. Dropping
// everything from the first line break instead removes the injected text
// entirely, which is both safer and legible.
func sanitizeMailHeader(v string) string {
	v = strings.ReplaceAll(v, "\x00", "")
	if idx := strings.IndexAny(v, "\r\n"); idx >= 0 {
		v = v[:idx]
	}
	return strings.TrimSpace(v)
}

func normalizeNewlines(v string) string {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	return strings.ReplaceAll(v, "\n", "\r\n")
}
