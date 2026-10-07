package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nenych/chcli/internal/config"
)

// memStore is an in-memory TokenStore.
type memStore struct {
	mu    sync.Mutex
	sets  map[string]TokenSet
	saves int
}

func newMemStore() *memStore { return &memStore{sets: map[string]TokenSet{}} }

func (s *memStore) Load(key string) (*TokenSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.sets[key]
	if !ok {
		return nil, nil
	}
	ts.Storage = "memory"
	return &ts, nil
}

func (s *memStore) Save(key string, ts *TokenSet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets[key] = *ts
	s.saves++
	ts.Storage = "memory"
	return nil
}

func (s *memStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sets, key)
	return nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness wires a provider to a fake identity provider, an in-memory token
// store, a controllable clock and a scripted "browser".
type harness struct {
	t        *testing.T
	idp      *fakeIDP
	store    *memStore
	clock    *fakeClock
	out      *bytes.Buffer
	browsers atomic.Int32 // how many times a browser was opened
	cfg      OIDCConfig
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, idp: newFakeIDP(t), store: newMemStore(), clock: &fakeClock{t: time.Now()}, out: &bytes.Buffer{}}
	h.idp.now = h.clock.Now
	h.cfg = OIDCConfig{
		Label:         "OIDC",
		Profile:       "production",
		CacheKey:      "profile:production",
		ClientID:      h.idp.clientID,
		ClientSecret:  NewSecretSource("client secret", "client-secret-value", config.Command{}, nil),
		Issuer:        h.idp.issuer(),
		Scopes:        []string{"openid", "email"},
		UsernameClaim: "email",
		Flow:          config.FlowBrowser,
		TokenType:     config.TokenTypeID,
	}
	return h
}

