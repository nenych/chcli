// Package cli defines the chcli command line: flags, subcommands and the
// wiring between configuration, authentication, transport and the shell.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/cli/browser"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/chclient"
	"github.com/nenych/chcli/internal/config"
	"github.com/nenych/chcli/internal/output"
	"github.com/nenych/chcli/internal/session"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitError       = 1   // a statement failed, or another runtime error
	ExitUsage       = 2   // invalid flags or configuration
	ExitAuth        = 3   // authentication failed or a login is required
	ExitConnection  = 4   // the server could not be reached
	ExitInterrupted = 130 // interrupted by Ctrl+C
)

// BuildInfo identifies the build.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// usageError marks errors caused by how the program was invoked or configured.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// app holds the state shared by all commands.
type app struct {
	build  BuildInfo
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	configPath  string
	profile     string
	debug       bool
	askPassword bool
	query       string
	file        string
	format      string
}

// Execute runs the command line and returns the process exit code.
func Execute(build BuildInfo) int {
	if build.Version == "dev" {
		// Built with "go install": take the version from the module info.
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			build.Version = strings.TrimPrefix(info.Main.Version, "v")
		}
	}
	a := &app{build: build, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	err := a.rootCommand().Execute()
	if err == nil {
		return ExitOK
	}
	code := exitCode(err)
	// A cancelled run has already said so, as has a command that reported
	// its own failure; everything else needs explaining.
	var reported *statusError
	if !errors.Is(err, context.Canceled) && !errors.As(err, &reported) {
		fmt.Fprintf(a.stderr, "chcli: %s\n", session.FormatError(err))
		if code == ExitUsage {
			var flagErr *flagError
			if errors.As(err, &flagErr) {
				fmt.Fprintln(a.stderr, "Run 'chcli --help' for usage.")
			}
		}
	}
	return code
}

// statusError carries an exit code for a failure that has already been
// reported to the user.
type statusError struct{ code int }

func (e *statusError) Error() string { return "" }

func exitCode(err error) int {
	var (
		usage  *usageError
		login  *auth.LoginRequiredError
		authE  *chclient.AuthError
		connE  *chclient.ConnError
		status *statusError
	)
	switch {
	case errors.As(err, &status):
		return status.code
	case errors.As(err, &usage):
		return ExitUsage
	case errors.As(err, &login), errors.As(err, &authE):
		return ExitAuth
	case errors.As(err, &connE):
		return ExitConnection
	case errors.Is(err, context.Canceled):
		return ExitInterrupted
	}
	return ExitError
}

// noArgs rejects positional arguments as a usage error.
func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return &usageError{&flagError{err}}
	}
	return nil
}

// flagError marks command-line parsing errors, which get a usage hint.
type flagError struct{ err error }

