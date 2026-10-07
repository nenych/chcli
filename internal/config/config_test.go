package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const sampleConfig = `
connections:
  production:
    host: clickhouse.example.com
    port: 9440
    database: default
    secure: true
    auth:
      type: google
      client_id: abc.apps.googleusercontent.com
      client_secret: file-secret
      issuer: https://accounts.google.com
      audience: abc.apps.googleusercontent.com
      scopes:
        - openid
        - email
      username_claim: email
      redirect_uri: http://127.0.0.1:8765/callback
  staging:
    host: clickhouse-staging.example.com
    secure: true
    auth:
      type: oidc
      issuer: https://auth.example.com
      client_id: clickhouse-cli
      audience: clickhouse
  local:
    host: localhost
    port: 9000
    auth:
      type: password
      username: default
      password: example
history:
  enabled: true
  max_entries: 500
output:
  format: vertical
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadSample(t *testing.T) *File {
	t.Helper()
	f, err := Load(writeConfig(t, sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// source builds a config layer from a map keyed by setting key.
func source(values map[string]string) Source {
	return Source{
		Lookup: func(key string) (string, bool) { v, ok := values[key]; return v, ok },
		Name:   func(key string) string { return "--" + key },
	}
}

var none = Source{}

func TestLoad(t *testing.T) {
	f := loadSample(t)
	if got, want := f.ProfileNames(), []string{"local", "production", "staging"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProfileNames = %v, want %v", got, want)
	}
	p := f.Connections["production"]
	if p.Host != "clickhouse.example.com" || p.Port != 9440 || p.Secure == nil || !*p.Secure {
		t.Errorf("production profile parsed wrong: %+v", p)
	}
	if p.Auth.ClientSecret.Reveal() != "file-secret" || !reflect.DeepEqual(p.Auth.Scopes, []string{"openid", "email"}) {
		t.Errorf("production auth parsed wrong: %#v", p.Auth)
	}
	if !f.History.IsEnabled() || f.History.Limit() != 500 || f.Output.Format != "vertical" {
		t.Errorf("history/output parsed wrong: %+v %+v", f.History, f.Output)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || len(f.Connections) != 0 {
		t.Fatalf("Load(missing) = %+v, %v", f, err)
	}
	if !f.History.IsEnabled() || f.History.Limit() != DefaultHistoryEntries {
		t.Errorf("history defaults wrong: %+v", f.History)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load(writeConfig(t, "connections:\n  x:\n    hots: typo\n"))
	if err == nil || !strings.Contains(err.Error(), "hots") {
		t.Fatalf("expected an error naming the unknown field, got %v", err)
	}
}

func TestLoadWarnsAboutReadableSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	path := writeConfig(t, sampleConfig)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Warnings) != 1 || !strings.Contains(f.Warnings[0], "chmod 600") {
		t.Errorf("Warnings = %v", f.Warnings)
	}
	if f, _ := Load(writeConfig(t, sampleConfig)); len(f.Warnings) != 0 {
		t.Errorf("0600 file must not warn: %v", f.Warnings)
	}
}

func TestResolvePrecedence(t *testing.T) {
	f := loadSample(t)
	env := EnvSource(func(name string) (string, bool) {
		v, ok := map[string]string{
			"CHCLI_DATABASE":            "from_env",
			"CHCLI_HOST":                "env.example.com",
			"CHCLI_OAUTH_CLIENT_SECRET": "env-secret",
			"CHCLI_PORT":                "", // empty counts as unset
		}[name]
		return v, ok
	})
	flags := source(map[string]string{KeyDatabase: "chronicle"})

	r, err := Resolve(f, "production", env, flags)
	if err != nil {
		t.Fatal(err)
	}
	if r.Database != "chronicle" {
		t.Errorf("flag must beat env and profile: database = %q", r.Database)
	}
	if r.Host != "env.example.com" {
		t.Errorf("env must beat profile: host = %q", r.Host)
	}
	if r.Auth.ClientSecret.Reveal() != "env-secret" {
		t.Errorf("env secret must beat the profile's")
	}
	if r.Port != 9440 || !r.Secure || r.Auth.Type != AuthGoogle || r.Profile != "production" {
		t.Errorf("profile values lost: %+v", r)
	}
}

// The example from the specification: a profile plus one override.
func TestResolveOverrideDoesNotModifyProfile(t *testing.T) {
	f := loadSample(t)
	before := fmt.Sprintf("%#v", f.Connections["production"])

	r, err := Resolve(f, "production", none, source(map[string]string{
		KeyDatabase: "chronicle", KeyScope: "openid,profile", KeyHost: "another.example.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Host != "another.example.com" || r.Port != 9440 || r.Database != "chronicle" || !r.Secure || r.Auth.Type != AuthGoogle {
		t.Errorf("effective config wrong: %+v", r)
	}
	if !reflect.DeepEqual(r.Auth.Scopes, []string{"openid", "profile"}) {
		t.Errorf("scopes = %v", r.Auth.Scopes)
	}
	if after := fmt.Sprintf("%#v", f.Connections["production"]); after != before {
		t.Errorf("stored profile was modified:\nbefore %s\nafter  %s", before, after)
	}
	if got := f.Connections["production"].Auth.Scopes; !reflect.DeepEqual(got, []string{"openid", "email"}) {
		t.Errorf("stored scopes were modified: %v", got)
	}
}

func TestResolveAuthTypeOverrideDropsProfileAuth(t *testing.T) {
	f := loadSample(t)
	r, err := Resolve(f, "production", none, source(map[string]string{KeyAuth: "password", KeyUser: "admin"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Auth{Type: AuthPassword, Username: "admin"}
	if !reflect.DeepEqual(r.Auth, want) {
		t.Errorf("auth = %#v, want %#v", r.Auth, want)
	}
}

func TestResolveDirectConnections(t *testing.T) {
	tests := []struct {
		name  string
		flags map[string]string
		check func(t *testing.T, r *Resolved)
	}{
		{"password defaults", map[string]string{KeyHost: "localhost"}, func(t *testing.T, r *Resolved) {
			if r.Port != 9000 || r.Secure || r.Database != "default" || r.Protocol != ProtocolNative ||
				r.Auth.Type != AuthPassword || r.Auth.Username != "default" {
				t.Errorf("%+v", r)
			}
		}},
		{"secure picks the TLS port", map[string]string{KeyHost: "h", KeySecure: "true"}, func(t *testing.T, r *Resolved) {
			if r.Port != 9440 || !r.Secure {
				t.Errorf("%+v", r)
			}
		}},
		{"TLS port implies secure", map[string]string{KeyHost: "h", KeyPort: "9440"}, func(t *testing.T, r *Resolved) {
			if !r.Secure {
				t.Errorf("%+v", r)
			}
		}},
		{"explicit secure=false wins over port", map[string]string{KeyHost: "h", KeyPort: "9440", KeySecure: "false"}, func(t *testing.T, r *Resolved) {
			if r.Secure {
				t.Errorf("%+v", r)
			}
		}},
		{"http ports", map[string]string{KeyHost: "h", KeyProtocol: "http", KeySecure: "true"}, func(t *testing.T, r *Resolved) {
			if r.Port != 8443 {
				t.Errorf("%+v", r)
			}
		}},
		{"google shortcut", map[string]string{KeyHost: "h", KeyGoogleOAuth: "true", KeyClientID: "x.apps.googleusercontent.com"}, func(t *testing.T, r *Resolved) {
			a := r.Auth
			if a.Type != AuthGoogle || a.Issuer != GoogleIssuer || a.UsernameClaim != "email" || a.TokenType != TokenTypeAccess ||
				a.Flow != FlowBrowser || !reflect.DeepEqual(a.Scopes, []string{"openid", "email", "profile"}) {
				t.Errorf("%#v", a)
			}
		}},
		{"generic oidc", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyIssuer: "https://auth.example.com", KeyClientID: "cli", KeyAudience: "clickhouse"}, func(t *testing.T, r *Resolved) {
			a := r.Auth
			if a.Type != AuthOIDC || a.TokenType != TokenTypeAccess || a.Audience != "clickhouse" {
				t.Errorf("%#v", a)
			}
		}},
		{"jwt inferred from token", map[string]string{KeyHost: "h", KeyJWTToken: "tok"}, func(t *testing.T, r *Resolved) {
			if r.Auth.Type != AuthJWT || r.Auth.Token.Reveal() != "tok" {
				t.Errorf("%#v", r.Auth)
			}
		}},
		{"oidc inferred from client id", map[string]string{KeyHost: "h", KeyClientID: "c", KeyIssuer: "https://idp.example.com"}, func(t *testing.T, r *Resolved) {
			if r.Auth.Type != AuthOIDC {
				t.Errorf("%#v", r.Auth)
			}
		}},
		{"google inferred from issuer", map[string]string{KeyHost: "h", KeyClientID: "c", KeyIssuer: GoogleIssuer}, func(t *testing.T, r *Resolved) {
			if r.Auth.Type != AuthGoogle {
				t.Errorf("%#v", r.Auth)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Resolve(&File{}, "", none, source(tt.flags))
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, r)
		})
	}
}

// A bearer token must not travel over the network unencrypted unless the
// user says so: TLS is the default for token authentication to remote hosts.
func TestTokenAuthDefaultsToTLS(t *testing.T) {
	jwt := func(extra map[string]string) map[string]string {
		m := map[string]string{KeyAuth: "jwt", KeyJWTToken: "tok"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	tests := []struct {
		name       string
		flags      map[string]string
		wantSecure bool
		wantPort   int
		wantErr    string
	}{
		{"remote host", jwt(map[string]string{KeyHost: "ch.example.com"}), true, 9440, ""},
		{"remote host over http", jwt(map[string]string{KeyHost: "ch.example.com", KeyProtocol: "http"}), true, 8443, ""},
		{"remote host, custom port", jwt(map[string]string{KeyHost: "ch.example.com", KeyPort: "9443"}), true, 9443, ""},
		{"port 443 is TLS", map[string]string{KeyHost: "ch.example.com", KeyPort: "443", KeyProtocol: "http"}, true, 443, ""},
		{"remote host, plaintext port", jwt(map[string]string{KeyHost: "ch.example.com", KeyPort: "9000"}), false, 0, "without TLS"},
		{"explicit opt-out", jwt(map[string]string{KeyHost: "ch.example.com", KeyPort: "9000", KeySecure: "false"}), false, 9000, ""},
		{"loopback name", jwt(map[string]string{KeyHost: "localhost"}), false, 9000, ""},
		{"loopback address", jwt(map[string]string{KeyHost: "127.0.0.1"}), false, 9000, ""},
		{"oidc to a remote host", map[string]string{KeyHost: "ch.example.com", KeyAuth: "oidc", KeyIssuer: "https://idp.example.com", KeyClientID: "c"}, true, 9440, ""},
		{"passwords keep ClickHouse's default", map[string]string{KeyHost: "ch.example.com"}, false, 9000, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Resolve(&File{}, "", none, source(tt.flags))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Secure != tt.wantSecure || r.Port != tt.wantPort {
				t.Errorf("secure = %v, port = %d; want %v, %d", r.Secure, r.Port, tt.wantSecure, tt.wantPort)
			}
		})
	}

	// A profile that says secure: false has made the choice explicitly.
	off := false
	file := &File{Connections: map[string]Profile{"p": {Host: "ch.example.com", Port: 9000, Secure: &off, Auth: Auth{Type: AuthJWT, Token: "t"}}}}
	if r, err := Resolve(file, "p", none, none); err != nil || r.Secure {
		t.Errorf("explicit secure=false in a profile: %+v, %v", r, err)
	}
}

// Credential flags that the selected authentication type would ignore are a
// mistake to report, not to swallow.
func TestCredentialFlagsMustFitAuthType(t *testing.T) {
	file := &File{Connections: map[string]Profile{
		"pw":  {Host: "localhost", Auth: Auth{Type: AuthPassword, Username: "u"}},
		"sso": {Host: "localhost", Auth: Auth{Type: AuthOIDC, Issuer: "https://idp.example.com", ClientID: "c"}},
	}}
	conflicts := []struct {
		profile string
		flags   map[string]string
		want    string
	}{
		{"pw", map[string]string{KeyJWTToken: "tok"}, `--jwt-token does not apply to "password" authentication; add --auth jwt`},
		{"pw", map[string]string{KeyClientID: "c"}, `--oauth-client-id does not apply to "password" authentication`},
		{"sso", map[string]string{KeyPassword: "p"}, `--password does not apply to "oidc" authentication`},
		{"sso", map[string]string{KeyUser: "u"}, `--user does not apply to "oidc" authentication`},
	}
	for _, c := range conflicts {
		if _, err := Resolve(file, c.profile, none, source(c.flags)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("profile %s with %v: error = %v, want %q", c.profile, c.flags, err, c.want)
		}
	}
	// Switching the type together with its credentials is fine.
	if _, err := Resolve(file, "pw", none, source(map[string]string{KeyAuth: "jwt", KeyJWTToken: "tok"})); err != nil {
		t.Errorf("explicit switch: %v", err)
	}
	// Environment variables are often set shell-wide and are simply ignored
	// when they do not apply.
	env := EnvSource(func(name string) (string, bool) {
		v, ok := map[string]string{"CHCLI_PASSWORD": "p", "CHCLI_JWT_TOKEN": "t"}[name]
		return v, ok
	})
	if r, err := Resolve(file, "sso", env, none); err != nil || r.Auth.Type != AuthOIDC {
		t.Errorf("unrelated environment variables: %+v, %v", r, err)
	}
}

func TestResolveValidation(t *testing.T) {
	file := &File{Connections: map[string]Profile{
		"production": {Host: "h", Auth: Auth{Type: AuthGoogle}},
		"nohost":     {},
	}}
	tests := []struct {
		name    string
		profile string
		flags   map[string]string
		want    string // substring of the error
	}{
		{"unknown profile", "nope", nil, `profile "nope" not found (available: nohost, production)`},
		{"missing client id", "production", nil, `profile "production": auth type "google" requires auth.client_id`},
		{"missing host", "nohost", nil, `profile "nohost": no host configured`},
		{"no host at all", "", nil, "connection: no host configured"},
		{"bad port", "", map[string]string{KeyHost: "h", KeyPort: "abc"}, `invalid --port value "abc"`},
		{"port out of range", "", map[string]string{KeyHost: "h", KeyPort: "70000"}, "invalid port 70000"},
		{"bad bool", "", map[string]string{KeyHost: "h", KeySecure: "maybe"}, `invalid --secure value "maybe"`},
		{"secure on plaintext port", "", map[string]string{KeyHost: "h", KeySecure: "true", KeyPort: "9000"}, "secure=true but port 9000"},
		{"tls options without tls", "", map[string]string{KeyHost: "h", KeyInsecureSkipVerify: "true"}, "secure=false"},
		{"unknown auth type", "", map[string]string{KeyHost: "h", KeyAuth: "kerberos"}, `unknown auth type "kerberos"`},
		{"unknown protocol", "", map[string]string{KeyHost: "h", KeyProtocol: "grpc"}, `unknown protocol "grpc"`},
		{"jwt without token", "", map[string]string{KeyHost: "h", KeyAuth: "jwt"}, `auth type "jwt" requires a token`},
		{"oidc without issuer", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c"}, "requires auth.issuer"},
		{"oidc bad issuer", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c", KeyIssuer: "not a url"}, "is not a valid URL"},
		{"plain http issuer", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c", KeyIssuer: "http://idp.example.com"}, "must use https"},
		{"plain http token endpoint", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c", KeyIssuer: "https://i", KeyTokenEndpoint: "http://idp.example.com/token"}, "must use https"},
		{"non-loopback redirect", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c", KeyIssuer: "https://i", KeyRedirectURI: "http://evil.example.com/cb"}, "loopback"},
		{"https redirect", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c", KeyIssuer: "https://i", KeyRedirectURI: "https://127.0.0.1/cb"}, "http:// loopback URL"},
		{"bad flow", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c", KeyIssuer: "https://i", KeyFlow: "implicit"}, `unknown auth.flow "implicit"`},
		{"bad token type", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c", KeyIssuer: "https://i", KeyTokenType: "refresh"}, "unknown auth.token_type"},
		{"google shortcut conflict", "", map[string]string{KeyHost: "h", KeyGoogleOAuth: "true", KeyAuth: "oidc"}, "--google-oauth conflicts with --auth=oidc"},
		{"device flow needs endpoint", "", map[string]string{KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c", KeyTokenEndpoint: "https://i/token", KeyFlow: "device"}, "requires auth.device_endpoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolve(file, tt.profile, none, source(tt.flags))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestManualEndpointsWithoutIssuer(t *testing.T) {
	_, err := Resolve(&File{}, "", none, source(map[string]string{
		KeyHost: "h", KeyAuth: "oidc", KeyClientID: "c",
		KeyAuthEndpoint: "https://idp.example.com/authorize", KeyTokenEndpoint: "https://idp.example.com/token",
	}))
	if err != nil {
		t.Errorf("explicit endpoints must be enough without an issuer: %v", err)
	}
}

func TestLoopbackIdentityProviderMayUseHTTP(t *testing.T) {
	for _, issuer := range []string{"http://localhost:8080/realms/dev", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		_, err := Resolve(&File{}, "", none, source(map[string]string{KeyHost: "localhost", KeyAuth: "oidc", KeyClientID: "c", KeyIssuer: issuer}))
		if err != nil {
			t.Errorf("issuer %s: %v", issuer, err)
		}
	}
}

func TestEnvName(t *testing.T) {
	for key, want := range map[string]string{
		KeyPassword: "CHCLI_PASSWORD", KeyJWTToken: "CHCLI_JWT_TOKEN", KeyClientSecret: "CHCLI_OAUTH_CLIENT_SECRET",
		KeyInsecureSkipVerify: "CHCLI_INSECURE_SKIP_VERIFY",
	} {
		if got := EnvName(key); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", key, got, want)
		}
	}
}

// Secrets must not leak through any of the usual ways a value gets printed.
func TestSecretRedaction(t *testing.T) {
	const secret = "hunter2-very-secret"
	r, err := Resolve(&File{}, "", none, source(map[string]string{
		KeyHost: "h", KeyAuth: "oidc", KeyIssuer: "https://i", KeyClientID: "public-id", KeyClientSecret: secret,
	}))
	if err != nil {
		t.Fatal(err)
	}
	pw, err := Resolve(&File{}, "", none, source(map[string]string{KeyHost: "h", KeyPassword: secret}))
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := Resolve(&File{}, "", none, source(map[string]string{KeyHost: "h", KeyJWTToken: secret}))
	if err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, res := range []*Resolved{r, pw, jwt} {
		yamlOut, err := yaml.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		logger.Debug("resolved", "config", res, "value", *res, "auth", res.Auth, "secret", res.Auth.ClientSecret)
		outputs := map[string]string{
			"yaml": string(yamlOut),
			"%v":   fmt.Sprintf("%v", *res),
			"%+v":  fmt.Sprintf("%+v", res),
			"%#v":  fmt.Sprintf("%#v", res.Auth),
			"%s":   fmt.Sprintf("%s %s %s", res.Auth.Password, res.Auth.Token, res.Auth.ClientSecret),
			"slog": logged.String(),
		}
		for how, out := range outputs {
			if strings.Contains(out, secret) {
				t.Errorf("secret leaked via %s: %s", how, out)
			}
		}
		if !strings.Contains(string(yamlOut), Redacted) {
			t.Errorf("yaml output should show the redaction marker:\n%s", yamlOut)
		}
	}
	if !strings.Contains(fmt.Sprintf("%+v", r), "public-id") {
		t.Error("non-secret values must stay visible")
	}
	if s := Secret(""); s.String() != "" {
		t.Error("an empty secret must print as empty, not as redacted")
	}
}

func TestDefaultPathHonoursEnvironment(t *testing.T) {
	t.Setenv("CHCLI_CONFIG", "/custom/config.yaml")
	if got := DefaultPath(); got != "/custom/config.yaml" {
		t.Errorf("DefaultPath = %q", got)
	}
	if runtime.GOOS == "windows" {
		return
	}
	t.Setenv("CHCLI_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, want := DefaultPath(), filepath.Join("/xdg", "chcli", "config.yaml"); got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got, want := DefaultPath(), "/home/u/.config/chcli/config.yaml"; got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if got, want := StateDir(), "/home/u/.local/state/chcli"; got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
}

func TestTokenCommandForms(t *testing.T) {
	f, err := Load(writeConfig(t, `