// browse plays the user's browser: it follows the authorization URL, which
// the fake provider redirects to the loopback callback.
func (h *harness) browse(authURL string) error {
	h.browsers.Add(1)
	go func() {
		resp, err := http.Get(authURL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	return nil
}

// provider builds a fresh provider, as a new process would.
func (h *harness) provider(interactive bool) *OIDCProvider {
	p := newOIDCProvider(h.cfg, Options{Interactive: interactive, Out: h.out, Store: h.store, OpenBrowser: h.browse})
	p.now = h.clock.Now
	return p
}

func (h *harness) stored() TokenSet {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return h.store.sets[h.cfg.CacheKey]
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestBrowserLogin(t *testing.T) {
	h := newHarness(t)
	h.cfg = googlePreset(h.cfg)
	h.cfg.Label = "Google OAuth"

	creds, err := h.provider(true).Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !creds.UsesToken() || creds.Identity != "user@example.com" {
		t.Errorf("credentials = %+v", creds)
	}
	if got := creds.Expiry; got.Before(h.clock.Now().Add(59*time.Minute)) || got.After(h.clock.Now().Add(61*time.Minute)) {
		t.Errorf("expiry = %v, want about an hour from now", got)
	}

	// The authorization request used PKCE, a state and a nonce, and the
	// Google preset asked for offline access.
	auth := h.idp.lastRequest("/authorize")
	for _, param := range []string{"state", "nonce", "code_challenge"} {
		if auth.Get(param) == "" {
			t.Errorf("authorization request lacks %s", param)
		}
	}
	if auth.Get("code_challenge_method") != "S256" || auth.Get("scope") != "openid email" ||
		auth.Get("access_type") != "offline" || auth.Get("prompt") != "consent" {
		t.Errorf("authorization request = %v", auth)
	}
	if !strings.HasPrefix(auth.Get("redirect_uri"), "http://127.0.0.1:") {
		t.Errorf("redirect_uri = %q, want a loopback URL", auth.Get("redirect_uri"))
	}
	// The fake provider only issues tokens when the PKCE verifier matches.
	if h.idp.lastRequest("/token").Get("code_verifier") == "" {
		t.Error("token request lacks code_verifier")
	}

	// The session was persisted, with the ID token as the bearer. The access
	// token is not needed again and must not be kept.
	stored := h.stored()
	if stored.IDToken != creds.Token || stored.RefreshToken == "" || stored.Identity != "user@example.com" {
		t.Errorf("stored session incomplete: id=%t refresh=%t identity=%q", stored.IDToken != "", stored.RefreshToken != "", stored.Identity)
	}
	if stored.AccessToken != "" {
		t.Error("the unused access token was stored")
	}

	out := h.out.String()
	if !strings.Contains(out, "Opening browser for Google authentication...") {
		t.Errorf("missing login message in output:\n%s", out)
	}
	for name, secret := range map[string]string{
		"ID token": stored.IDToken, "refresh token": stored.RefreshToken, "client secret": "client-secret-value",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("%s leaked into user-facing output", name)
		}
	}
}

func TestCachedTokenIsReusedWithoutNetworkOrBrowser(t *testing.T) {
	h := newHarness(t)
	first, err := h.provider(true).Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	tokenRequests, discoveries, browsers := h.idp.requestCount("/token"), h.idp.requestCount("/.well-known/openid-configuration"), h.browsers.Load()

	// A new process, not allowed to interact.
	p := h.provider(false)
	for range 3 {
		creds, err := p.Authenticate(testContext(t))
		if err != nil {
			t.Fatal(err)
		}
		if creds.Token != first.Token || creds.Identity != first.Identity {
			t.Error("cached credentials differ from the original login")
		}
	}
	if h.idp.requestCount("/token") != tokenRequests || h.idp.requestCount("/.well-known/openid-configuration") != discoveries {
		t.Error("a valid cached token must not cause any request to the provider")
	}
	if h.browsers.Load() != browsers {
		t.Error("a valid cached token must not open the browser")
	}
}

func TestExpiredTokenIsRefreshedAutomatically(t *testing.T) {
	h := newHarness(t)
	first, err := h.provider(true).Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	refreshToken := h.stored().RefreshToken
	p := h.provider(false)

	// Still fresh just before the skew window...
	h.clock.Advance(time.Hour - 2*expirySkew)
	if creds, err := p.Authenticate(testContext(t)); err != nil || creds.Token != first.Token {
		t.Fatalf("token should still be reused: %v", err)
	}
	// ...and refreshed inside it, before it actually expires.
	h.clock.Advance(90 * time.Second)
	creds, err := p.Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if creds.Token == first.Token {
		t.Error("an expiring token must be replaced")
	}
	if got := h.idp.lastRequest("/token"); got.Get("grant_type") != "refresh_token" || got.Get("refresh_token") != refreshToken {
		t.Errorf("expected a refresh grant, got %v", got.Get("grant_type"))
	}
	if h.browsers.Load() != 1 {
		t.Error("refreshing must not open the browser")
	}
	stored := h.stored()
	if stored.IDToken != creds.Token {
		t.Error("the refreshed token was not persisted")
	}
	if stored.RefreshToken != refreshToken {
		t.Error("the refresh token must be kept when the provider does not rotate it")
	}
	if creds.Identity != "user@example.com" {
		t.Errorf("identity lost on refresh: %q", creds.Identity)
	}
}

func TestRotatedRefreshTokenIsStored(t *testing.T) {
	h := newHarness(t)
	h.idp.rotateRefresh = true
	if _, err := h.provider(true).Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	old := h.stored().RefreshToken
	h.clock.Advance(2 * time.Hour)
	if _, err := h.provider(false).Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if got := h.stored().RefreshToken; got == old || got == "" {
		t.Errorf("rotated refresh token not stored (old %q, new %q)", old, got)
	}
}

func TestRevokedRefreshTokenRequiresLogin(t *testing.T) {
	h := newHarness(t)
	if _, err := h.provider(true).Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Hour)
	h.idp.revokeRefreshTokens()

	// Non-interactive: a clear, typed error and no browser.
	_, err := h.provider(false).Authenticate(testContext(t))
	var loginErr *LoginRequiredError
	if !errors.As(err, &loginErr) {
		t.Fatalf("error = %v, want LoginRequiredError", err)
	}
	want := "OAuth credentials for profile \"production\" are not available.\nRun:\n  chcli auth login --profile production\nfrom an interactive terminal first."
	if err.Error() != want {
		t.Errorf("message:\n%s\nwant:\n%s", err.Error(), want)
	}
	if h.browsers.Load() != 1 {
		t.Error("non-interactive mode opened a browser")
	}

	// Interactive: falls back to a new login.
	creds, err := h.provider(true).Authenticate(testContext(t))
	if err != nil || creds.Token == "" {
		t.Fatalf("interactive re-login failed: %v", err)
	}
	if h.browsers.Load() != 2 {
		t.Error("interactive mode should have logged in again")
	}
}

// Only an explicit rejection of the refresh token (invalid_grant) may end a
// session. Everything else a token endpoint can answer with is potentially
// temporary: the session must survive and no browser may open.
func TestTransientRefreshFailureKeepsSession(t *testing.T) {
	failures := map[string]struct {
		status int
		code   string
	}{
		"503 server error":        {http.StatusServiceUnavailable, ""},
		"429 rate limited":        {http.StatusTooManyRequests, "slow_down"},
		"400 temporarily unavail": {http.StatusBadRequest, "temporarily_unavailable"},
		"401 invalid client":      {http.StatusUnauthorized, "invalid_client"},
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if _, err := h.provider(true).Authenticate(testContext(t)); err != nil {
				t.Fatal(err)
			}
			before := h.stored()
			h.clock.Advance(2 * time.Hour)
			h.idp.tokenStatus, h.idp.tokenErrorCode = failure.status, failure.code

			// Even an interactive session must not be thrown into a browser
			// login because the provider had a hiccup.
			_, err := h.provider(true).Authenticate(testContext(t))
			var loginErr *LoginRequiredError
			if err == nil || errors.As(err, &loginErr) || !strings.Contains(err.Error(), "refresh") {
				t.Fatalf("error = %v, want a refresh failure", err)
			}
			if _, err := h.provider(false).Authenticate(testContext(t)); err == nil || errors.As(err, &loginErr) {
				t.Fatalf("non-interactive error = %v, want a refresh failure, not a login request", err)
			}
			if h.browsers.Load() != 1 {
				t.Error("a transient failure must not trigger a new login")
			}
			if h.stored().RefreshToken != before.RefreshToken {
				t.Error("the session must survive a transient failure")
			}

			h.idp.tokenStatus = 0
			if _, err := h.provider(false).Authenticate(testContext(t)); err != nil {
				t.Errorf("refresh should work again once the provider recovers: %v", err)
			}
		})
	}
}

