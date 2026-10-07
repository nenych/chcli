package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/nenych/chcli/internal/config"
)

func resolved(t *testing.T, profile string, a config.Auth) *config.Resolved {
	t.Helper()
	return &config.Resolved{Profile: profile, Host: "h", Auth: a}
}

func TestNewProviderSelection(t *testing.T) {
	oidc := config.Auth{ClientID: "c", ClientSecret: "s", Issuer: "https://idp.example.com", Audience: "clickhouse",
		Scopes: []string{"openid"}, Flow: config.FlowBrowser, TokenType: config.TokenTypeAccess}

	t.Run("password", func(t *testing.T) {
		p, err := NewProvider(resolved(t, "", config.Auth{Type: config.AuthPassword, Username: "u", Password: "pw"}), Options{})
		if err != nil {
			t.Fatal(err)
		}
		creds, err := p.Authenticate(context.Background())
		if err != nil || creds.Username != "u" || creds.Password != "pw" || creds.UsesToken() || creds.Identity != "u" {
			t.Errorf("creds = %+v, err = %v", creds, err)
		}
		if _, ok := p.(SessionProvider); ok {
			t.Error("password auth has no login session")
		}
	})

	t.Run("jwt", func(t *testing.T) {
		p, err := NewProvider(resolved(t, "", config.Auth{Type: config.AuthJWT, Token: "opaque-token"}), Options{})
		if err != nil {
			t.Fatal(err)
		}
		creds, err := p.Authenticate(context.Background())
		if err != nil || creds.Token != "opaque-token" || creds.Username != "" {
			t.Errorf("creds = %+v, err = %v", creds, err)
		}
	})

	t.Run("oidc", func(t *testing.T) {
		a := oidc
		a.Type = config.AuthOIDC
		p, err := NewProvider(resolved(t, "staging", a), Options{Store: newMemStore()})
		if err != nil {
			t.Fatal(err)
		}
		op, ok := p.(*OIDCProvider)
		if !ok {
			t.Fatalf("provider is %T", p)
		}
		if op.cfg.Label != "OIDC" || op.cfg.Audience != "clickhouse" || len(op.cfg.AuthParams) != 0 ||
			op.cfg.CacheKey != "profile:staging" || op.cfg.ClientSecret != "s" {
			t.Errorf("cfg = %+v", op.cfg)
		}
	})

	// Google is the generic provider plus a preset, not a separate implementation.
	t.Run("google", func(t *testing.T) {
		a := oidc
		a.Type = config.AuthGoogle
		p, err := NewProvider(resolved(t, "production", a), Options{Store: newMemStore()})
		if err != nil {
			t.Fatal(err)
		}
		op, ok := p.(*OIDCProvider)
		if !ok {
			t.Fatalf("provider is %T", p)
		}
		if op.cfg.Label != "Google OAuth" || op.cfg.AuthParams["access_type"] != "offline" || op.cfg.Audience != "" {
			t.Errorf("cfg = %+v", op.cfg)
		}
		var _ SessionProvider = op
	})

	t.Run("unknown", func(t *testing.T) {
		if _, err := NewProvider(resolved(t, "", config.Auth{Type: "kerberos"}), Options{}); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestCacheKey(t *testing.T) {
	a := config.Auth{Issuer: "https://idp", ClientID: "c1"}
	if got := cacheKey(resolved(t, "production", a)); got != "profile:production" {
		t.Errorf("profile key = %q", got)
	}
	direct := cacheKey(resolved(t, "", a))
	if !strings.HasPrefix(direct, "direct:") {
		t.Errorf("direct key = %q", direct)
	}
	b := a
	b.ClientID = "c2"
	if cacheKey(resolved(t, "", b)) == direct {
		t.Error("different clients must not share a cache entry")
	}
}

func TestJWTProvider(t *testing.T) {
	now := time.Now()
	exp := now.Add(time.Hour).Truncate(time.Second)
	p := &JWTProvider{Token: unsignedJWT(t, map[string]any{"sub": "svc", "preferred_username": "jdoe", "exp": exp.Unix()})}
	creds, err := p.Authenticate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.Identity != "jdoe" || !creds.Expiry.Equal(exp) || creds.Token != p.Token {
		t.Errorf("creds = %+v", creds)
	}

	expired := &JWTProvider{Token: unsignedJWT(t, map[string]any{"sub": "svc", "exp": now.Add(-time.Minute).Unix()})}
	_, err = expired.Authenticate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %v, want an expiry error", err)
	}
	if strings.Contains(err.Error(), expired.Token) {
		t.Error("the error message contains the token")
	}
}

func TestParseJWTClaims(t *testing.T) {
	for _, bad := range []string{"", "opaque", "a.b", "a.!!!.c", "a.e30x.c"} {
		if _, err := parseJWTClaims(bad); err == nil {
			t.Errorf("parseJWTClaims(%q) should fail", bad)
		}
	}
	claims, err := parseJWTClaims(unsignedJWT(t, map[string]any{"email": "a@b.c", "exp": 1700000000}))
	if err != nil {
		t.Fatal(err)
	}
	if claimString(claims, "missing", "", "email") != "a@b.c" || claimTime(claims, "exp").Unix() != 1700000000 {
		t.Errorf("claims = %v", claims)
	}
	if !claimTime(claims, "nbf").IsZero() {
		t.Error("a missing time claim must be zero")
	}
}

func TestFileStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "tokens")
	s := &FileStore{Dir: dir}
	const key = "profile:prod/uction" // must not escape the directory

	if ts, err := s.Load(key); ts != nil || err != nil {
		t.Fatalf("Load(missing) = %v, %v", ts, err)
	}
	in := &TokenSet{AccessToken: "a", IDToken: "i", RefreshToken: "r", Identity: "u@e.com",
		IDExpiry: time.Now().Add(time.Hour).Truncate(time.Second), Fingerprint: "fp"}
	if err := s.Save(key, in); err != nil {
		t.Fatal(err)
	}
	out, err := s.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	if out.AccessToken != "a" || out.IDToken != "i" || out.RefreshToken != "r" || out.Identity != "u@e.com" ||
		!out.IDExpiry.Equal(in.IDExpiry) || out.Fingerprint != "fp" || !strings.HasPrefix(out.Storage, "file ") {
		t.Errorf("round trip = %+v", out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one file in the token directory, got %v (%v)", entries, err)
	}
	// The name must be valid everywhere: on Windows a ':' would address an
	// alternate data stream instead of a file.
	if name := entries[0].Name(); strings.ContainsAny(name, `:/\*?"<>|%`) || !strings.HasSuffix(name, ".json") {
		t.Errorf("token file name %q is not portable", name)
	}
	if (&FileStore{Dir: dir}).path("profile:a/b") == (&FileStore{Dir: dir}).path("profile:a_b") {
		t.Error("different keys must not share a file")
	}
	if runtime.GOOS != "windows" {
		info, _ := entries[0].Info()
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("token file mode = %o, want 600", perm)
		}
		dirInfo, _ := os.Stat(dir)
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("token directory mode = %o, want 700", perm)
		}
	}

	if err := s.Delete(key); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(key); err != nil {
		t.Errorf("deleting twice must not fail: %v", err)
	}
	if ts, _ := s.Load(key); ts != nil {
		t.Error("entry still present after delete")
	}
}