connections:
  shell:
    host: localhost
    auth:
      type: jwt
      token_command: gcloud auth print-identity-token --audiences=abc
  argv:
    host: localhost
    auth:
      type: jwt
      token_command: [gcloud, auth, print-identity-token, --audiences=abc]
  inferred:
    host: localhost
    auth:
      token_command: ./get-token
`))
	if err != nil {
		t.Fatal(err)
	}
	shell, err := Resolve(f, "shell", none, none)
	if err != nil || shell.Auth.TokenCommand.Shell != "gcloud auth print-identity-token --audiences=abc" || len(shell.Auth.TokenCommand.Argv) != 0 {
		t.Errorf("shell form = %+v, %v", shell.Auth.TokenCommand, err)
	}
	argv, err := Resolve(f, "argv", none, none)
	if err != nil || !reflect.DeepEqual(argv.Auth.TokenCommand.Argv, []string{"gcloud", "auth", "print-identity-token", "--audiences=abc"}) || argv.Auth.TokenCommand.Shell != "" {
		t.Errorf("argv form = %+v, %v", argv.Auth.TokenCommand, err)
	}
	inferred, err := Resolve(f, "inferred", none, none)
	if err != nil || inferred.Auth.Type != AuthJWT {
		t.Errorf("a token command alone implies jwt: %+v, %v", inferred.Auth, err)
	}
	// config show renders the command (it is not a secret) in the form it was given.
	out, err := yaml.Marshal(argv)
	if err != nil || !regexp.MustCompile(`token_command:\n\s+- gcloud\n\s+- auth\n`).Match(out) {
		t.Errorf("yaml = %s, %v", out, err)
	}
	out, _ = yaml.Marshal(shell)
	if !strings.Contains(string(out), "token_command: gcloud auth print-identity-token --audiences=abc") {
		t.Errorf("yaml = %s", out)
	}

	// Flags and environment take the shell form.
	r, err := Resolve(&File{}, "", none, source(map[string]string{KeyHost: "localhost", CommandKey(KeyJWTToken): "cat ~/.token"}))
	if err != nil || r.Auth.Type != AuthJWT || r.Auth.TokenCommand.Shell != "cat ~/.token" {
		t.Errorf("flag form = %+v, %v", r.Auth, err)
	}

	for name, cfg := range map[string]string{
		"both token and command": "connections:\n  x:\n    host: localhost\n    auth:\n      type: jwt\n      token: t\n      token_command: cmd\n",
		"wrong yaml type":        "connections:\n  x:\n    host: localhost\n    auth:\n      type: jwt\n      token_command: {program: x}\n",
	} {
		f, err := Load(writeConfig(t, cfg))
		if err == nil {
			_, err = Resolve(f, "x", none, none)
		}
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Every secret has a command companion derived by one rule, in the file, on
// the command line and in the environment.
func TestSecretCommandsAreUniform(t *testing.T) {
	for _, key := range SecretKeys {
		if got, want := CommandKey(key), key+"-command"; got != want {
			t.Errorf("CommandKey(%q) = %q", key, got)
		}
	}
	if got := EnvName(CommandKey(KeyClientSecret)); got != "CHCLI_OAUTH_CLIENT_SECRET_COMMAND" {
		t.Errorf("env name = %q", got)
	}

	f, err := Load(writeConfig(t, `