// With refresh token rotation the old token is spent the moment the provider
// answers. If validating the response then fails (here: the signing keys
// cannot be fetched), the new refresh token must already be stored, or the
// session would be unrecoverable.
func TestRotatedRefreshTokenSurvivesFailedValidation(t *testing.T) {
	h := newHarness(t)
	h.idp.rotateRefresh = true
	if _, err := h.provider(true).Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	original := h.stored().RefreshToken
	h.clock.Advance(2 * time.Hour)
	h.idp.jwksStatus = http.StatusInternalServerError

	_, err := h.provider(false).Authenticate(testContext(t))
	var loginErr *LoginRequiredError
	if err == nil || errors.As(err, &loginErr) {
		t.Fatalf("error = %v, want a validation failure that keeps the session", err)
	}
	rotated := h.stored().RefreshToken
	if rotated == original || rotated == "" {
		t.Fatalf("the rotated refresh token was not stored (still %q)", rotated)
	}

	h.idp.jwksStatus = 0
	creds, err := h.provider(false).Authenticate(testContext(t))
	if err != nil || creds.Token == "" {
		t.Fatalf("the session should recover once the keys are reachable again: %v", err)
	}
	if h.browsers.Load() != 1 {
		t.Error("recovery must not need a new login")
	}
}

// A long-running shell and a second chcli process share one stored session.
// When the other process has refreshed (and rotated) the tokens, the shell
// must pick the new ones up instead of presenting its stale refresh token.
func TestSessionRefreshedByAnotherProcessIsPickedUp(t *testing.T) {
	h := newHarness(t)
	h.idp.rotateRefresh = true
	shell := h.provider(true)
	if _, err := shell.Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Hour)

	other, err := h.provider(false).Authenticate(testContext(t)) // rotates the refresh token
	if err != nil {
		t.Fatal(err)
	}
	tokenRequests := h.idp.requestCount("/token")

	creds, err := shell.Authenticate(testContext(t))
	if err != nil {
		t.Fatalf("the shell lost its session: %v", err)
	}
	if creds.Token != other.Token {
		t.Error("the shell should use the tokens the other process stored")
	}
	if h.idp.requestCount("/token") != tokenRequests {
		t.Error("no further refresh should have been needed")
	}
	if h.browsers.Load() != 1 {
		t.Error("no new login should have been needed")
	}

	// And a logout elsewhere ends the shell's session too.
	if err := h.provider(false).Logout(); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Hour)
	var loginErr *LoginRequiredError
	if _, err := NonInteractive(shell).Authenticate(testContext(t)); !errors.As(err, &loginErr) {
		t.Errorf("after a logout elsewhere: %v, want LoginRequiredError", err)
	}
}

