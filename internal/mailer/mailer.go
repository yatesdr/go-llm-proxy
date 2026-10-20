// Package mailer sends plain-text email through an SMTP relay (stdlib
// net/smtp). With STARTTLS configured, a server that doesn't offer it is an
// error, never a plaintext fallback.
package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds one whole delivery: connect, TLS, auth and DATA.
const DefaultTimeout = 15 * time.Second

// Settings describe the relay.
type Settings struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string // "Name <address>" or a bare address
	ReplyTo  string // optional
	STARTTLS bool
}

// Message is one plain-text email to one recipient.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Sender delivers messages. *SMTP is the real one; tests use a recorder.
type Sender interface {
	Send(ctx context.Context, s Settings, m Message) error
}

// ErrNotConfigured means the relay host or From address is missing.
var ErrNotConfigured = errors.New("mailer: SMTP is not configured")

// SMTP sends through an SMTP relay. The zero value is ready to use.
type SMTP struct {
	Timeout   time.Duration // default DefaultTimeout
	TLSConfig *tls.Config   // optional base TLS config (tests set RootCAs)
}

// Send delivers m through the relay in s.
func (c *SMTP) Send(ctx context.Context, s Settings, m Message) error {
	if s.Host == "" || s.From == "" {
		return ErrNotConfigured
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("mailer: bad From %q: %w", s.From, err)
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("mailer: bad recipient: %w", err)
	}
	body, err := build(s, from, to.Address, m)
	if err != nil {
		return err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	port := s.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mailer: dial %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mailer: greeting: %w", err)
	}
	defer client.Close()
	if err := client.Hello(heloName(from.Address)); err != nil {
		return fmt.Errorf("mailer: EHLO: %w", err)
	}
	if s.STARTTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("mailer: server does not offer STARTTLS; refusing to send in plaintext")
		}
		tc := &tls.Config{}
		if c.TLSConfig != nil {
			tc = c.TLSConfig.Clone()
		}
		tc.ServerName = s.Host
		if tc.MinVersion < tls.VersionTLS12 {
			tc.MinVersion = tls.VersionTLS12
		}
		if err := client.StartTLS(tc); err != nil {
			return fmt.Errorf("mailer: STARTTLS: %w", err)
		}
	}
	if s.Username != "" {
		// smtp.PlainAuth refuses to send credentials over an unencrypted
		// connection to anything but localhost.
		if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return fmt.Errorf("mailer: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to.Address); err != nil {
		return fmt.Errorf("mailer: RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		w.Close()
		return fmt.Errorf("mailer: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: end DATA: %w", err)
	}
	return client.Quit()
}

// build renders the RFC 5322 headers and a quoted-printable text body.
func build(s Settings, from *mail.Address, to string, m Message) ([]byte, error) {
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", from.String())
	h("To", to)
	if s.ReplyTo != "" {
		if a, err := mail.ParseAddress(s.ReplyTo); err == nil {
			h("Reply-To", a.String())
		}
	}
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", "<"+randomID()+"@"+domainOf(from.Address)+">")
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")
	h("Content-Type", `text/plain; charset="utf-8"`)
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	text := strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(text)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func randomID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}

// heloName is the sender address's domain: servers that require a
// fully-qualified HELO refuse Go's default "localhost".
func heloName(address string) string {
	if i := strings.LastIndexByte(address, '@'); i >= 0 && strings.Contains(address[i+1:], ".") {
		return strings.ToLower(address[i+1:])
	}
	return "localhost"
}
