// Package auth acquires the credentials chcli presents to ClickHouse.
//
// The rest of the program only sees the Provider interface and the
// Credentials it returns; how a token was obtained (static value, cached
// OAuth session, browser login, refresh) stays inside this package.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nenych/chcli/internal/config"
)

// Credentials is what a ClickHouse connection needs to authenticate. Either
// Token is set (JWT / bearer authentication) or Username and Password are.
type Credentials struct {
	Username string
	Password string
	Token    string

	// Identity is a display name for the authenticated principal, for
	// example the e-mail claim of an OIDC token.
	Identity string
	// Expiry is when Token stops being valid; zero if unknown or not applicable.
	Expiry time.Time
}

// UsesToken reports whether these are token credentials.
func (c *Credentials) UsesToken() bool { return c.Token != "" }

// Provider yields credentials, refreshing or re-acquiring them when needed.
// Authenticate is called before every connection attempt and query, so
// implementations must be cheap when nothing has to be done.
type Provider interface {
	Authenticate(ctx context.Context) (*Credentials, error)
}

// SessionProvider is implemented by providers that keep a login session
// (cached tokens) which can be inspected and ended.
type SessionProvider interface {
	Provider
	// Login runs the interactive login flow even if cached tokens exist.
	Login(ctx context.Context) (*Credentials, error)
	// Logout forgets all cached tokens.
	Logout() error
	// Status describes the cached session without contacting the provider.
	Status() (Status, error)
}

// Status describes a cached login session. It never contains token material.
type Status struct {
	LoggedIn    bool
	Identity    string
	Expiry      time.Time
	Refreshable bool
	Storage     string
}

// LoginRequiredError is returned when OAuth credentials are needed but the
// process is not allowed to start an interactive login.
type LoginRequiredError struct {
	Profile string
}

func (e *LoginRequiredError) Error() string {
	if e.Profile == "" {
		return "OAuth credentials are not available.\nRun:\n  chcli auth login\n" +
			"with the same connection flags from an interactive terminal first."
	}
	return fmt.Sprintf("OAuth credentials for profile %q are not available.\nRun:\n  chcli auth login --profile %s\n"+
		"from an interactive terminal first.", e.Profile, e.Profile)
}

// Label returns the human-readable name of an authentication type.
func Label(authType string) string {
	switch authType {
	case config.AuthPassword:
		return "Password"
	case config.AuthJWT:
		return "JWT"
	case config.AuthGoogle:
		return "Google OAuth"
	case config.AuthOIDC:
		return "OIDC"
	}
	return authType
}

// Options are the environment-dependent collaborators of a provider.
type Options struct {
	// Interactive permits opening a browser and waiting for the user.
	Interactive bool
	// Out receives messages for the user during login.
	Out io.Writer
	// Store persists OAuth tokens between runs.
	Store TokenStore
	// OpenBrowser opens a URL in the user's browser.
	OpenBrowser func(url string) error
	// HTTPClient is used for all requests to the identity provider.
	HTTPClient *http.Client
}

// NewProvider selects and builds the provider for a resolved configuration.
func NewProvider(r *config.Resolved, o Options) (Provider, error) {
	a := r.Auth
	switch a.Type {
	case config.AuthPassword:
		return &PasswordProvider{Username: a.Username, Password: a.Password.Reveal()}, nil
	case config.AuthJWT:
		return &JWTProvider{Token: a.Token.Reveal()}, nil
	case config.AuthOIDC:
		return newOIDCProvider(oidcConfigFrom(r), o), nil
	case config.AuthGoogle:
		return newOIDCProvider(googlePreset(oidcConfigFrom(r)), o), nil
	}
	return nil, fmt.Errorf("unknown auth type %q", a.Type)
}

func oidcConfigFrom(r *config.Resolved) OIDCConfig {
	a := r.Auth
	return OIDCConfig{
		Label:         Label(a.Type),
		Profile:       r.Profile,
		CacheKey:      cacheKey(r),
		ClientID:      a.ClientID,
		ClientSecret:  a.ClientSecret.Reveal(),
		Issuer:        a.Issuer,
		AuthURL:       a.AuthorizationEndpoint,
		TokenURL:      a.TokenEndpoint,
		DeviceURL:     a.DeviceEndpoint,
		Audience:      a.Audience,
		Scopes:        a.Scopes,
		UsernameClaim: a.UsernameClaim,
		RedirectURI:   a.RedirectURI,
		Flow:          a.Flow,
		TokenType:     a.TokenType,
	}
}

// googlePreset layers Google's quirks on top of the generic OIDC provider:
// offline access has to be requested explicitly to receive a refresh token,
// and Google has no "audience" request parameter (the ID token audience is
// always the client ID).
func googlePreset(c OIDCConfig) OIDCConfig {
	c.AuthParams = map[string]string{"access_type": "offline", "prompt": "consent"}
	c.Audience = ""
	return c
}

// cacheKey names the token cache entry of a connection. Profiles get their
// own entry; ad-hoc connections share one per identity provider and client.
func cacheKey(r *config.Resolved) string {
	if r.Profile != "" {
		return "profile:" + r.Profile
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{r.Auth.Issuer, r.Auth.TokenEndpoint, r.Auth.ClientID}, "\n")))
	return "direct:" + hex.EncodeToString(sum[:8])
}

// PasswordProvider authenticates with a ClickHouse user name and password.
type PasswordProvider struct {
	Username string
	Password string
}

func (p *PasswordProvider) Authenticate(context.Context) (*Credentials, error) {
	return &Credentials{Username: p.Username, Password: p.Password, Identity: p.Username}, nil
}

// JWTProvider authenticates with a token supplied by the user.
type JWTProvider struct {
	Token string
	now   func() time.Time
}

func (p *JWTProvider) Authenticate(context.Context) (*Credentials, error) {
	creds := &Credentials{Token: p.Token}
	// The token may be opaque; claims are only used for display and for a
	// friendlier error than the server's when it has already expired.
	if claims, err := parseJWTClaims(p.Token); err == nil {
		creds.Identity = claimString(claims, "email", "preferred_username", "sub")
		creds.Expiry = claimTime(claims, "exp")
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	if !creds.Expiry.IsZero() && !now().Before(creds.Expiry) {
		return nil, fmt.Errorf("the JWT token expired at %s; supply a fresh token", creds.Expiry.Local().Format(time.RFC1123))
	}
	return creds, nil
}