// Background work (loading completion metadata) shares the interactive
// provider of the shell but must never start a login itself.
func TestNonInteractiveViewNeverStartsLogin(t *testing.T) {
	h := newHarness(t)
	shell := h.provider(true)
	if _, err := shell.Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	background := NonInteractive(shell)

	// While the session is good, the view shares it.
	if creds, err := background.Authenticate(testContext(t)); err != nil || creds.Token == "" {
		t.Fatalf("background credentials: %v", err)
	}
	h.clock.Advance(2 * time.Hour)
	h.idp.revokeRefreshTokens()

	var loginErr *LoginRequiredError
	if _, err := background.Authenticate(testContext(t)); !errors.As(err, &loginErr) {
		t.Fatalf("error = %v, want LoginRequiredError", err)
	}
	if h.browsers.Load() != 1 || h.idp.requestCount("/authorize") != 1 {
		t.Error("the background view started a login")
	}
	// The shell itself still can.
	if _, err := shell.Authenticate(testContext(t)); err != nil {
		t.Fatalf("foreground re-login: %v", err)
	}
	if p := NonInteractive(&PasswordProvider{Username: "u"}); p == nil {
		t.Error("other providers pass through unchanged")
	}
}

// A caller waiting for a login in progress must still be cancellable.
func TestWaitingForLoginHonoursContext(t *testing.T) {
	h := newHarness(t)
	p := h.provider(true)
	p.openBrowser = func(string) error { return nil } // a login nobody completes
	loginCtx, stopLogin := context.WithCancel(context.Background())
	loginDone := make(chan struct{})
	go func() {
		defer close(loginDone)
		_, _ = p.Authenticate(loginCtx)
	}()
	for h.idp.requestCount("/.well-known/openid-configuration") == 0 { // the login holds the lock now
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := NonInteractive(p).Authenticate(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the caller's deadline", err)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Errorf("the waiting caller was held for %v", waited)
	}
	stopLogin()
	<-loginDone
}

func TestNonInteractiveWithoutSessionNeverOpensBrowser(t *testing.T) {
	h := newHarness(t)
	_, err := h.provider(false).Authenticate(testContext(t))
	var loginErr *LoginRequiredError
	if !errors.As(err, &loginErr) || loginErr.Profile != "production" {
		t.Fatalf("error = %v, want LoginRequiredError for the profile", err)
	}
	if h.browsers.Load() != 0 || h.idp.requestCount("/authorize") != 0 {
		t.Error("non-interactive mode must not start a login")
	}
	if _, err := h.provider(false).Login(testContext(t)); !errors.As(err, &loginErr) {
		t.Errorf("Login in non-interactive mode = %v, want LoginRequiredError", err)
	}
}

func TestCallbackRejectsWrongState(t *testing.T) {
	h := newHarness(t)
	p := h.provider(true)
	var forgedStatus atomic.Int32
	p.openBrowser = func(authURL string) error {
		go func() {
			u, _ := url.Parse(authURL)
			// A forged callback (CSRF): right endpoint, wrong state.
			forged := u.Query().Get("redirect_uri") + "?code=attacker-code&state=forged"
			if resp, err := http.Get(forged); err == nil {
				forgedStatus.Store(int32(resp.StatusCode))
				resp.Body.Close()
			}
			// The genuine login then proceeds.
			if resp, err := http.Get(authURL); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	creds, err := p.Authenticate(testContext(t))
	if err != nil {
		t.Fatalf("the genuine login should still succeed: %v", err)
	}
	if forgedStatus.Load() != http.StatusBadRequest {
		t.Errorf("forged callback got status %d, want 400", forgedStatus.Load())
	}
	if creds.Identity != "user@example.com" {
		t.Errorf("identity = %q", creds.Identity)
	}
	if got := h.idp.lastRequest("/token").Get("code"); got == "attacker-code" {
		t.Error("the forged authorization code was exchanged")
	}
}

func TestLoginFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
		want  string
	}{
		{"provider denies", func(h *harness) { h.idp.authError = "access_denied" }, "access_denied: the user said no"},
		{"nonce mismatch", func(h *harness) { h.idp.wrongNonce = true }, "nonce does not match"},
		{"forged signature", func(h *harness) { h.idp.signKey = newFakeIDP(h.t).key }, "not valid"},
		{"no id token", func(h *harness) { h.idp.noIDToken = true }, "did not return an ID token"},
		{"token for another client", func(h *harness) { h.idp.idTokenAudience = "another-client" }, "not valid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			_, err := h.provider(true).Authenticate(testContext(t))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if len(h.store.sets) != 0 {
				t.Error("a failed login must not store anything")
			}
		})
	}
}

