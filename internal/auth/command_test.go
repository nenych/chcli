package auth

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nenych/chcli/internal/config"
)

func shellCommand(t *testing.T, script string) config.Command {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test commands are POSIX shell")
	}
	return config.Command{Shell: script}
}

func TestTokenCommand(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	soon := unsignedJWT(t, map[string]any{"email": "svc@example.com", "exp": now.Add(90 * time.Second).Unix()})
	later := unsignedJWT(t, map[string]any{"email": "svc@example.com", "exp": now.Add(time.Hour).Unix()})

	// The command hands out `soon` first and `later` on every later run; a
	// counter file records how often it ran.
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	script := "n=$(cat " + counter + " 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + counter + "; " +
		"echo 'warning: something on stderr' >&2; " +
		"if [ $n -eq 1 ]; then echo " + soon + "; else echo " + later + "; fi"
	runs := func() string {
		b, _ := os.ReadFile(counter)
		return strings.TrimSpace(string(b))
	}

	var stderr bytes.Buffer
	p := &JWTProvider{Command: shellCommand(t, script), Err: &stderr}
	creds, err := p.Authenticate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Token != soon || creds.Identity != "svc@example.com" || runs() != "1" {
		t.Errorf("first call: token ok=%v identity=%q runs=%s", creds.Token == soon, creds.Identity, runs())
	}
	if !strings.Contains(stderr.String(), "warning: something on stderr") {
		t.Error("the command's stderr must reach the user")
	}

	// A valid token is reused without running the command again.
	if again, err := p.Authenticate(ctx); err != nil || again.Token != soon || runs() != "1" {
		t.Errorf("second call: err=%v reused=%v runs=%s", err, again != nil && again.Token == soon, runs())
	}

	// Inside the expiry margin the command runs again and the token changes.
	p.now = func() time.Time { return now.Add(45 * time.Second) }
	refreshed, err := p.Authenticate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Token != later || runs() != "2" {
		t.Errorf("expiring token was not replaced: replaced=%v runs=%s", refreshed.Token == later, runs())
	}
	// The non-interactive view shares the cache.
	if c, err := NonInteractive(p).Authenticate(ctx); err != nil || c.Token != later || runs() != "2" {
		t.Errorf("non-interactive view: err=%v shared=%v runs=%s", err, c != nil && c.Token == later, runs())
	}
}

func TestTokenCommandOutputForms(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		script string
		want   string
	}{
		"plain":            {"echo opaque-token", "opaque-token"},
		"trailing newline": {"printf 'tok\\n\\n'", "tok"},
		"last line wins":   {"echo 'Fetching credentials...'; echo real-token", "real-token"},
		"json token":       {`echo '{"token":"from-json"}'`, "from-json"},
		"json access":      {`echo '{"access_token":"acc","expires_in":3600}'`, "acc"},
		"exec credential":  {`echo '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"k8s"}}'`, "k8s"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &JWTProvider{Command: shellCommand(t, tc.script)}
			creds, err := p.Authenticate(ctx)
			if err != nil || creds.Token != tc.want {
				t.Errorf("token = %q, err = %v; want %q", creds, err, tc.want)
			}
		})
	}
	// The list form runs the program directly, without a shell.
	if runtime.GOOS != "windows" {
		p := &JWTProvider{Command: config.Command{Argv: []string{"printf", "%s", "argv-token"}}}
		if creds, err := p.Authenticate(ctx); err != nil || creds.Token != "argv-token" {
			t.Errorf("argv form: %v, %v", creds, err)
		}
	}
}

func TestTokenCommandFailures(t *testing.T) {
	ctx := context.Background()
	// Output of a failing command must not surface in the error; it may be
	// a partial credential. The secret lives in a file so that the command
	// line itself does not contain it.
	leak := filepath.Join(t.TempDir(), "leak")
	if err := os.WriteFile(leak, []byte("secret-output-must-not-leak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		script string
		want   string
	}{
		"non-zero exit":  {"cat " + leak + "; exit 3", "failed with exit status 3"},
		"no output":      {"true", "printed nothing"},
		"bad json":       {"echo '{not json'", "could not be parsed"},
		"json no token":  {`echo '{"foo":"bar"}'`, "without a"},
		"missing binary": {"/no/such/program", "failed with exit status 127"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &JWTProvider{Command: shellCommand(t, tc.script), Err: &bytes.Buffer{}}
			_, err := p.Authenticate(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-output") {
				t.Error("the command's output leaked into the error")
			}
		})
	}

	expired := unsignedJWT(t, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})
	p := &JWTProvider{Command: shellCommand(t, "echo "+expired)}
	if _, err := p.Authenticate(ctx); err == nil || !strings.Contains(err.Error(), "expired at") {
		t.Errorf("expired token from the command: %v", err)
	}

	// A cancelled context stops a command that hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p = &JWTProvider{Command: shellCommand(t, "sleep 30")}
	started := time.Now()
	if _, err := p.Authenticate(ctx); err == nil {
		t.Error("a hanging command must fail once the context is done")
	}
	if time.Since(started) > 5*time.Second {
		t.Error("the command outlived the context")
	}
}

