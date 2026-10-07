package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeIDP is an in-process OpenID Connect provider: discovery, authorization
// code flow with PKCE enforcement, refresh, device flow and a JWKS endpoint.
// It lets the whole login machinery be tested without a real identity
// provider or a real browser.
type fakeIDP struct {
	t        *testing.T
	srv      *httptest.Server
	key      *rsa.PrivateKey
	signKey  *rsa.PrivateKey // defaults to key; set differently to forge signatures
	clientID string
	email    string
	tokenTTL time.Duration
	now      func() time.Time

	mu            sync.Mutex
	seq           int
	codes         map[string]url.Values // authorization code -> the request that produced it
	refreshTokens map[string]bool
	requests      map[string][]url.Values // endpoint path -> parameters of each request

	// Behaviour switches.
	idTokenAudience string // audience of issued ID tokens; defaults to clientID
	authError       string // returned from /authorize instead of a code
	wrongNonce      bool
	noIDToken       bool
	rotateRefresh   bool
	tokenStatus     int    // when non-zero, /token fails with this status
	tokenErrorCode  string // the OAuth error code sent with tokenStatus
	jwksStatus      int    // when non-zero, /jwks fails with this status
	devicePending   int    // number of "authorization_pending" answers before success
	jwtAccessTokens bool
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{
		t: t, key: key, signKey: key, clientID: "test-client", email: "user@example.com",
		tokenTTL: time.Hour, now: time.Now,
		codes: map[string]url.Values{}, refreshTokens: map[string]bool{}, requests: map[string][]url.Values{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("/authorize", idp.authorize)
	mux.HandleFunc("/token", idp.token)
	mux.HandleFunc("/device", idp.device)
	mux.HandleFunc("/jwks", idp.jwks)
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (i *fakeIDP) issuer() string { return i.srv.URL }

// requestCount returns how many requests an endpoint has received.
func (i *fakeIDP) requestCount(path string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.requests[path])
}

func (i *fakeIDP) lastRequest(path string) url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()
	reqs := i.requests[path]
	if len(reqs) == 0 {
		i.t.Fatalf("no request to %s was made", path)
	}
	return reqs[len(reqs)-1]
}

func (i *fakeIDP) record(r *http.Request) url.Values {
	_ = r.ParseForm()
	i.mu.Lock()
	defer i.mu.Unlock()
	i.requests[r.URL.Path] = append(i.requests[r.URL.Path], r.Form)
	return r.Form
}

func (i *fakeIDP) next(prefix string) string {
	i.seq++
	return fmt.Sprintf("%s-%d", prefix, i.seq)
}

func (i *fakeIDP) discovery(w http.ResponseWriter, r *http.Request) {
	i.record(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                i.issuer(),
		"authorization_endpoint":                i.issuer() + "/authorize",
		"token_endpoint":                        i.issuer() + "/token",
		"device_authorization_endpoint":         i.issuer() + "/device",
		"jwks_uri":                              i.issuer() + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (i *fakeIDP) authorize(w http.ResponseWriter, r *http.Request) {
	q := i.record(r)
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("client_id") != i.clientID || q.Get("response_type") != "code" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	out := url.Values{"state": {q.Get("state")}}
	if i.authError != "" {
		out.Set("error", i.authError)
		out.Set("error_description", "the user said no")
	} else {
		i.mu.Lock()
		code := i.next("code")
		i.codes[code] = q
		i.mu.Unlock()
		out.Set("code", code)
	}
	redirect.RawQuery = out.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (i *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	form := i.record(r)
	if i.tokenStatus != 0 {
		code := i.tokenErrorCode
		if code == "" {
			code = "server_error"
		}
		writeJSON(w, i.tokenStatus, map[string]any{"error": code})
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	nonce := ""
	switch form.Get("grant_type") {
	case "authorization_code":
		req, ok := i.codes[form.Get("code")]
		delete(i.codes, form.Get("code")) // single use
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "unknown code"})
			return
		}
		// PKCE (RFC 7636): the verifier must hash to the challenge.
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if req.Get("code_challenge_method") != "S256" || base64.RawURLEncoding.EncodeToString(sum[:]) != req.Get("code_challenge") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "PKCE verification failed"})
			return
		}
		if form.Get("redirect_uri") != req.Get("redirect_uri") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "redirect_uri mismatch"})
			return
		}
		nonce = req.Get("nonce")
	case "refresh_token":
		if !i.refreshTokens[form.Get("refresh_token")] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "refresh token revoked"})
			return
		}
		if i.rotateRefresh {
			delete(i.refreshTokens, form.Get("refresh_token"))
		}
	case "urn:ietf:params:oauth:grant-type:device_code":
		if i.devicePending > 0 {
			i.devicePending--
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "authorization_pending"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}

	exp := i.now().Add(i.tokenTTL)
	resp := map[string]any{
		"access_token": i.next("opaque-access-token"),
		"token_type":   "Bearer",
		"expires_in":   int(i.tokenTTL.Seconds()),
	}
	if i.jwtAccessTokens {
		resp["access_token"] = i.sign(map[string]any{
			"iss": i.issuer(), "aud": "clickhouse", "sub": "user-1", "preferred_username": "jdoe", "exp": exp.Unix(),
		})
	}
	if !i.noIDToken {
		if i.wrongNonce {
			nonce = "not-the-nonce-you-sent"
		}
		aud := i.clientID
		if i.idTokenAudience != "" {
			aud = i.idTokenAudience
		}
		claims := map[string]any{
			"iss": i.issuer(), "aud": aud, "sub": "user-1", "email": i.email,
			"iat": i.now().Unix(), "exp": exp.Unix(),
		}
		if nonce != "" {
			claims["nonce"] = nonce
		}
		resp["id_token"] = i.sign(claims)
	}
	if form.Get("grant_type") != "refresh_token" || i.rotateRefresh {
		rt := i.next("refresh-token")
		i.refreshTokens[rt] = true
		resp["refresh_token"] = rt
	}
	writeJSON(w, http.StatusOK, resp)
}

func (i *fakeIDP) device(w http.ResponseWriter, r *http.Request) {
	i.record(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      "device-code-1",
		"user_code":        "ABCD-EFGH",
		"verification_uri": i.issuer() + "/activate",
		"expires_in":       300,
		"interval":         1,
	})
}

func (i *fakeIDP) jwks(w http.ResponseWriter, r *http.Request) {
	i.record(r)
	if i.jwksStatus != 0 {
		http.Error(w, "keys unavailable", i.jwksStatus)
		return
	}
	pub := i.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": "test-key", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// revokeRefreshTokens makes every refresh token issued so far invalid.
func (i *fakeIDP) revokeRefreshTokens() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.refreshTokens = map[string]bool{}
}

// sign produces an RS256 JWT.
func (i *fakeIDP) sign(claims map[string]any) string {
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			i.t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test-key"}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.signKey, crypto.SHA256, digest[:])
	if err != nil {
		i.t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// unsignedJWT builds a token with the given claims and a junk signature, for
// tests that only decode claims.
func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".c2ln"
}
