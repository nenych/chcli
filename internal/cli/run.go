package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/chclient"
	"github.com/nenych/chcli/internal/config"
	"github.com/nenych/chcli/internal/history"
	"github.com/nenych/chcli/internal/metadata"
	"github.com/nenych/chcli/internal/output"
	"github.com/nenych/chcli/internal/repl"
	"github.com/nenych/chcli/internal/session"
	"github.com/nenych/chcli/internal/sqlutil"
)

// run is the root command: the interactive shell, or scripted execution when
// statements are supplied with --query, --file or on standard input.
func (a *app) run(cmd *cobra.Command) error {
	if a.query != "" && a.file != "" {
		return &usageError{errors.New("--query and --file cannot be used together")}
	}
	file, resolved, err := a.resolve(cmd)
	if err != nil {
		return err
	}
	script, scripted, err := a.scriptInput(cmd)
	if err != nil {
		return err
	}
	if !scripted && !isTerminal(os.Stdout) {
		return &usageError{errors.New("the interactive shell needs a terminal; use --query or --file to run statements")}
	}
	if a.askPassword {
		if err := a.promptPassword(resolved); err != nil {
			return err
		}
	}

	format, err := a.outputFormat(file, scripted)
	if err != nil {
		return err
	}

	// Only the interactive shell may open a browser. Scripted runs behave the
	// same whether or not a terminal happens to be attached.
	provider, err := a.newProvider(resolved, !scripted)
	if err != nil {
		return err
	}
	client := chclient.New(chclient.OptionsFrom(resolved, a.build.Version), provider)
	defer client.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := client.Connect(ctx); err != nil {
		return err
	}

	if scripted {
		return a.runScript(ctx, client, script, format)
	}
	stop() // from here on the shell handles Ctrl+C itself
	return a.runShell(file, resolved, provider, client, format)
}

// scriptInput returns the statements to run non-interactively, if any.
func (a *app) scriptInput(cmd *cobra.Command) (script string, scripted bool, err error) {
	switch {
	case cmd.Flags().Changed("query"):
		return a.query, true, nil
	case a.file != "":
		data, err := os.ReadFile(a.file)
		if err != nil {
			return "", false, &usageError{err}
		}
		return string(data), true, nil
	case !isTerminal(os.Stdin):
		data, err := io.ReadAll(a.stdin)
		if err != nil {
			return "", false, fmt.Errorf("read standard input: %w", err)
		}
		return string(data), true, nil
	}
	return "", false, nil
}

// outputFormat picks the format: --format, then (for the shell) the
// configured default, then table on a terminal and tsv when piped.
func (a *app) outputFormat(file *config.File, scripted bool) (string, error) {
	name := a.format
	if name == "" && !scripted {
		name = file.Output.Format
	}
	if name == "" {
		if isTerminal(os.Stdout) {
			return output.Table, nil
		}
		return output.TSV, nil
	}
	format, err := output.Normalize(name)
	if err != nil {
		return "", &usageError{err}
	}
	return format, nil
}

// runScript executes statements in order and stops at the first failure.
func (a *app) runScript(ctx context.Context, client *chclient.Client, script, format string) error {
	sess := &session.Session{Client: client, Format: format}
	stmts, rest := sqlutil.SplitStatements(script)
	if sqlutil.HasContent(rest) {
		stmts = append(stmts, strings.TrimSpace(rest))
	}
	for _, stmt := range stmts {
		if _, err := sess.Execute(ctx, stmt, a.stdout); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) runShell(file *config.File, resolved *config.Resolved, provider auth.Provider, client *chclient.Client, format string) error {
	if creds, err := provider.Authenticate(context.Background()); err == nil && creds.UsesToken() && creds.Identity != "" {
		fmt.Fprintf(a.stdout, "Authenticated as %s\n", creds.Identity)
	}
	fmt.Fprintf(a.stdout, "Connected to %s (ClickHouse %s)\n", resolved.Host, client.Info().Version)

	var hist *history.History
	if file.History.IsEnabled() {
		path := filepath.Join(config.StateDir(), "history", history.FileName(resolved.Label()))
		h, err := history.Open(path, file.History.Limit())
		if err != nil {
			fmt.Fprintf(a.stderr, "Warning: history is unavailable: %v\n", err)
		} else {
			hist = h
		}
	}

	// Completion metadata is loaded over its own connection so that it never
	// competes with, or disturbs the session state of, the user's statements.
	// Being background work, it must never start a login of its own.
	metaClient := chclient.New(chclient.OptionsFrom(resolved, a.build.Version), auth.NonInteractive(provider))
	defer metaClient.Close()

	repl.New(repl.Options{
		Config:      resolved,
		Client:      client,
		Auth:        provider,
		Meta:        metadata.NewCache(metaClient),
		History:     hist,
		HistorySize: file.History.Limit(),
		Format:      format,
		Pager:       file.Output.Pager,
	}).Run()
	return nil
}
