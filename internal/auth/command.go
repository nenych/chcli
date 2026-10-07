package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nenych/chcli/internal/config"
)

// commandTimeout bounds a secret command, which may have to talk to an
// identity provider or even ask the user to log in.
const commandTimeout = 2 * time.Minute

// SecretSource yields a secret that is either given directly or printed by a
// command. A command runs at most once per process; its result is cached.
// (Tokens, which expire, are handled by JWTProvider instead.)
type SecretSource struct {
	what    string // for messages: "password", "client secret"
	value   string
	command config.Command
	errOut  io.Writer

	mu     sync.Mutex
	loaded bool
}

// NewSecretSource builds a source from a direct value or a command (at most
// one of them is set; the configuration guarantees that).
func NewSecretSource(what, value string, command config.Command, errOut io.Writer) *SecretSource {
	return &SecretSource{what: what, value: value, command: command, errOut: errOut}
}

// Get returns the secret, running the command the first time. interactive
// lets the command use the terminal.
func (s *SecretSource) Get(ctx context.Context, interactive bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded || !s.command.IsSet() {
		return s.value, nil
	}
	value, err := runSecretCommand(ctx, s.command, interactive, s.errOut)
	if err != nil {
		return "", err
	}
	s.value, s.loaded = value, true
	return value, nil
}

// runSecretCommand runs a command and returns the secret it printed: the
// last non-empty line of standard output, or, when the output is a JSON
// object, its "token", "access_token", "id_token", "status.token" (kubeconfig
// ExecCredential), "password", "secret", "client_secret" or "value" field.
// Standard error goes to errOut; with interactive set, the command also gets
// the terminal as its standard input. The output is never logged and never
// part of an error.
func runSecretCommand(ctx context.Context, command config.Command, interactive bool, errOut io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	var cmd *exec.Cmd
	switch {
	case len(command.Argv) > 0:
		cmd = exec.CommandContext(ctx, command.Argv[0], command.Argv[1:]...)
	case runtime.GOOS == "windows":
		cmd = exec.CommandContext(ctx, "cmd", "/C", command.Shell)
	default:
		cmd = exec.CommandContext(ctx, "sh", "-c", command.Shell)
	}
	cmd.Stderr = errOut
	if interactive {
		cmd.Stdin = os.Stdin
	}
	configureProcess(cmd)
	// After cancellation, do not wait forever for descendants that still
	// hold the output pipe.
	cmd.WaitDelay = 2 * time.Second

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("command %q did not finish within %s", command, commandTimeout)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("command %q failed with %s", command, exitErr.ProcessState)
		}
		return "", fmt.Errorf("command %q: %w", command, err)
	}
	return parseSecretOutput(out)
}

func parseSecretOutput(out []byte) (string, error) {
	text := strings.TrimSpace(string(out))
	if strings.HasPrefix(text, "{") {
		var doc struct {
			Token        string `json:"token"`
			AccessToken  string `json:"access_token"`
			IDToken      string `json:"id_token"`
			Password     string `json:"password"`
			Secret       string `json:"secret"`
			ClientSecret string `json:"client_secret"`
			Value        string `json:"value"`
			Status       struct {
				Token string `json:"token"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(text), &doc); err != nil {
			return "", errors.New("the command printed JSON that could not be parsed")
		}
		for _, candidate := range []string{doc.Token, doc.AccessToken, doc.IDToken, doc.Status.Token, doc.Password, doc.Secret, doc.ClientSecret, doc.Value} {
			if candidate != "" {
				return candidate, nil
			}
		}
		return "", errors.New(`the command printed JSON without a "token", "access_token", "id_token", "status.token", "password", "secret", "client_secret" or "value" field`)
	}
	lines := bytes.Split([]byte(text), []byte("\n"))
	last := strings.TrimSpace(string(lines[len(lines)-1]))
	if last == "" {
		return "", errors.New("the command printed nothing")
	}
	return last, nil
}