func TestNewProviderWiresTokenCommand(t *testing.T) {
	r := resolved(t, "svc", config.Auth{Type: config.AuthJWT, TokenCommand: config.Command{Argv: []string{"printf", "%s", "x"}}})
	p, err := NewProvider(r, Options{Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	jp, ok := p.(*JWTProvider)
	if !ok || !jp.Command.IsSet() || !jp.Interactive {
		t.Errorf("provider = %#v", p)
	}
}

// A password or client secret command runs once per process and its result
// is shared with the non-interactive view; the secret itself never appears
// in output.
func TestPasswordFromCommand(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	script := "n=$(cat " + counter + " 2>/dev/null || echo 0); echo $((n+1)) > " + counter + "; echo hunter2-from-command"
	runs := func() string {
		b, _ := os.ReadFile(counter)
		return strings.TrimSpace(string(b))
	}
	r := resolved(t, "", config.Auth{Type: config.AuthPassword, Username: "u", PasswordCommand: shellCommand(t, script)})
	var stderr bytes.Buffer
	p, err := NewProvider(r, Options{Interactive: true, Out: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		creds, err := p.Authenticate(context.Background())
		if err != nil || creds.Password != "hunter2-from-command" || creds.Username != "u" {
			t.Fatalf("call %d: %+v, %v", i, creds, err)
		}
	}
	if c, err := NonInteractive(p).Authenticate(context.Background()); err != nil || c.Password != "hunter2-from-command" {
		t.Errorf("non-interactive view: %+v, %v", c, err)
	}
	if runs() != "1" {
		t.Errorf("the command ran %s times, want once", runs())
	}
	if strings.Contains(stderr.String(), "hunter2") {
		t.Error("the password reached stderr")
	}

	// Without a command the given value is used as is.
	plain, _ := NewProvider(resolved(t, "", config.Auth{Type: config.AuthPassword, Username: "u", Password: "pw"}), Options{})
	if creds, err := plain.Authenticate(context.Background()); err != nil || creds.Password != "pw" {
		t.Errorf("plain password: %+v, %v", creds, err)
	}
}

func TestClientSecretFromCommand(t *testing.T) {
	h := newHarness(t)
	h.idp.clientSecret = "client-secret-value"
	counter := filepath.Join(t.TempDir(), "runs")
	script := "n=$(cat " + counter + " 2>/dev/null || echo 0); echo $((n+1)) > " + counter + "; echo client-secret-value"
	h.cfg.ClientSecret = NewSecretSource("client secret", "", shellCommand(t, script), h.out)

	// Login exchanges the code with the secret the command printed...
	creds, err := h.provider(true).Authenticate(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if creds.Identity != "user@example.com" {
		t.Errorf("identity = %q", creds.Identity)
	}
	// ...and a refresh in a new process (a fresh source) runs it again, once.
	h.clock.Advance(2 * time.Hour)
	h.cfg.ClientSecret = NewSecretSource("client secret", "", shellCommand(t, script), h.out)
	if _, err := h.provider(false).Authenticate(testContext(t)); err != nil {
		t.Fatalf("refresh with the command-provided secret: %v", err)
	}
	b, _ := os.ReadFile(counter)
	if strings.TrimSpace(string(b)) != "2" {
		t.Errorf("the command ran %s times across two processes, want 2", strings.TrimSpace(string(b)))
	}
	if strings.Contains(h.out.String(), "client-secret-value") {
		t.Error("the client secret reached the user-facing output")
	}

	// A wrong secret from the command is a login failure, not a crash.
	h.cfg.ClientSecret = NewSecretSource("client secret", "", shellCommand(t, "echo wrong"), h.out)
	if err := h.provider(true).Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.provider(true).Authenticate(testContext(t)); err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Errorf("wrong secret: %v", err)
	}
}