func TestLogoutAndStatus(t *testing.T) {
	h := newHarness(t)
	p := h.provider(true)

	st, err := p.Status()
	if err != nil || st.LoggedIn {
		t.Fatalf("status before login = %+v, %v", st, err)
	}
	if _, err := p.Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	st, err = h.provider(false).Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.LoggedIn || st.Identity != "user@example.com" || !st.Refreshable || st.Expiry.IsZero() || st.Storage != "memory" {
		t.Errorf("status after login = %+v", st)
	}

	if err := p.Logout(); err != nil {
		t.Fatal(err)
	}
	if len(h.store.sets) != 0 {
		t.Error("logout left tokens in the store")
	}
	if st, _ := p.Status(); st.LoggedIn {
		t.Error("still logged in after logout")
	}
	var loginErr *LoginRequiredError
	if _, err := h.provider(false).Authenticate(testContext(t)); !errors.As(err, &loginErr) {
		t.Errorf("after logout: %v, want LoginRequiredError", err)
	}
}

func TestChangedProviderConfigInvalidatesCache(t *testing.T) {
	h := newHarness(t)
	if _, err := h.provider(true).Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	h.cfg.Audience = "a-different-audience"
	var loginErr *LoginRequiredError
	if _, err := h.provider(false).Authenticate(testContext(t)); !errors.As(err, &loginErr) {
		t.Errorf("tokens issued for another configuration were reused (err=%v)", err)
	}
}

func TestAccessTokenTypeAndAudience(t *testing.T) {
	h := newHarness(t)
	h.idp.jwtAccessTokens = true
	h.cfg.TokenType = config.TokenTypeAccess
	h.cfg.Audience = "clickhouse"
	h.cfg.UsernameClaim = "preferred_username"

	creds, err := h.provider(true).Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	stored := h.stored()
	if creds.Token != stored.AccessToken || stored.IDToken != "" {
		t.Error("token_type access_token must present the access token and store nothing else")
	}
	if claims, err := parseJWTClaims(creds.Token); err != nil || claimString(claims, "aud") != "clickhouse" {
		t.Errorf("the bearer is not the access token: %v", err)
	}
	if got := h.idp.lastRequest("/authorize").Get("audience"); got != "clickhouse" {
		t.Errorf("audience parameter = %q", got)
	}
	// The ID token has no preferred_username, so the fallback chain yields the e-mail.
	if creds.Identity != "user@example.com" {
		t.Errorf("identity = %q", creds.Identity)
	}
}

func TestIdentityFromAccessTokenWhenNoIDToken(t *testing.T) {
	h := newHarness(t)
	h.idp.jwtAccessTokens = true
	h.idp.noIDToken = true
	h.cfg.TokenType = config.TokenTypeAccess
	h.cfg.UsernameClaim = "preferred_username"

	creds, err := h.provider(true).Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if creds.Identity != "jdoe" {
		t.Errorf("identity = %q, want the access token's preferred_username", creds.Identity)
	}
}

func TestPublicClientWithoutSecret(t *testing.T) {
	h := newHarness(t)
	h.cfg.ClientSecret = nil
	if _, err := h.provider(true).Authenticate(testContext(t)); err != nil {
		t.Fatalf("a public client must be able to log in with PKCE alone: %v", err)
	}
	if got := h.idp.lastRequest("/token"); got.Get("client_secret") != "" || got.Get("code_verifier") == "" {
		t.Errorf("token request = %v", got)
	}
}