func (e *flagError) Error() string { return e.err.Error() }
func (e *flagError) Unwrap() error { return e.err }

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "chcli",
		Short: "A modern interactive ClickHouse client",
		Long: `chcli is an interactive ClickHouse client with smart autocompletion,
connection profiles and first-class OAuth/OIDC support.

Run without a query to start the interactive shell, or pass statements with
--query, --file or on standard input for scripted use.`,
		Example: `  chcli --host localhost --user default
  chcli --profile production
  chcli --profile production --database analytics -q "SELECT count() FROM events"
  chcli --profile production --format json < report.sql
  chcli auth login --profile production`,
		Version:       a.versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return &usageError{&flagError{fmt.Errorf(
				"unexpected argument %q; profiles are selected with --profile, for example: chcli --profile %s", args[0], args[0])}}
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return a.run(cmd) },
	}
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetVersionTemplate("chcli {{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{&flagError{err}} })

	pf := root.PersistentFlags()
	pf.SortFlags = false
	pf.StringVar(&a.profile, "profile", "", "connection profile from the configuration file")
	pf.StringVar(&a.configPath, "config", "", "configuration file (default "+config.DefaultPath()+")")
	pf.String(config.KeyHost, "", "server host")
	pf.Int(config.KeyPort, 0, "server port (default 9000, 9440 with TLS; 8123/8443 for http)")
	pf.StringP(config.KeyDatabase, "d", "", `database (default "default")`)
	pf.String(config.KeyProtocol, "", "transport protocol: native or http (default native)")
	pf.Bool(config.KeySecure, false, "connect with TLS")
	pf.Bool(config.KeyInsecureSkipVerify, false, "do not verify the server's TLS certificate (unsafe)")
	pf.String(config.KeyCACert, "", "PEM file with the CA certificate(s) to trust")
	pf.String(config.KeyAuth, "", "authentication type: password, jwt, oidc or google")
	pf.StringP(config.KeyUser, "u", "", `user name for password authentication (default "default")`)
	pf.String(config.KeyPassword, "", "password (prefer CHCLI_PASSWORD or --ask-password)")
	pf.BoolVar(&a.askPassword, "ask-password", false, "prompt for the password")
	pf.String(config.KeyJWTToken, "", "JWT / bearer token (prefer CHCLI_JWT_TOKEN)")
	pf.String(config.KeyJWTTokenCommand, "", "shell command that prints the JWT / bearer token, run again when it expires")
	pf.Bool(config.KeyGoogleOAuth, false, "shortcut for --auth google")
	pf.String(config.KeyClientID, "", "OAuth client ID")
	pf.String(config.KeyClientSecret, "", "OAuth client secret (prefer CHCLI_OAUTH_CLIENT_SECRET)")
	pf.String(config.KeyIssuer, "", "OIDC issuer URL, used for endpoint discovery")
	pf.String(config.KeyAuthEndpoint, "", "OAuth authorization endpoint (overrides discovery)")
	pf.String(config.KeyTokenEndpoint, "", "OAuth token endpoint (overrides discovery)")
	pf.String(config.KeyDeviceEndpoint, "", "OAuth device authorization endpoint (overrides discovery)")
	pf.String(config.KeyAudience, "", "audience to request for the access token")
	pf.String(config.KeyRedirectURI, "", "loopback redirect URI (default http://127.0.0.1:<random port>/callback)")
	pf.String(config.KeyUsernameClaim, "", `token claim shown as the user name (default "email")`)
	pf.StringArray(config.KeyScope, nil, "OAuth scope; repeat for several (default openid, email, profile)")
	pf.String(config.KeyFlow, "", "OAuth login flow: browser or device (default browser)")
	pf.String(config.KeyTokenType, "", "token sent to ClickHouse: access_token or id_token (default access_token)")
	pf.BoolVar(&a.debug, "debug", false, "print debug logs to stderr (credentials are redacted)")

	f := root.Flags()
	f.SortFlags = false
	f.StringVarP(&a.query, "query", "q", "", "run the given statement(s) and exit")
	f.StringVarP(&a.file, "file", "f", "", "run the statements in a file and exit")
	f.StringVar(&a.format, "format", "", "output format: "+strings.Join(output.Formats, ", ")+
		" (default table on a terminal, tsv otherwise)")

	a.registerCompletions(root)
	root.PersistentPreRun = func(*cobra.Command, []string) { a.setupLogging() }
	root.AddCommand(a.authCommand(), a.configCommand(), a.doctorCommand(), a.versionCommand())
	return root
}

func (a *app) versionString() string {
	s := a.build.Version
	if a.build.Commit != "" {
		s += " (" + a.build.Commit
		if a.build.Date != "" {
			s += ", " + a.build.Date
		}
		s += ")"
	}
	return s
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the chcli version",
		Args:  noArgs,
		Run: func(*cobra.Command, []string) {
			fmt.Fprintf(a.stdout, "chcli %s\n", a.versionString())
		},
	}
}

