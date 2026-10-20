package mailer

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a minimal SMTP server: EHLO, optional STARTTLS, AUTH PLAIN,
// MAIL, RCPT, DATA, QUIT. It records what it was sent.
type fakeSMTP struct {
	ln       net.Listener
	tlsCfg   *tls.Config // nil = STARTTLS not offered
	mu       sync.Mutex
	auth     string // decoded AUTH PLAIN payload
	from, to string
	data     string
	usedTLS  bool
}

func startFake(t *testing.T, tlsCfg *tls.Config) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, tlsCfg: tlsCfg}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	write := func(s string) { c.Write([]byte(s + "\r\n")) }
	write("220 fake ESMTP")
	secure := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			if f.tlsCfg != nil && !secure {
				write("250-fake")
				write("250-STARTTLS")
				write("250 AUTH PLAIN")
			} else {
				write("250-fake")
				write("250 AUTH PLAIN")
			}
		case "STARTTLS":
			write("220 go ahead")
			tc := tls.Server(c, f.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, r, secure = tc, bufio.NewReader(tc), true
			f.mu.Lock()
			f.usedTLS = true
			f.mu.Unlock()
		case "AUTH":
			parts := strings.Fields(line)
			if len(parts) == 3 {
				dec, _ := base64.StdEncoding.DecodeString(parts[2])
				f.mu.Lock()
				f.auth = string(dec)
				f.mu.Unlock()
			}
			write("235 ok")
		case "MAIL":
			f.mu.Lock()
			f.from = line
			f.mu.Unlock()
			write("250 ok")
		case "RCPT":
			f.mu.Lock()
			f.to = line
			f.mu.Unlock()
			write("250 ok")
		case "DATA":
			write("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			write("250 queued")
		case "QUIT":
			write("221 bye")
			return
		default:
			write("502 unknown")
		}
	}
}

func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake smtp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

func TestSendSTARTTLSAndAuth(t *testing.T) {
	srvTLS, pool := selfSigned(t)
	f := startFake(t, srvTLS)
	m := &SMTP{TLSConfig: &tls.Config{RootCAs: pool}}
	s := Settings{Host: "127.0.0.1", Port: f.port(), Username: "sender", Password: "pw",
		From: "LLM <no-reply@example.com>", ReplyTo: "support@example.com", STARTTLS: true}
	err := m.Send(context.Background(), s, Message{To: "user@example.com", Subject: "Confirm your email",
		Text: "Open this link:\nhttps://llm.example.com/join/confirm/abc"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.usedTLS {
		t.Error("STARTTLS was not used")
	}
	if f.auth != "\x00sender\x00pw" {
		t.Errorf("auth = %q", f.auth)
	}
	if !strings.Contains(f.from, "<no-reply@example.com>") || !strings.Contains(f.to, "<user@example.com>") {
		t.Errorf("envelope from=%q to=%q", f.from, f.to)
	}
	for _, want := range []string{"From: \"LLM\" <no-reply@example.com>", "To: user@example.com",
		"Reply-To: <support@example.com>", "Subject: Confirm your email", "text/plain",
		"https://llm.example.com/join/confirm/abc"} {
		if !strings.Contains(f.data, want) {
			t.Errorf("message missing %q:\n%s", want, f.data)
		}
	}
}

func TestSendRefusesPlaintextWhenSTARTTLSRequired(t *testing.T) {
	f := startFake(t, nil) // no STARTTLS offered
	err := (&SMTP{}).Send(context.Background(), Settings{Host: "127.0.0.1", Port: f.port(),
		From: "no-reply@example.com", STARTTLS: true}, Message{To: "user@example.com", Subject: "x", Text: "y"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("expected STARTTLS refusal, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data != "" {
		t.Error("message was sent in plaintext")
	}
}

func TestSendPlainWhenSTARTTLSOff(t *testing.T) {
	f := startFake(t, nil)
	err := (&SMTP{}).Send(context.Background(), Settings{Host: "127.0.0.1", Port: f.port(),
		From: "no-reply@example.com"}, Message{To: "user@example.com", Subject: "x", Text: "hello"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.data, "hello") {
		t.Errorf("body missing: %q", f.data)
	}
}

func TestSendNotConfigured(t *testing.T) {
	if err := (&SMTP{}).Send(context.Background(), Settings{}, Message{To: "a@example.com"}); err != ErrNotConfigured {
		t.Fatalf("got %v", err)
	}
}

func TestSendTimesOut(t *testing.T) {
	// A server that accepts and never greets.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	start := time.Now()
	err = (&SMTP{Timeout: 200 * time.Millisecond}).Send(context.Background(),
		Settings{Host: "127.0.0.1", Port: port, From: "no-reply@example.com"},
		Message{To: "user@example.com", Subject: "x", Text: "y"})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout not honoured (%s)", time.Since(start))
	}
}
