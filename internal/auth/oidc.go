package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/nenych/chcli/internal/config"
)

// expirySkew is how long before its expiry a token is considered stale, so
// that a token never expires between being handed out and reaching the server.
const expirySkew = time.Minute

// OIDCConfig configures the generic OAuth 2.0 / OpenID Connect provider.
type OIDCConfig struct {
	// Label names the provider in messages ("Google OAuth", "OIDC").
	Label string
	// Profile is the connection profile, used in the login hint.
	Profile string
	// CacheKey names the token cache entry.
	CacheKey string

	ClientID     string
	ClientSecret string // optional: public clients rely on PKCE alone
	Issuer       string
	// AuthURL, TokenURL and DeviceURL override the endpoints found through
	// OIDC discovery. Without an Issuer they are the only source.
	AuthURL   string
	TokenURL  string
	DeviceURL string

	Audience      string
	Scopes        []string
	UsernameClaim string
	RedirectURI   string
	Flow          string
	TokenType     string
	// AuthParams are extra authorization request parameters.
	AuthParams map[string]string
}

// fingerprint changes whenever the settings that determine which tokens are
// issued change.
func (c OIDCConfig) fingerprint() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		c.Issuer, c.TokenURL, c.ClientID, c.Audience, strings.Join(c.Scopes, " "),
	}, "\n")))
	return hex.EncodeToString(sum[:8])
}

// OIDCProvider implements browser and device logins against any OpenID
// Connect provider, caches the resulting tokens and refreshes them.
type OIDCProvider struct {
	cfg         OIDCConfig
	store       TokenStore
	interactive bool
	out         io.Writer
	openBrowser func(string) error
	httpClient  *http.Client
	now         func() time.Time

	// lock serialises logins and refreshes. It is a channel rather than a
	// mutex so that a caller waiting behind a slow network call can still be
	// cancelled.
	lock    chan struct{}
	current *TokenSet // in-memory copy of the stored session
	loaded  bool
}