// Credential managers cap the size of an entry, and real token sets exceed
// the cap. Large sets are spread over several entries instead of being pushed
// out to a file.
func TestKeyringStoreChunksLargeEntries(t *testing.T) {
	keyring.MockInit()
	const key = "profile:big"
	s := keyringStore{}
	big := &TokenSet{AccessToken: strings.Repeat("a", 3000), RefreshToken: strings.Repeat("r", 1500), Identity: "josé@example.com", Fingerprint: "fp"}
	if err := s.Save(key, big); err != nil {
		t.Fatal(err)
	}
	head, err := keyring.Get(keyringService, key)
	if err != nil || !strings.HasPrefix(head, keyringChunked) {
		t.Fatalf("main entry = %q, %v; want a chunk manifest", head, err)
	}
	parts := keyringParts(key)
	if parts < 3 {
		t.Fatalf("expected several chunks, got %d", parts)
	}
	for i := range parts {
		part, err := keyring.Get(keyringService, keyringPart(key, i))
		if err != nil || len(part) > keyringChunk {
			t.Errorf("chunk %d: %d bytes, %v", i, len(part), err)
		}
	}
	got, err := s.Load(key)
	if err != nil || got.AccessToken != big.AccessToken || got.RefreshToken != big.RefreshToken || got.Identity != big.Identity {
		t.Fatalf("round trip failed: %v", err)
	}

	// A smaller set replaces it without leaving old chunks behind.
	small := &TokenSet{AccessToken: "short", Fingerprint: "fp"}
	if err := s.Save(key, small); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(key); err != nil || got.AccessToken != "short" {
		t.Fatalf("small round trip: %+v, %v", got, err)
	}
	for i := range parts {
		if _, err := keyring.Get(keyringService, keyringPart(key, i)); !errors.Is(err, keyring.ErrNotFound) {
			t.Errorf("stale chunk %d was left behind", i)
		}
	}

	// Deleting a chunked entry removes every part.
	if err := s.Save(key, big); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(key); err != nil {
		t.Fatal(err)
	}
	for i := range parts {
		if _, err := keyring.Get(keyringService, keyringPart(key, i)); !errors.Is(err, keyring.ErrNotFound) {
			t.Errorf("chunk %d survived delete", i)
		}
	}
	if got, _ := s.Load(key); got != nil {
		t.Error("entry survived delete")
	}
}

