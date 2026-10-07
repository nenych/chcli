package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/chclient"
)

const testConfig = `
connections:
  production:
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: google
      client_id: abc.apps.googleusercontent.com
      client_secret: super-secret-value
      issuer: https://accounts.google.com
      scopes: [openid, email]
  local:
    host: localhost
    auth:
      type: password
      username: default
      password: local-password-value
`

// invoke runs chcli with args in an isolated environment and returns its
// stdout, stderr and error.
func invoke(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHCLI_CONFIG", cfg)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("LocalAppData", filepath.Join(dir, "state"))

	var stdout, stderr bytes.Buffer
	a := &app{build: BuildInfo{Version: "1.2.3"}, stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	root := a.rootCommand()
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

// The example from the specification: profile + one override.
func TestConfigShowMergesAndRedacts(t *testing.T) {
	t.Setenv("CHCLI_OAUTH_AUDIENCE", "from-env")
	out, _, err := invoke(t, "config", "show", "--profile", "production", "--database", "chronicle", "--oauth-scope", "openid", "--oauth-scope", "profile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"profile: production", "host: clickhouse.example.com", "port: 9440", "database: chronicle", "secure: true",
		"type: google", "client_id: abc.apps.googleusercontent.com", "client_secret: '***'", "audience: from-env",
		"- openid\n", "- profile\n", "token_type: access_token",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config show output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "super-secret-value") || strings.Contains(out, "- email") {
		t.Errorf("config show leaked a secret or ignored the scope override:\n%s", out)
	}
}

func TestFlagsBeatEnvironmentBeatProfile(t *testing.T) {
	t.Setenv("CHCLI_HOST", "env-host")
	t.Setenv("CHCLI_USER", "env-user")
	t.Setenv("CHCLI_PASSWORD", "env-password-value")
	out, _, err := invoke(t, "config", "show", "--profile", "local", "--user", "flag-user")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "host: env-host") || !strings.Contains(out, "username: flag-user") {
		t.Errorf("precedence wrong:\n%s", out)
	}
	if strings.Contains(out, "password-value") {
		t.Errorf("password leaked:\n%s", out)
	}
}

func TestAuthOverrideOnProfile(t *testing.T) {
	out, _, err := invoke(t, "config", "show", "--profile", "production", "--auth", "password", "--user", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "type: password") || !strings.Contains(out, "username: admin") || strings.Contains(out, "client_id") {
		t.Errorf("auth override did not replace the profile's auth block:\n%s", out)
	}
}

func TestConfigList(t *testing.T) {
	out, _, err := invoke(t, "config", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "local") || !strings.HasPrefix(lines[2], "production") ||
		!strings.Contains(lines[2], "Google OAuth") {
		t.Errorf("config list output:\n%s", out)
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "password-value") {
		t.Errorf("config list leaked a secret:\n%s", out)
	}
}

func TestPositionalArgumentsAreNotProfiles(t *testing.T) {
	_, _, err := invoke(t, "production")
	var usage *usageError
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "--profile production") {
		t.Fatalf("error = %v, want a usage error pointing at --profile", err)
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exit code = %d, want %d", exitCode(err), ExitUsage)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--no-such-flag"}, "unknown flag"},
		{[]string{"config", "show", "--profile", "nope"}, `profile "nope" not found (available: local, production)`},
		{[]string{"config", "show"}, "no host configured"},
		{[]string{"config", "show", "--host", "h", "--port", "abc"}, "invalid argument"},
		{[]string{"config", "show", "--host", "h", "--auth", "google"}, `auth type "google" requires auth.client_id`},
		{[]string{"-q", "SELECT 1", "-f", "x.sql", "--host", "h"}, "cannot be used together"},
		{[]string{"-q", "SELECT 1", "--host", "h", "--format", "parquet"}, "unsupported output format"},
		{[]string{"auth", "login", "--profile", "local"}, "has no login step"},
		{[]string{"config", "show", "--profile", "local", "--jwt-token", "tok"}, `--jwt-token does not apply to "password" authentication`},
		{[]string{"config", "show", "--host", "ch.example.com", "--port", "9000", "--auth", "jwt", "--jwt-token", "tok"}, "without TLS"},
		{[]string{"auth", "status", "extra"}, "unknown command"},
	}
	for _, tt := range tests {
		_, _, err := invoke(t, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("chcli %v: error = %v, want it to contain %q", tt.args, err, tt.want)
			continue
		}
		if code := exitCode(err); code != ExitUsage {
			t.Errorf("chcli %v: exit code = %d, want %d", tt.args, code, ExitUsage)
		}
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{errors.New("query failed"), ExitError},
		{&usageError{errors.New("bad flag")}, ExitUsage},
		{&auth.LoginRequiredError{Profile: "p"}, ExitAuth},
		{&chclient.AuthError{Err: &auth.LoginRequiredError{Profile: "p"}}, ExitAuth},
		{fmt.Errorf("wrapped: %w", &chclient.AuthError{Err: errors.New("denied")}), ExitAuth},
		{&chclient.ConnError{Addr: "h:9000", Err: errors.New("refused")}, ExitConnection},
		{fmt.Errorf("stopped: %w", context.Canceled), ExitInterrupted},
		{&statusError{code: ExitAuth}, ExitAuth},
	}
	for _, tt := range tests {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

// Scripted runs must fail with the login hint instead of opening a browser.
func TestNonInteractiveOAuthWithoutSession(t *testing.T) {
	_, _, err := invoke(t, "--profile", "production", "-q", "SELECT 1")
	var login *auth.LoginRequiredError
	if !errors.As(err, &login) {
		t.Fatalf("error = %v (%T), want LoginRequiredError", err, err)
	}
	want := "OAuth credentials for profile \"production\" are not available.\nRun:\n  chcli auth login --profile production\nfrom an interactive terminal first."
	if err.Error() != want {
		t.Errorf("message:\n%s\nwant:\n%s", err.Error(), want)
	}
	if exitCode(err) != ExitAuth {
		t.Errorf("exit code = %d", exitCode(err))
	}
}

func TestAuthStatus(t *testing.T) {
	out, _, err := invoke(t, "auth", "status", "--profile", "local")
	if err != nil || out != "Profile: local\nAuthentication: Password\nUser: default\n" {
		t.Errorf("password status = %q, %v", out, err)
	}

	out, _, err = invoke(t, "auth", "status", "--profile", "production")
	if exitCode(err) != ExitAuth || !strings.Contains(out, "Authentication: Google OAuth\nToken status: not logged in\n") {
		t.Errorf("logged-out status = %q, %v", out, err)
	}

	out, _, err = invoke(t, "auth", "logout", "--profile", "production")
	if err != nil || !strings.Contains(out, "Logged out of production.") {
		t.Errorf("logout = %q, %v", out, err)
	}
}

func TestShellCompletionOfProfileNames(t *testing.T) {
	out, _, err := invoke(t, "__complete", "--profile", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "local\n") || !strings.Contains(out, "production\n") {
		t.Errorf("profile completion = %q", out)
	}
	out, _, _ = invoke(t, "__complete", "--auth", "")
	if !strings.Contains(out, "google") || !strings.Contains(out, "oidc") {
		t.Errorf("auth type completion = %q", out)
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		script, _, err := invoke(t, "completion", shell)
		if err != nil || len(script) < 100 {
			t.Errorf("completion %s: %d bytes, %v", shell, len(script), err)
		}
	}
}

func TestVersion(t *testing.T) {
	out, _, err := invoke(t, "version")
	if err != nil || out != "chcli 1.2.3\n" {
		t.Errorf("version = %q, %v", out, err)
	}
	out, _, err = invoke(t, "--version")
	if err != nil || out != "chcli 1.2.3\n" {
		t.Errorf("--version = %q, %v", out, err)
	}
}

func TestDebugLoggingRedactsSecrets(t *testing.T) {
	for _, args := range [][]string{
		{"config", "show", "--profile", "production", "--debug"},                                  // client secret from the file
		{"config", "show", "--profile", "local", "--debug", "--password", "typed-password-value"}, // password from a flag
		{"config", "show", "--host", "localhost", "--debug", "--jwt-token", "typed-token-value"},  // token from a flag
	} {
		_, stderr, err := invoke(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(stderr, "configuration resolved") {
			t.Fatalf("%v: no debug output:\n%s", args, stderr)
		}
		for _, secret := range []string{"super-secret-value", "typed-password-value", "local-password-value", "typed-token-value"} {
			if strings.Contains(stderr, secret) {
				t.Errorf("%v: debug log leaked %q:\n%s", args, secret, stderr)
			}
		}
		// The secret did reach the configuration: it is logged, but redacted.
		if !strings.Contains(stderr, "***") {
			t.Errorf("%v: expected a redacted secret in the debug log:\n%s", args, stderr)
		}
	}
}