func (a *app) setupLogging() {
	level := slog.LevelWarn
	if a.debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level})))
}

func (a *app) registerCompletions(root *cobra.Command) {
	fixed := func(values ...string) cobra.CompletionFunc {
		return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return values, cobra.ShellCompDirectiveNoFileComp
		}
	}
	_ = root.RegisterFlagCompletionFunc("profile", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		file, err := config.Load(a.configFile())
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return file.ProfileNames(), cobra.ShellCompDirectiveNoFileComp
	})
	_ = root.RegisterFlagCompletionFunc(config.KeyAuth, fixed(config.AuthPassword, config.AuthJWT, config.AuthOIDC, config.AuthGoogle))
	_ = root.RegisterFlagCompletionFunc(config.KeyProtocol, fixed(config.ProtocolNative, config.ProtocolHTTP))
	_ = root.RegisterFlagCompletionFunc(config.KeyFlow, fixed(config.FlowBrowser, config.FlowDevice))
	_ = root.RegisterFlagCompletionFunc(config.KeyTokenType, fixed(config.TokenTypeID, config.TokenTypeAccess))
	_ = root.RegisterFlagCompletionFunc("format", fixed(output.Formats...))
}

func (a *app) configFile() string {
	if a.configPath != "" {
		return a.configPath
	}
	return config.DefaultPath()
}

// flagSource exposes the flags that were explicitly set as a config layer.
func flagSource(flags *pflag.FlagSet) config.Source {
	return config.Source{
		Lookup: func(key string) (string, bool) {
			f := flags.Lookup(key)
			if f == nil || !f.Changed {
				return "", false
			}
			if list, ok := f.Value.(pflag.SliceValue); ok {
				return strings.Join(list.GetSlice(), ","), true
			}
			return f.Value.String(), true
		},
		Name: func(key string) string { return "--" + key },
	}
}

// loadConfig reads the configuration file and reports its warnings.
func (a *app) loadConfig() (*config.File, error) {
	file, err := config.Load(a.configFile())
	if err != nil {
		return nil, &usageError{err}
	}
	for _, w := range file.Warnings {
		fmt.Fprintf(a.stderr, "Warning: %s\n", w)
	}
	return file, nil
}

// resolve computes the effective connection configuration for a command.
func (a *app) resolve(cmd *cobra.Command) (*config.File, *config.Resolved, error) {
	file, err := a.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	resolved, err := config.Resolve(file, a.profile, config.EnvSource(os.LookupEnv), flagSource(cmd.Flags()))
	if err != nil {
		return nil, nil, &usageError{err}
	}
	slog.Debug("configuration resolved", "config", fmt.Sprintf("%+v", *resolved))
	return file, resolved, nil
}

// newProvider builds the auth provider. interactive permits a browser login.
func (a *app) newProvider(resolved *config.Resolved, interactive bool) (auth.Provider, error) {
	browser.Stdout, browser.Stderr = io.Discard, io.Discard
	return auth.NewProvider(resolved, auth.Options{
		Interactive: interactive,
		Out:         a.stderr,
		Store:       auth.NewTokenStore(filepath.Join(config.StateDir(), "tokens")),
		OpenBrowser: browser.OpenURL,
	})
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// promptPassword reads a password from the terminal without echo.
func (a *app) promptPassword(resolved *config.Resolved) error {
	if resolved.Auth.Type != config.AuthPassword {
		return &usageError{fmt.Errorf("--ask-password only applies to password authentication, not %q", resolved.Auth.Type)}
	}
	if !isTerminal(os.Stdin) {
		return &usageError{errors.New("--ask-password needs a terminal; use CHCLI_PASSWORD in scripts")}
	}
	fmt.Fprintf(a.stderr, "Password for %s: ", resolved.Auth.Username)
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(a.stderr)
	if err != nil {
		return err
	}
	resolved.Auth.Password = config.Secret(pw)
	return nil
}