func newOIDCProvider(cfg OIDCConfig, o Options) *OIDCProvider {
	p := &OIDCProvider{
		cfg:         cfg,
		store:       o.Store,
		interactive: o.Interactive,
		out:         o.Out,
		openBrowser: o.OpenBrowser,
		httpClient:  o.HTTPClient,
		now:         time.Now,
		lock:        make(chan struct{}, 1),
	}
	if p.out == nil {
		p.out = io.Discard
	}
	if p.httpClient == nil {
		p.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if p.openBrowser == nil {
		p.openBrowser = func(string) error { return errors.New("no browser available") }
	}
	return p
}

func (p *OIDCProvider) acquire(ctx context.Context) error {
	select {
	case p.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *OIDCProvider) release() { <-p.lock }

// Authenticate returns valid token credentials, doing the least work that
// achieves it: cached tokens are reused, expired ones refreshed, and only when
// neither is possible does an interactive login run. Non-interactive callers
// get a LoginRequiredError instead of a browser.
func (p *OIDCProvider) Authenticate(ctx context.Context) (*Credentials, error) {
	return p.authenticate(ctx, p.interactive)
}

func (p *OIDCProvider) authenticate(ctx context.Context, interactive bool) (*Credentials, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()

	ts := p.session()
	if ts != nil && p.fresh(ts) {
		return p.credentials(ts), nil
	}
	// Before spending a refresh token, look at the store again: another
	// chcli process may have refreshed or replaced the session since it was
	// read, and with rotating refresh tokens ours would then be rejected.
	if ts = p.reload(); ts != nil && p.fresh(ts) {
		return p.credentials(ts), nil
	}

	if ts != nil && ts.RefreshToken != "" {
		refreshed, err := p.refresh(ctx, ts)
		switch {
		case err == nil:
			return p.credentials(refreshed), nil
		case !needsLogin(err):
			// Anything but an outright rejection of the refresh token may be
			// temporary: keep the session and report the problem.
			return nil, fmt.Errorf("refresh %s token: %w", p.cfg.Label, err)
		}
		slog.Debug("oidc: refresh token rejected, a new login is required", "error", err)
	}

	if !interactive {
		return nil, &LoginRequiredError{Profile: p.cfg.Profile}
	}
	ts, err := p.login(ctx)
	if err != nil {
		return nil, err
	}
	return p.credentials(ts), nil
}

// NonInteractive returns a view of provider that never starts an interactive
// login: where the original would open a browser, the view fails with a
// LoginRequiredError. It is for background work, which must neither surprise
// the user with a browser window nor hold up the foreground while one is
// open. Cached tokens and refreshes are shared with the original.
func NonInteractive(provider Provider) Provider {
	switch p := provider.(type) {
	case *OIDCProvider:
		return nonInteractive{p}
	case *JWTProvider:
		return nonInteractiveJWT{p}
	}
	return provider
}

type nonInteractive struct{ p *OIDCProvider }

func (n nonInteractive) Authenticate(ctx context.Context) (*Credentials, error) {
	return n.p.authenticate(ctx, false)
}

type nonInteractiveJWT struct{ p *JWTProvider }

func (n nonInteractiveJWT) Authenticate(ctx context.Context) (*Credentials, error) {
	return n.p.authenticate(ctx, false)
}

// Login forces the interactive flow, replacing any cached session.
func (p *OIDCProvider) Login(ctx context.Context) (*Credentials, error) {
	if !p.interactive {
		return nil, &LoginRequiredError{Profile: p.cfg.Profile}
	}
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()
	ts, err := p.login(ctx)
	if err != nil {
		return nil, err
	}
	return p.credentials(ts), nil
}

// Logout removes the cached session.
func (p *OIDCProvider) Logout() error {
	_ = p.acquire(context.Background())
	defer p.release()
	p.current, p.loaded = nil, true
	return p.store.Delete(p.cfg.CacheKey)
}

// Status reports on the cached session without any network access.
func (p *OIDCProvider) Status() (Status, error) {
	_ = p.acquire(context.Background())
	defer p.release()
	ts := p.session()
	if ts == nil {
		return Status{}, nil
	}
	return Status{
		LoggedIn:    true,
		Identity:    ts.Identity,
		Expiry:      p.expiry(ts),
		Refreshable: ts.RefreshToken != "",
		Storage:     ts.Storage,
	}, nil
}

// session returns the cached token set, reading the store at most once.
// Sets issued for a different provider configuration are ignored.
func (p *OIDCProvider) session() *TokenSet {
	if !p.loaded {
		p.loaded = true
		ts, err := p.store.Load(p.cfg.CacheKey)
		if err != nil {
			slog.Debug("oidc: cannot read cached tokens", "error", err)
		}
		if ts != nil && ts.Fingerprint == p.cfg.fingerprint() {
			p.current = ts
		}
	}
	return p.current
}

// reload re-reads the stored session. If the store cannot be read, the
// in-memory copy is kept; if the entry is gone, so is the session (another
// process logged out).
func (p *OIDCProvider) reload() *TokenSet {
	ts, err := p.store.Load(p.cfg.CacheKey)
	if err != nil {
		slog.Debug("oidc: cannot re-read cached tokens", "error", err)
		return p.current
	}
	if ts != nil && ts.Fingerprint != p.cfg.fingerprint() {
		ts = nil
	}
	p.current, p.loaded = ts, true
	return ts
}

func (p *OIDCProvider) bearer(ts *TokenSet) string {
	if p.cfg.TokenType == config.TokenTypeID {
		return ts.IDToken
	}
	return ts.AccessToken
}

func (p *OIDCProvider) expiry(ts *TokenSet) time.Time {
	if p.cfg.TokenType == config.TokenTypeID {
		return ts.IDExpiry
	}
	return ts.AccessExpiry
}

func (p *OIDCProvider) fresh(ts *TokenSet) bool {
	if p.bearer(ts) == "" {
		return false
	}
	exp := p.expiry(ts)
	return exp.IsZero() || p.now().Add(expirySkew).Before(exp)
}

func (p *OIDCProvider) credentials(ts *TokenSet) *Credentials {
	return &Credentials{Token: p.bearer(ts), Identity: ts.Identity, Expiry: p.expiry(ts)}
}

// endpoints resolves the provider endpoints: explicit configuration wins and
// OIDC discovery fills in the rest. The returned oidc.Provider is nil when
// discovery was not possible, in which case ID tokens cannot be verified
// against the provider's keys.
func (p *OIDCProvider) endpoints(ctx context.Context) (oauth2.Endpoint, *oidc.Provider, error) {
	ep := oauth2.Endpoint{AuthURL: p.cfg.AuthURL, TokenURL: p.cfg.TokenURL, DeviceAuthURL: p.cfg.DeviceURL}
	if p.cfg.Issuer == "" {
		return ep, nil, nil
	}
	provider, err := oidc.NewProvider(p.httpContext(ctx), p.cfg.Issuer)
	if err != nil {
		if ep.TokenURL != "" && (ep.AuthURL != "" || ep.DeviceAuthURL != "") {
			slog.Debug("oidc: discovery failed, using configured endpoints", "issuer", p.cfg.Issuer, "error", err)
			return ep, nil, nil
		}
		return ep, nil, fmt.Errorf("OIDC discovery for %s failed: %w", p.cfg.Issuer, err)
	}
	discovered := provider.Endpoint()
	if ep.AuthURL == "" {
		ep.AuthURL = discovered.AuthURL
	}
	if ep.TokenURL == "" {
		ep.TokenURL = discovered.TokenURL
	}
	if ep.DeviceAuthURL == "" {
		ep.DeviceAuthURL = discovered.DeviceAuthURL
	}
	return ep, provider, nil
}

// Endpoints are the resolved provider endpoints, for diagnostics.
type Endpoints struct {
	Authorization string
	Token         string
	Device        string
	// Discovered is true when OIDC discovery succeeded.
	Discovered bool
}

// Endpoints resolves the provider endpoints the same way a login would.
func (p *OIDCProvider) Endpoints(ctx context.Context) (Endpoints, error) {
	ep, provider, err := p.endpoints(ctx)
	return Endpoints{Authorization: ep.AuthURL, Token: ep.TokenURL, Device: ep.DeviceAuthURL, Discovered: provider != nil}, err
}

func (p *OIDCProvider) httpContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
}

func (p *OIDCProvider) oauthConfig(ep oauth2.Endpoint) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     ep,
		Scopes:       p.cfg.Scopes,
	}
}

