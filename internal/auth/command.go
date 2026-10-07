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
	"time"

	"github.com/nenych/chcli/internal/config"
)

// tokenCommandTimeout bounds a token command, which may have to talk to an
// identity provider or even ask the user to log in.
const tokenCommandTimeout = 2 * time.Minute

// runTokenCommand runs the configured command and returns the token it
// printed. The token is the last non-empty line of standard output, or, when
// the output is a JSON object, its "token", "access_token", "id_token" or
// "status.token" (kubeconfig ExecCredential) field. Standard error goes to
// errOut; with interactive set, the command also gets the terminal as its
// standard input.
func runTokenCommand(ctx context.Context, command config.TokenCommand, interactive bool, errOut io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenCommandTimeout)
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

	// The output is a credential: it is never logged and never part of an error.
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("token command %q did not finish within %s", command, tokenCommandTimeout)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("token command %q failed with %s", command, exitErr.ProcessState)
		}
		return "", fmt.Errorf("token command %q: %w", command, err)
	}
	return parseTokenOutput(out)
}

func parseTokenOutput(out []byte) (string, error) {
	text := strings.TrimSpace(string(out))
	if strings.HasPrefix(text, "{") {
		var doc struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
			IDToken     string `json:"id_token"`
			Status      struct {
				Token string `json:"token"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(text), &doc); err != nil {
			return "", errors.New("the token command printed JSON that could not be parsed")
		}
		for _, candidate := range []string{doc.Token, doc.AccessToken, doc.IDToken, doc.Status.Token} {
			if candidate != "" {
				return candidate, nil
			}
		}
		return "", errors.New(`the token command printed JSON without a "token", "access_token", "id_token" or "status.token" field`)
	}
	lines := bytes.Split([]byte(text), []byte("\n"))
	last := strings.TrimSpace(string(lines[len(lines)-1]))
	if last == "" {
		return "", errors.New("the token command printed nothing")
	}
	return last, nil
}
