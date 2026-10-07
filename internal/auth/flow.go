package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
)

// loginTimeout bounds how long an interactive login may take.
const loginTimeout = 5 * time.Minute

// defaultRedirectURI listens on a random free loopback port. Providers that
// require an exactly registered redirect URI need redirect_uri configured.
const defaultRedirectURI = "http://127.0.0.1:0/callback"

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// requestOptions are the extra parameters sent with authorization requests.
func (p *OIDCProvider) requestOptions() []oauth2.AuthCodeOption {
	var opts []oauth2.AuthCodeOption
	if p.cfg.Audience != "" {
		opts = append(opts, oauth2.SetAuthURLParam("audience", p.cfg.Audience))
	}
	for k, v := range p.cfg.AuthParams {
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	return opts
}

// browserFlow runs the authorization code flow with PKCE: it serves the
// redirect URI on a loopback listener, sends the user to the provider and
// exchanges the returned code. The nonce it sent is returned for ID token
// validation.
func (p *OIDCProvider) browserFlow(ctx context.Context, conf *oauth2.Config) (*oauth2.Token, string, error) {
	if conf.Endpoint.AuthURL == "" {
		return nil, "", errors.New("the provider has no authorization endpoint; set authorization_endpoint or use the device flow")
	}
	redirect := p.cfg.RedirectURI
	if redirect == "" {
		redirect = defaultRedirectURI
	}
	u, err := url.Parse(redirect)
	if err != nil {
		return nil, "", fmt.Errorf("invalid redirect URI: %w", err)
	}
	port := u.Port()
	if port == "" {
		port = "0"
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, "", fmt.Errorf("cannot listen for the OAuth callback on %s: %w", net.JoinHostPort(u.Hostname(), port), err)
	}
	defer listener.Close()
	u.Host = net.JoinHostPort(u.Hostname(), fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
	if u.Path == "" {
		u.Path = "/"
	}
	conf.RedirectURL = u.String()

	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()

	type callback struct {
		code string
		err  error
	}
	result := make(chan callback, 1)
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != u.Path {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			// Requests without our state are not part of this login (CSRF or
			// a stray request); reject them and keep waiting.
			if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
				http.Error(w, "Invalid OAuth state.", http.StatusBadRequest)
				return
			}
			var cb callback
			switch {
			case q.Get("error") != "":
				cb.err = fmt.Errorf("the provider rejected the login: %s", describeOAuthError(q))
			case q.Get("code") == "":
				cb.err = errors.New("the provider returned no authorization code")
			default:
				cb.code = q.Get("code")
			}
			writeCallbackPage(w, cb.err)
			select {
			case result <- cb:
			default: // a duplicate callback; the first one wins
			}
		}),
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	opts := append(p.requestOptions(), oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce))
	authURL := conf.AuthCodeURL(state, opts...)

	fmt.Fprintf(p.out, "Opening browser for %s authentication...\n", p.providerName())
	if err := p.open(authURL); err != nil {
		fmt.Fprintf(p.out, "Open the following URL in your browser:\n%s\n", authURL)
	} else {
		fmt.Fprintf(p.out, "If the browser did not open, visit:\n%s\n", authURL)
	}
	fmt.Fprintln(p.out, "Waiting for authentication...")

	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	select {
	case cb := <-result:
		if cb.err != nil {
			return nil, "", cb.err
		}
		tok, err := conf.Exchange(p.httpContext(ctx), cb.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, "", fmt.Errorf("exchange authorization code: %w", sanitizeOAuthError(err))
		}
		return tok, nonce, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, "", errors.New("timed out waiting for the browser login to complete")
		}
		return nil, "", ctx.Err()
	}
}

// deviceFlow runs the device authorization grant (RFC 8628), for machines
// whose browser cannot reach a loopback listener on this host.
func (p *OIDCProvider) deviceFlow(ctx context.Context, conf *oauth2.Config) (*oauth2.Token, error) {
	if conf.Endpoint.DeviceAuthURL == "" {
		return nil, errors.New("the provider has no device authorization endpoint; set device_endpoint or use the browser flow")
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	da, err := conf.DeviceAuth(p.httpContext(ctx), p.requestOptions()...)
	if err != nil {
		return nil, fmt.Errorf("start device login: %w", sanitizeOAuthError(err))
	}
	fmt.Fprintf(p.out, "To authenticate with %s, open:\n  %s\nand enter the code: %s\n", p.providerName(), da.VerificationURI, da.UserCode)
	if da.VerificationURIComplete != "" {
		_ = p.open(da.VerificationURIComplete)
	}
	fmt.Fprintln(p.out, "Waiting for authentication...")

	tok, err := conf.DeviceAccessToken(p.httpContext(ctx), da)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("timed out waiting for the device login to complete")
		}
		return nil, fmt.Errorf("device login: %w", sanitizeOAuthError(err))
	}
	return tok, nil
}

// open hands a URL to the system's URL opener. The URL comes from the
// identity provider's metadata or responses, so it is restricted to web
// addresses: an opener will happily launch other schemes.
func (p *OIDCProvider) open(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("not a web address")
	}
	return p.openBrowser(rawURL)
}

// providerName is the provider's name as used mid-sentence ("Google", "OIDC").
func (p *OIDCProvider) providerName() string {
	if p.cfg.Label == "Google OAuth" {
		return "Google"
	}
	return p.cfg.Label
}

func describeOAuthError(q url.Values) string {
	if desc := q.Get("error_description"); desc != "" {
		return q.Get("error") + ": " + desc
	}
	return q.Get("error")
}

func writeCallbackPage(w http.ResponseWriter, err error) {
	title, body, status := "Authentication complete", "You can close this tab and return to the terminal.", http.StatusOK
	if err != nil {
		title, body, status = "Authentication failed", err.Error(), http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>chcli</title></head>`+
		`<body style="font-family:system-ui,sans-serif;text-align:center;margin-top:15vh">`+
		`<h2>%s</h2><p>%s</p></body></html>`, html.EscapeString(title), html.EscapeString(body))
}