type brokenStore struct{ err error }

func (b brokenStore) Load(string) (*TokenSet, error) { return nil, b.err }
func (b brokenStore) Save(string, *TokenSet) error   { return b.err }
func (b brokenStore) Delete(string) error            { return b.err }

func TestFallbackStore(t *testing.T) {
	keyring.MockInit()
	const key = "profile:test"
	file := &FileStore{Dir: t.TempDir()}

	t.Run("prefers the keyring", func(t *testing.T) {
		s := &fallbackStore{primary: keyringStore{}, secondary: file}
		// A stale file copy must not survive a keyring save.
		if err := file.Save(key, &TokenSet{AccessToken: "stale"}); err != nil {
			t.Fatal(err)
		}
		ts := &TokenSet{AccessToken: "fresh"}
		if err := s.Save(key, ts); err != nil {
			t.Fatal(err)
		}
		if ts.Storage != "system keyring" {
			t.Errorf("storage = %q", ts.Storage)
		}
		if leftover, _ := file.Load(key); leftover != nil {
			t.Error("stale file copy was not removed")
		}
		got, err := s.Load(key)
		if err != nil || got.AccessToken != "fresh" || got.Storage != "system keyring" {
			t.Errorf("Load = %+v, %v", got, err)
		}
		if err := s.Delete(key); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Load(key); got != nil {
			t.Error("entry survived delete")
		}
	})

	t.Run("falls back to files", func(t *testing.T) {
		s := &fallbackStore{primary: brokenStore{errors.New("no keyring daemon")}, secondary: file}
		ts := &TokenSet{AccessToken: "x"}
		if err := s.Save(key, ts); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ts.Storage, "file ") {
			t.Errorf("storage = %q", ts.Storage)
		}
		got, err := s.Load(key)
		if err != nil || got == nil || got.AccessToken != "x" {
			t.Errorf("Load = %+v, %v", got, err)
		}
		// An unreachable keyring cannot be holding anything: logout succeeds.
		if err := s.Delete(key); err != nil {
			t.Errorf("Delete = %v", err)
		}
		if got, _ := s.Load(key); got != nil {
			t.Error("entry survived delete")
		}
	})

	t.Run("logout fails loudly if the keyring keeps the entry", func(t *testing.T) {
		stuck := &stuckStore{memStore: newMemStore()}
		s := &fallbackStore{primary: stuck, secondary: file}
		if err := s.Save(key, &TokenSet{AccessToken: "x"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(key); err == nil {
			t.Error("Delete must report an entry it could not remove")
		}
	})
}

// stuckStore saves and loads but cannot delete.
type stuckStore struct{ *memStore }

func (stuckStore) Delete(string) error { return errors.New("keychain is locked") }

func TestLoginRequiredErrorWithoutProfile(t *testing.T) {
	msg := (&LoginRequiredError{}).Error()
	if !strings.Contains(msg, "chcli auth login") || strings.Contains(msg, "--profile") {
		t.Errorf("message = %q", msg)
	}
}