connections:
  pw:
    host: localhost
    auth:
      type: password
      username: analyst
      password_command: security find-generic-password -s chcli-pw -w
  sso:
    host: localhost
    auth:
      type: google
      client_id: abc.apps.googleusercontent.com
      client_secret_command: [pass, show, chcli/google]
`))
	if err != nil {
		t.Fatal(err)
	}
	pw, err := Resolve(f, "pw", none, none)
	if err != nil || pw.Auth.PasswordCommand.Shell != "security find-generic-password -s chcli-pw -w" {
		t.Errorf("password_command = %+v, %v", pw.Auth.PasswordCommand, err)
	}
	sso, err := Resolve(f, "sso", none, none)
	if err != nil || !reflect.DeepEqual(sso.Auth.ClientSecretCommand.Argv, []string{"pass", "show", "chcli/google"}) {
		t.Errorf("client_secret_command = %+v, %v", sso.Auth.ClientSecretCommand, err)
	}
	// A different auth type drops the commands of the others.
	if r, err := Resolve(f, "pw", none, source(map[string]string{KeyAuth: "jwt", KeyJWTToken: "t"})); err != nil || r.Auth.PasswordCommand.IsSet() {
		t.Errorf("password_command survived an auth switch: %+v, %v", r.Auth, err)
	}

	env := EnvSource(func(name string) (string, bool) {
		v, ok := map[string]string{"CHCLI_PASSWORD_COMMAND": "cat ~/.pw"}[name]
		return v, ok
	})
	if r, err := Resolve(&File{}, "", env, source(map[string]string{KeyHost: "localhost"})); err != nil || r.Auth.PasswordCommand.Shell != "cat ~/.pw" {
		t.Errorf("env password command: %+v, %v", r.Auth, err)
	}
	r, err := Resolve(&File{}, "", none, source(map[string]string{KeyHost: "localhost", KeyAuth: "oidc", KeyIssuer: "https://i", KeyClientID: "c",
		CommandKey(KeyClientSecret): "op read op://vault/chcli/secret"}))
	if err != nil || r.Auth.ClientSecretCommand.Shell != "op read op://vault/chcli/secret" {
		t.Errorf("flag client secret command: %+v, %v", r.Auth, err)
	}

	// Value and command for the same secret are mutually exclusive.
	for name, flags := range map[string]map[string]string{
		"password":      {KeyHost: "localhost", KeyPassword: "p", CommandKey(KeyPassword): "cmd"},
		"client secret": {KeyHost: "localhost", KeyAuth: "oidc", KeyIssuer: "https://i", KeyClientID: "c", KeyClientSecret: "s", CommandKey(KeyClientSecret): "cmd"},
	} {
		if _, err := Resolve(&File{}, "", none, source(flags)); err == nil || !strings.Contains(err.Error(), "both set") {
			t.Errorf("%s: error = %v, want a conflict", name, err)
		}
	}
	// The companion of a secret that does not apply to the auth type is reported like the secret itself.
	if _, err := Resolve(f, "sso", none, source(map[string]string{CommandKey(KeyPassword): "cmd"})); err == nil || !strings.Contains(err.Error(), "--password-command does not apply") {
		t.Errorf("error = %v", err)
	}
}
