package chclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/config"
)

func TestOptionsFrom(t *testing.T) {
	r := &config.Resolved{Host: "ch.example.com", Port: 9440, Database: "db", Protocol: config.ProtocolNative, Secure: true}
	o := OptionsFrom(r, "1.2.3")
	if o.Addr() != "ch.example.com:9440" || o.Database != "db" || !o.Secure || o.ClientVersion != "1.2.3" {
		t.Errorf("options = %+v", o)
	}
	if got := (Options{Host: "::1", Port: 9000}).Addr(); got != "[::1]:9000" {
		t.Errorf("IPv6 addr = %q", got)
	}
}

func TestTLSConfig(t *testing.T) {
	if cfg, err := (Options{Host: "h"}).TLSConfig(); cfg != nil || err != nil {
		t.Errorf("plaintext connection must have no TLS config: %v, %v", cfg, err)
	}

	// Certificates are verified unless the user explicitly opts out.
	cfg, err := Options{Host: "ch.example.com", Secure: true}.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InsecureSkipVerify || cfg.ServerName != "ch.example.com" || cfg.MinVersion < tls.VersionTLS12 || cfg.RootCAs != nil {
		t.Errorf("default TLS config = %+v", cfg)
	}
	cfg, _ = Options{Host: "h", Secure: true, InsecureSkipVerify: true}.TLSConfig()
	if !cfg.InsecureSkipVerify {
		t.Error("explicit opt-out was ignored")
	}

	if _, err := (Options{Host: "h", Secure: true, CACert: filepath.Join(t.TempDir(), "missing.pem")}).TLSConfig(); err == nil {
		t.Error("a missing CA file must be an error")
	}
	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Options{Host: "h", Secure: true, CACert: notPEM}).TLSConfig(); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Errorf("error = %v", err)
	}
}

func TestErrorClassification(t *testing.T) {
	c := New(Options{Host: "h", Port: 9000}, nil)
	ctx := context.Background()
	var (
		authErr *AuthError
		connErr *ConnError
	)

	for _, code := range []int32{192, 193, 194, 516} {
		if err := c.wrap(ctx, &clickhouse.Exception{Code: code, Message: "nope"}); !errors.As(err, &authErr) {
			t.Errorf("exception %d should be an AuthError, got %T", code, err)
		}
	}
	queryErr := c.wrap(ctx, &clickhouse.Exception{Code: 60, Message: "Unknown table"})
	if errors.As(queryErr, &authErr) || errors.As(queryErr, &connErr) {
		t.Errorf("an ordinary exception must pass through, got %T", queryErr)
	}

	for _, err := range []error{
		io.EOF, io.ErrUnexpectedEOF,
		fmt.Errorf("read: %w", io.EOF),
		&net.OpError{Op: "dial", Err: errors.New("connection refused")},
		clickhouse.ErrAcquireConnTimeout,
	} {
		wrapped := c.wrap(ctx, err)
		if !errors.As(wrapped, &connErr) {
			t.Errorf("%v should be a ConnError, got %T", err, wrapped)
		} else if !strings.Contains(wrapped.Error(), "h:9000") {
			t.Errorf("connection error should name the server: %v", wrapped)
		}
	}

	if err := c.wrap(ctx, context.Canceled); !errors.Is(err, context.Canceled) || errors.As(err, &connErr) {
		t.Errorf("cancellation must stay recognisable, got %v", err)
	}
	tlsErr := c.wrap(ctx, &tls.CertificateVerificationError{Err: errors.New("unknown authority")})
	if !errors.As(tlsErr, &connErr) || !strings.Contains(tlsErr.Error(), "--ca-cert") {
		t.Errorf("TLS verification failure should explain the options: %v", tlsErr)
	}
	if c.wrap(ctx, nil) != nil {
		t.Error("wrap(nil) must be nil")
	}

	// HTTP errors carry the request URL, which includes the user name and
	// query parameters. Only the cause is reported.
	httpErr := c.wrap(ctx, &url.Error{Op: "Post", URL: "http://default:hunter2@h:8123?database=default",
		Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}})
	if !errors.As(httpErr, &connErr) || strings.Contains(httpErr.Error(), "hunter2") || strings.Contains(httpErr.Error(), "database=") {
		t.Errorf("HTTP error = %v", httpErr)
	}

	// Once the caller's context is cancelled, any driver error is reported
	// as the cancellation it results from, whatever the transport calls it.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{io.EOF, errors.New("interrupt signal received"), &net.OpError{Op: "read", Err: errors.New("use of closed network connection")}} {
		if got := c.wrap(cancelled, err); !errors.Is(got, context.Canceled) {
			t.Errorf("wrap(cancelled ctx, %v) = %v, want context.Canceled", err, got)
		}
	}
}

type recordingTransport struct{ got *http.Request }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func TestBearerTransport(t *testing.T) {
	rec := &recordingTransport{}
	original, _ := http.NewRequest(http.MethodPost, "http://ch.example.com:8123/", nil)
	if _, err := (bearerTransport{token: "tok-123", next: rec}).RoundTrip(original); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Errorf("Authorization = %q", got)
	}
	if original.Header.Get("Authorization") != "" {
		t.Error("the caller's request must not be modified")
	}
}

// rotatingProvider hands out whatever token it currently holds.
type rotatingProvider struct{ token string }

func (p *rotatingProvider) Authenticate(context.Context) (*auth.Credentials, error) {
	if p.token == "" {
		return nil, &auth.LoginRequiredError{Profile: "production"}
	}
	return &auth.Credentials{Token: p.token}, nil
}

// When the provider refreshes the token, the next statement must run on a
// connection opened with the new one. (The driver connects lazily, so this
// needs no server.)
func TestConnectionIsRebuiltWhenTokenChanges(t *testing.T) {
	p := &rotatingProvider{token: "token-1"}
	c := New(Options{Host: "127.0.0.1", Port: 9440, Database: "default", Protocol: config.ProtocolNative, Secure: true}, p)
	defer c.Close()

	first, err := c.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := c.acquire(context.Background()); again != first {
		t.Error("an unchanged token must reuse the connection")
	}

	p.token = "token-2"
	second, err := c.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Error("a new token must open a new connection")
	}
	if c.token != "token-2" {
		t.Errorf("client still holds %q", c.token)
	}

	// Credentials that cannot be obtained surface as an authentication error.
	p.token = ""
	var authErr *AuthError
	var loginErr *auth.LoginRequiredError
	if _, err := c.acquire(context.Background()); !errors.As(err, &authErr) || !errors.As(err, &loginErr) {
		t.Errorf("error = %v (%T), want AuthError wrapping LoginRequiredError", err, err)
	}
}
