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
	"sync"
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
	// Issuer and Audience are the corresponding claims of a JWT, for
	// diagnostics: a server rejects a token whose audience is not the one it
	// expects. Empty for opaque tokens.
	Issuer   string
	Audience string
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
		return &PasswordProvider{
			Username:    a.Username,
			Password:    NewSecretSource("password", a.Password.Reveal(), a.PasswordCommand, o.Out),
			interactive: o.Interactive,
		}, nil
	case config.AuthJWT:
		return &JWTProvider{Token: a.Token.Reveal(), Command: a.TokenCommand, Interactive: o.Interactive, Err: o.Out}, nil
	case config.AuthOIDC:
		return newOIDCProvider(oidcConfigFrom(r, o), o), nil
	case config.AuthGoogle:
		return newOIDCProvider(googlePreset(oidcConfigFrom(r, o)), o), nil
	}
	return nil, fmt.Errorf("unknown auth type %q", a.Type)
}

func oidcConfigFrom(r *config.Resolved, o Options) OIDCConfig {
	a := r.Auth
	return OIDCConfig{
		Label:         Label(a.Type),
		Profile:       r.Profile,
		CacheKey:      cacheKey(r),
		ClientID:      a.ClientID,
		ClientSecret:  NewSecretSource("client secret", a.ClientSecret.Reveal(), a.ClientSecretCommand, o.Out),
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
	// Password is the password, given directly or by a command.
	Password    *SecretSource
	interactive bool
}

func (p *PasswordProvider) Authenticate(ctx context.Context) (*Credentials, error) {
	return p.authenticate(ctx, p.interactive)
}

func (p *PasswordProvider) authenticate(ctx context.Context, interactive bool) (*Credentials, error) {
	password := ""
	if p.Password != nil {
		var err error
		if password, err = p.Password.Get(ctx, interactive); err != nil {
			return nil, err
		}
	}
	return &Credentials{Username: p.Username, Password: password, Identity: p.Username}, nil
}

// JWTProvider authenticates with a token supplied by the user: either given
// directly, or printed by an external command (like kubeconfig's exec
// credential plugins), which is run again whenever the token it returned has
// expired.
type JWTProvider struct {
	Token   string
	Command config.Command
	// Interactive lets the command use the terminal (for example to ask the
	// user to log in to the identity provider).
	Interactive bool
	// Err receives the command's standard error.
	Err io.Writer

	now    func() time.Time
	mu     sync.Mutex
	cached *Credentials
}

func (p *JWTProvider) Authenticate(ctx context.Context) (*Credentials, error) {
	return p.authenticate(ctx, p.Interactive)
}

func (p *JWTProvider) authenticate(ctx context.Context, interactive bool) (*Credentials, error) {
	if !p.Command.IsSet() {
		creds, expired := p.credentials(p.Token)
		if expired {
			return nil, fmt.Errorf("the JWT token expired at %s; supply a fresh token", creds.Expiry.Local().Format(time.RFC1123))
		}
		return creds, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.cached; c != nil && (c.Expiry.IsZero() || p.clock().Add(expirySkew).Before(c.Expiry)) {
		return c, nil
	}
	token, err := runSecretCommand(ctx, p.Command, interactive, p.Err)
	if err != nil {
		return nil, err
	}
	creds, expired := p.credentials(token)
	if expired {
		return nil, fmt.Errorf("token command %q printed a token that expired at %s", p.Command, creds.Expiry.Local().Format(time.RFC1123))
	}
	p.cached = creds
	return creds, nil
}

func (p *JWTProvider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// credentials wraps a token, reading its claims for display and to notice an
// expired token before the server does. The token may be opaque, in which
// case nothing is known about it.
func (p *JWTProvider) credentials(token string) (creds *Credentials, expired bool) {
	creds = &Credentials{Token: token}
	if claims, err := parseJWTClaims(token); err == nil {
		creds.Identity = claimString(claims, "email", "preferred_username", "sub")
		creds.Expiry = claimTime(claims, "exp")
		creds.Issuer = claimString(claims, "iss")
		creds.Audience = claimString(claims, "aud")
		if list, ok := claims["aud"].([]any); ok {
			parts := make([]string, 0, len(list))
			for _, a := range list {
				if s, ok := a.(string); ok {
					parts = append(parts, s)
				}
			}
			creds.Audience = strings.Join(parts, ", ")
		}
	}
	return creds, !creds.Expiry.IsZero() && !p.clock().Before(creds.Expiry)
}
