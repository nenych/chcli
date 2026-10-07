package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/config"
	"github.com/nenych/chcli/internal/repl"
	"github.com/nenych/chcli/internal/session"
)

func (a *app) authCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage OAuth/OIDC login sessions",
		Args:  noArgs,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:     "login",
			Short:   "Log in with the connection's identity provider and cache the tokens",
			Example: "  chcli auth login --profile production",
			Args:    noArgs,
			RunE:    func(cmd *cobra.Command, _ []string) error { return a.authLogin(cmd) },
		},
		&cobra.Command{
			Use:     "logout",
			Short:   "Remove the cached tokens of a connection",
			Example: "  chcli auth logout --profile production",
			Args:    noArgs,
			RunE:    func(cmd *cobra.Command, _ []string) error { return a.authLogout(cmd) },
		},
		&cobra.Command{
			Use:     "status",
			Short:   "Show the authentication state of a connection without revealing credentials",
			Example: "  chcli auth status --profile production",
			Args:    noArgs,
			RunE:    func(cmd *cobra.Command, _ []string) error { return a.authStatus(cmd) },
		},
	)
	return cmd
}

// sessionProvider resolves the connection and returns its provider if it
// keeps a login session (OIDC or Google); other auth types have nothing to
// log in to or out of.
func (a *app) sessionProvider(cmd *cobra.Command, interactive bool) (*config.Resolved, auth.SessionProvider, error) {
	_, resolved, err := a.resolve(cmd)
	if err != nil {
		return nil, nil, err
	}
	provider, err := a.newProvider(resolved, interactive)
	if err != nil {
		return nil, nil, err
	}
	sp, _ := provider.(auth.SessionProvider)
	return resolved, sp, nil
}

func (a *app) authLogin(cmd *cobra.Command) error {
	resolved, provider, err := a.sessionProvider(cmd, true)
	if err != nil {
		return err
	}
	if provider == nil {
		return &usageError{fmt.Errorf("%s authentication has no login step; \"auth login\" applies to oidc and google connections",
			auth.Label(resolved.Auth.Type))}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	creds, err := provider.Login(ctx)
	if err != nil {
		return err
	}
	identity := creds.Identity
	if identity == "" {
		identity = "(identity not present in the token)"
	}
	fmt.Fprintf(a.stdout, "Authenticated as %s\n", identity)
	return nil
}

func (a *app) authLogout(cmd *cobra.Command) error {
	resolved, provider, err := a.sessionProvider(cmd, false)
	if err != nil {
		return err
	}
	if provider == nil {
		fmt.Fprintf(a.stdout, "%s authentication keeps no cached session; nothing to do.\n", auth.Label(resolved.Auth.Type))
		return nil
	}
	if err := provider.Logout(); err != nil {
		return fmt.Errorf("remove cached tokens: %w", err)
	}
	fmt.Fprintf(a.stdout, "Logged out of %s.\n", resolved.Label())
	return nil
}

func (a *app) authStatus(cmd *cobra.Command) error {
	resolved, provider, err := a.sessionProvider(cmd, false)
	if err != nil {
		return err
	}
	line := func(key, value string) { fmt.Fprintf(a.stdout, "%s: %s\n", key, value) }
	if resolved.Profile != "" {
		line("Profile", resolved.Profile)
	}
	line("Authentication", auth.Label(resolved.Auth.Type))

	if provider == nil {
		switch resolved.Auth.Type {
		case config.AuthPassword:
			line("User", resolved.Auth.Username)
			if resolved.Auth.PasswordCommand.IsSet() {
				line("Password source", "command: "+resolved.Auth.PasswordCommand.String())
			}
		case config.AuthJWT:
			return a.staticTokenStatus(cmd, resolved, line)
		}
		return nil
	}

	st, err := provider.Status()
	if err != nil {
		return err
	}
	if !st.LoggedIn {
		line("Token status", "not logged in")
		return &statusError{ExitAuth}
	}
	if st.Identity != "" {
		line("User", st.Identity)
	}
	expired := !st.Expiry.IsZero() && !time.Now().Before(st.Expiry)
	switch {
	case !expired:
		line("Token status", "valid")
		if !st.Expiry.IsZero() {
			line("Expires in", repl.FormatDuration(time.Until(st.Expiry)))
		}
	case st.Refreshable:
		line("Token status", "expired (will be refreshed automatically)")
	default:
		line("Token status", "expired (login required)")
	}
	if st.Refreshable {
		line("Refresh token", "present")
	}
	line("Token storage", st.Storage)
	if expired && !st.Refreshable {
		return &statusError{ExitAuth}
	}
	return nil
}

// staticTokenStatus reports on a user-supplied JWT, running the token
// command if one is configured.
func (a *app) staticTokenStatus(cmd *cobra.Command, resolved *config.Resolved, line func(key, value string)) error {
	if resolved.Auth.TokenCommand.IsSet() {
		line("Token source", "command: "+resolved.Auth.TokenCommand.String())
	}
	provider, err := a.newProvider(resolved, false)
	if err != nil {
		return err
	}
	creds, err := provider.Authenticate(cmd.Context())
	if err != nil {
		line("Token status", session.FormatError(err))
		return &statusError{ExitAuth}
	}
	if creds.Identity != "" {
		line("User", creds.Identity)
	}
	// The claims a server checks: a mismatch here is the usual reason for
	// "token is invalid" errors.
	if creds.Issuer != "" {
		line("Issuer", creds.Issuer)
	}
	if creds.Audience != "" {
		line("Audience", creds.Audience)
	}
	line("Token status", "valid")
	if !creds.Expiry.IsZero() {
		line("Expires in", repl.FormatDuration(time.Until(creds.Expiry)))
	}
	return nil
}