// login runs the configured interactive flow and stores the new session.
func (p *OIDCProvider) login(ctx context.Context) (*TokenSet, error) {
	ep, provider, err := p.endpoints(ctx)
	if err != nil {
		return nil, err
	}
	conf := p.oauthConfig(ep)

	var (
		tok   *oauth2.Token
		nonce string
	)
	if p.cfg.Flow == config.FlowDevice {
		tok, err = p.deviceFlow(ctx, conf)
	} else {
		tok, nonce, err = p.browserFlow(ctx, conf)
	}
	if err != nil {
		return nil, err
	}

	ts, err := p.tokenSet(ctx, tok, provider, nonce)
	if err != nil {
		return nil, err
	}
	p.remember(ts)
	return ts, nil
}

// refresh trades the refresh token for new tokens and stores them.
func (p *OIDCProvider) refresh(ctx context.Context, old *TokenSet) (*TokenSet, error) {
	ep, provider, err := p.endpoints(ctx)
	if err != nil {
		return nil, err
	}
	slog.Debug("oidc: refreshing tokens")
	src := p.oauthConfig(ep).TokenSource(p.httpContext(ctx), &oauth2.Token{RefreshToken: old.RefreshToken})
	tok, err := src.Token()
	if err != nil {
		return nil, sanitizeOAuthError(err)
	}
	// With refresh token rotation the provider has just invalidated the old
	// token. Store the new one before anything else can fail (verifying the
	// ID token needs the network again), or the session would be lost.
	if tok.RefreshToken != "" && tok.RefreshToken != old.RefreshToken {
		rotated := *old
		rotated.RefreshToken = tok.RefreshToken
		p.remember(&rotated)
		old = &rotated
	}
	ts, err := p.tokenSet(ctx, tok, provider, "")
	if err != nil {
		if errors.Is(err, errNoBearer) {
			// The provider answered, but without the token we present (for
			// example no ID token on refresh): only a full login gets one.
			return nil, &loginNeededError{err}
		}
		return nil, err
	}
	if ts.Identity == "" {
		ts.Identity = old.Identity
	}
	p.remember(ts)
	return ts, nil
}

// errNoBearer is matched by errors reporting that a token response lacks the
// token configured as the one to present to ClickHouse.
var errNoBearer = errors.New("the provider did not return the required token")

type noBearerError string

func (e noBearerError) Error() string        { return string(e) }
func (e noBearerError) Is(target error) bool { return target == errNoBearer }