func TestManualEndpointsWithoutDiscovery(t *testing.T) {
	h := newHarness(t)
	h.cfg.Issuer = ""
	h.cfg.AuthURL = h.idp.issuer() + "/authorize"
	h.cfg.TokenURL = h.idp.issuer() + "/token"

	creds, err := h.provider(true).Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if creds.Identity != "user@example.com" {
		t.Errorf("identity = %q", creds.Identity)
	}
	if h.idp.requestCount("/.well-known/openid-configuration") != 0 {
		t.Error("no discovery expected without an issuer")
	}
}

// Without discovery there are no keys to verify an ID token's signature
// with, but a token for another client, or an expired one, is still refused.
func TestIDTokenClaimsAreCheckedWithoutDiscovery(t *testing.T) {
	for name, breakIt := range map[string]func(h *harness){
		"another audience": func(h *harness) { h.idp.idTokenAudience = "another-client" },
		"expired":          func(h *harness) { h.idp.tokenTTL = -time.Minute },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Issuer = ""
			h.cfg.AuthURL = h.idp.issuer() + "/authorize"
			h.cfg.TokenURL = h.idp.issuer() + "/token"
			breakIt(h)
			_, err := h.provider(true).Authenticate(testContext(t))
			if err == nil || !strings.Contains(err.Error(), "not valid") {
				t.Fatalf("error = %v, want the ID token to be refused", err)
			}
			if len(h.store.sets) != 0 {
				t.Error("nothing may be stored after a refused login")
			}
		})
	}
}

// URLs supplied by the identity provider are only handed to the system's
// opener if they are web addresses.
func TestOnlyWebAddressesAreOpened(t *testing.T) {
	h := newHarness(t)
	p := h.provider(true)
	opened := 0
	p.openBrowser = func(string) error { opened++; return nil }
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "calculator:", "", "https://", "/relative"} {
		if err := p.open(u); err == nil {
			t.Errorf("open(%q) was allowed", u)
		}
	}
	if opened != 0 {
		t.Errorf("%d non-web URLs reached the opener", opened)
	}
	if err := p.open("https://idp.example.com/device?user_code=ABCD"); err != nil || opened != 1 {
		t.Errorf("a web address must be opened: %v", err)
	}
}

func TestEndpointOverridesBeatDiscovery(t *testing.T) {
	h := newHarness(t)
	h.cfg.TokenURL = "https://override.example.com/token"
	ep, err := h.provider(false).Endpoints(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Token != "https://override.example.com/token" || ep.Authorization != h.idp.issuer()+"/authorize" ||
		ep.Device != h.idp.issuer()+"/device" || !ep.Discovered {
		t.Errorf("endpoints = %+v", ep)
	}
}

func TestDeviceFlow(t *testing.T) {
	h := newHarness(t)
	h.cfg.Flow = config.FlowDevice
	h.idp.devicePending = 1

	creds, err := h.provider(true).Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if creds.Identity != "user@example.com" {
		t.Errorf("identity = %q", creds.Identity)
	}
	out := h.out.String()
	if !strings.Contains(out, "ABCD-EFGH") || !strings.Contains(out, h.idp.issuer()+"/activate") {
		t.Errorf("device instructions missing from output:\n%s", out)
	}
	if h.idp.requestCount("/authorize") != 0 {
		t.Error("the device flow must not use the authorization endpoint")
	}
}

func TestBrowserFallbackPrintsURL(t *testing.T) {
	h := newHarness(t)
	p := h.provider(true)
	p.openBrowser = func(authURL string) error {
		h.browse(authURL) //nolint:errcheck // the "user" opens the printed URL by hand
		return errors.New("no display")
	}
	if _, err := p.Authenticate(testContext(t)); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	if !strings.Contains(out, "Open the following URL in your browser:\n"+h.idp.issuer()+"/authorize?") ||
		!strings.Contains(out, "Waiting for authentication...") {
		t.Errorf("fallback instructions missing:\n%s", out)
	}
}

func TestLoginCanBeCancelled(t *testing.T) {
	h := newHarness(t)
	p := h.provider(true)
	p.openBrowser = func(string) error { return nil } // the user never completes the login
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := p.Authenticate(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