// tokenSet turns a token endpoint response into a cache entry, verifying the
// ID token when one was returned.
func (p *OIDCProvider) tokenSet(ctx context.Context, tok *oauth2.Token, provider *oidc.Provider, nonce string) (*TokenSet, error) {
	ts := &TokenSet{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		AccessExpiry: tok.Expiry,
		Fingerprint:  p.cfg.fingerprint(),
	}
	var claims map[string]any

	if raw, _ := tok.Extra("id_token").(string); raw != "" {
		idClaims, err := parseJWTClaims(raw)
		if err != nil {
			return nil, errors.New("the provider returned a malformed ID token")
		}
		if provider != nil {
			// Signature, issuer, audience and expiry.
			if _, err := provider.Verifier(&oidc.Config{ClientID: p.cfg.ClientID, Now: p.now}).Verify(p.httpContext(ctx), raw); err != nil {
				return nil, fmt.Errorf("the ID token returned by the provider is not valid: %w", err)
			}
		} else if err := p.checkIDClaims(idClaims); err != nil {
			// Without discovery there are no keys to check the signature
			// with. The token came straight from the token endpoint over
			// TLS, which OpenID Connect Core 3.1.3.7 accepts in place of a
			// signature check, but the claims that tie it to this client and
			// moment are still enforced.
			return nil, fmt.Errorf("the ID token returned by the provider is not valid: %w", err)
		}
		if nonce != "" && claimString(idClaims, "nonce") != nonce {
			return nil, errors.New("the ID token nonce does not match this login attempt")
		}
		ts.IDToken = raw
		ts.IDExpiry = claimTime(idClaims, "exp")
		claims = idClaims
	}

	if accessClaims, err := parseJWTClaims(tok.AccessToken); err == nil {
		if ts.AccessExpiry.IsZero() {
			ts.AccessExpiry = claimTime(accessClaims, "exp")
		}
		if claims == nil {
			claims = accessClaims
		}
	}
	ts.Identity = claimString(claims, p.cfg.UsernameClaim, "email", "preferred_username", "sub")

	if p.bearer(ts) == "" {
		if p.cfg.TokenType == config.TokenTypeID {
			return nil, noBearerError(`the provider did not return an ID token; request the "openid" scope or set token_type: access_token`)
		}
		return nil, noBearerError("the provider did not return an access token")
	}
	// Keep only the token that is presented to ClickHouse. The other one has
	// served its purpose (identity), and dropping it means less secret
	// material at rest and an entry small enough for OS keyrings, which cap
	// entries at a few kilobytes.
	if p.cfg.TokenType == config.TokenTypeID {
		ts.AccessToken, ts.AccessExpiry = "", time.Time{}
	} else {
		ts.IDToken, ts.IDExpiry = "", time.Time{}
	}
	return ts, nil
}

// checkIDClaims validates the issuer, audience and expiry claims of an ID
// token whose signature cannot be checked.
func (p *OIDCProvider) checkIDClaims(claims map[string]any) error {
	if p.cfg.Issuer != "" && claimString(claims, "iss") != p.cfg.Issuer {
		return fmt.Errorf("issued by %q, expected %q", claimString(claims, "iss"), p.cfg.Issuer)
	}
	audiences, _ := claims["aud"].([]any)
	if aud, ok := claims["aud"].(string); ok {
		audiences = []any{aud}
	}
	if !slices.Contains(audiences, any(p.cfg.ClientID)) {
		return errors.New("not issued for this client (audience mismatch)")
	}
	if exp := claimTime(claims, "exp"); exp.IsZero() || !p.now().Before(exp) {
		return errors.New("expired or without an expiry")
	}
	return nil
}

// remember caches the session in memory and persists it. Failing to persist
// is not fatal: the session still works for this process.
func (p *OIDCProvider) remember(ts *TokenSet) {
	p.current, p.loaded = ts, true
	if err := p.store.Save(p.cfg.CacheKey, ts); err != nil {
		fmt.Fprintf(p.out, "Warning: could not save the login session (%v); you will have to log in again next time.\n", err)
	}
}

// loginNeededError marks failures that only a new interactive login can fix.
type loginNeededError struct{ err error }

func (e *loginNeededError) Error() string { return e.err.Error() }
func (e *loginNeededError) Unwrap() error { return e.err }

// needsLogin reports whether a refresh failure means the session is gone
// (revoked or expired refresh token) rather than a transient problem.
func needsLogin(err error) bool {
	var lne *loginNeededError
	return errors.As(err, &lne)
}

// sanitizeOAuthError rewrites token endpoint errors so that they carry the
// standard error code and description but never the raw response body.
func sanitizeOAuthError(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return err
	}
	msg := re.ErrorCode
	if msg == "" && re.Response != nil {
		msg = re.Response.Status
	}
	if re.ErrorDescription != "" {
		msg += ": " + re.ErrorDescription
	}
	clean := fmt.Errorf("token endpoint error: %s", msg)
	// invalid_grant is how a provider says the grant (here: the refresh
	// token) is expired, revoked or already used (RFC 6749, 5.2). Every
	// other failure, including rate limiting and other 4xx responses, may
	// pass and must not cost the user their session.
	if re.ErrorCode == "invalid_grant" {
		return &loginNeededError{clean}
	}
	return clean
}
