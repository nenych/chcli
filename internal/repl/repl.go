// Package repl implements the interactive shell: line editing, completion,
// highlighting, meta commands and query execution with cancellation.
package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"time"

	prompt "github.com/elk-language/go-prompt"
	istrings "github.com/elk-language/go-prompt/strings"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/chclient"
	"github.com/nenych/chcli/internal/completion"
	"github.com/nenych/chcli/internal/config"
	"github.com/nenych/chcli/internal/history"
	"github.com/nenych/chcli/internal/metadata"
	"github.com/nenych/chcli/internal/output"
	"github.com/nenych/chcli/internal/session"
	"github.com/nenych/chcli/internal/sqlutil"
)

// metadataMaxAge is how old the completion metadata may get before a
// statement triggers a background refresh.
const metadataMaxAge = 10 * time.Minute

// Options wires the shell to the rest of the program.
type Options struct {
	Config  *config.Resolved
	Client  *chclient.Client // runs the user's statements
	Auth    auth.Provider
	Meta    *metadata.Cache
	History *history.History // nil disables persistent history
	// HistorySize bounds the in-memory history.
	HistorySize int
	Format      string
	// Pager is the command interactive table output is piped through; empty disables paging.
	Pager string
}

// REPL is an interactive session.
type REPL struct {
	opts    Options
	session *session.Session
	engine  *completion.Engine
	out     io.Writer
	errOut  io.Writer
	exit    bool
}

// New creates a shell that reads from the terminal and writes to stdout/stderr.
func New(opts Options) *REPL {
	r := &REPL{
		opts:    opts,
		session: &session.Session{Client: opts.Client, Format: opts.Format},
		out:     os.Stdout,
		errOut:  os.Stderr,
	}
	r.engine = &completion.Engine{OutputFormats: output.Formats}
	for _, cmd := range commands {
		for _, name := range cmd.names {
			r.engine.Commands = append(r.engine.Commands, completion.MetaCommand{Name: name, Help: cmd.help, Arg: cmd.arg})
		}
	}
	return r
}

// Run starts the shell and returns when the user leaves it.
func (r *REPL) Run() {
	r.opts.Meta.RefreshAsync()

	var entries []string
	if r.opts.History != nil {
		entries = append(entries, r.opts.History.Entries()...)
	}
	p := prompt.New(r.execute,
		prompt.WithPrefixCallback(r.prefix),
		prompt.WithCompleter(r.complete),
		prompt.WithCompletionWordSeparator(completion.WordSeparators),
		prompt.WithMaxSuggestion(10),
		prompt.WithLexer(prompt.NewEagerLexer(highlight)),
		prompt.WithExecuteOnEnterCallback(func(p *prompt.Prompt, _ int) (int, bool) {
			return 0, shouldExecute(p.Buffer().Text())
		}),
		prompt.WithHistorySize(max(r.opts.HistorySize, 1)),
		prompt.WithHistory(entries),
		prompt.WithExitChecker(func(_ string, breakline bool) bool { return breakline && r.exit }),
		prompt.WithPrefixTextColor(prompt.DefaultColor),
		prompt.WithSuggestionTextColor(prompt.White),
		prompt.WithSuggestionBGColor(prompt.DarkGray),
		prompt.WithSelectedSuggestionTextColor(prompt.Black),
		prompt.WithSelectedSuggestionBGColor(prompt.Cyan),
		prompt.WithDescriptionTextColor(prompt.LightGray),
		prompt.WithDescriptionBGColor(prompt.DarkGray),
		prompt.WithSelectedDescriptionTextColor(prompt.Black),
		prompt.WithSelectedDescriptionBGColor(prompt.Cyan),
		prompt.WithScrollbarBGColor(prompt.DarkGray),
		prompt.WithScrollbarThumbColor(prompt.LightGray),
	)
	p.Run()
	fmt.Fprintln(r.out, "Bye.")
}

// prefix renders the prompt, which names the profile (or host) and the
// current database: "production/chronicle :) ".
func (r *REPL) prefix() string {
	return fmt.Sprintf("%s/%s :) ", r.opts.Config.Label(), r.opts.Client.Database())
}

// shouldExecute decides what Enter does: run the input, or continue it on a
// new line. SQL runs once it is terminated by a semicolon (or \G); meta
// commands and the bare words exit/quit/help run immediately.
func shouldExecute(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.HasPrefix(trimmed, `\`) {
		return true
	}
	switch strings.ToLower(trimmed) {
	case "exit", "quit", "help":
		return true
	}
	return sqlutil.IsComplete(text)
}

func (r *REPL) complete(d prompt.Document) ([]prompt.Suggest, istrings.RuneNumber, istrings.RuneNumber) {
	cursor := d.CurrentRuneIndex()
	if strings.TrimSpace(d.Text) == "" {
		return nil, cursor, cursor
	}
	before := d.TextBeforeCursor()
	res := r.engine.Complete(d.Text, len(before), r.opts.Meta.Snapshot(), r.opts.Client.Database())
	suggestions := make([]prompt.Suggest, len(res.Suggestions))
	for i, s := range res.Suggestions {
		desc := string(s.Kind)
		if s.Detail != "" {
			desc += "  " + s.Detail
		}
		suggestions[i] = prompt.Suggest{Text: s.Text, Description: desc}
	}
	return suggestions, istrings.RuneCountInString(before[:res.Start]), cursor
}

// execute handles one submitted input: a meta command or one or more SQL
// statements.
func (r *REPL) execute(input string) {
	text := strings.TrimSpace(input)
	if text == "" {
		return
	}
	r.remember(input)

	if strings.HasPrefix(text, `\`) {
		r.runMeta(text)
		return
	}
	switch strings.ToLower(strings.TrimRight(text, "; \t")) {
	case "exit", "quit":
		r.exit = true
		return
	case "help":
		r.runMeta(`\help`)
		return
	}

	stmts, rest := sqlutil.SplitStatements(text)
	if sqlutil.HasContent(rest) {
		stmts = append(stmts, strings.TrimSpace(rest))
	}
	for _, stmt := range stmts {
		if !r.runStatement(stmt) {
			break // do not run the rest of a batch after a failure
		}
	}
}

func (r *REPL) remember(input string) {
	if r.opts.History == nil {
		return
	}
	if _, err := r.opts.History.Add(input); err != nil {
		fmt.Fprintf(r.errOut, "Warning: could not save history: %v\n", err)
	}
}

// runStatement executes one SQL statement and reports whether it succeeded.
func (r *REPL) runStatement(stmt string) bool {
	intr := r.interruptible()
	defer intr.stop()

	res, pagerClosed, err := r.executeWithPager(intr, stmt)
	switch {
	case err != nil:
		r.printError(err)
		return false
	case pagerClosed:
		// Not the whole result: say so instead of reporting a row count
		// that looks like the size of the result.
		fmt.Fprintf(r.out, "Stopped after %d rows: the pager was closed. %.3f sec.\n", res.Rows, res.Elapsed.Seconds())
	default:
		fmt.Fprintln(r.out, session.FormatFooter(res))
	}

	switch res.Verb {
	case "CREATE", "DROP", "ALTER", "RENAME", "ATTACH", "DETACH", "EXCHANGE", "UNDROP":
		r.opts.Meta.RefreshAsync()
	default:
		r.opts.Meta.RefreshIfOlder(metadataMaxAge)
	}
	return true
}

func (r *REPL) printError(err error) {
	fmt.Fprintf(r.errOut, "%s\n", session.FormatError(err))
	var connErr *chclient.ConnError
	if errors.As(err, &connErr) {
		fmt.Fprintln(r.errOut, "The connection will be re-established on the next statement.")
	}
}

// interrupt turns Ctrl+C into query cancellation while a statement runs.
type interrupt struct {
	ctx    context.Context
	cancel context.CancelFunc
	sig    chan os.Signal
	done   chan struct{}
	armed  atomic.Bool
}

// interruptible starts handling Ctrl+C for one statement: the first one
// cancels the context, which asks the server to stop the query and returns to
// the prompt; a second one, for a query that will not die, exits the program.
func (r *REPL) interruptible() *interrupt {
	ctx, cancel := context.WithCancel(context.Background())
	i := &interrupt{ctx: ctx, cancel: cancel, sig: make(chan os.Signal, 2), done: make(chan struct{})}
	i.armed.Store(true)
	signal.Notify(i.sig, os.Interrupt)
	go func() {
		interrupts := 0
		for {
			select {
			case <-i.done:
				return
			case <-i.sig:
			}
			if !i.armed.Load() {
				continue // see disarm
			}
			interrupts++
			if interrupts == 1 {
				fmt.Fprintln(r.errOut, "\nCancelling query... (press Ctrl+C again to force quit)")
				cancel()
				continue
			}
			fmt.Fprintln(r.errOut, "Forced exit.")
			os.Exit(130)
		}
	}()
	return i
}

// disarm makes further interrupts harmless without restoring the default
// (fatal) handling. It is used once the query has finished but the pager is
// still showing its output: Ctrl+C then belongs to the pager.
func (i *interrupt) disarm() { i.armed.Store(false) }

func (i *interrupt) stop() {
	signal.Stop(i.sig)
	close(i.done)
	i.cancel()
}

// executeWithPager runs a statement, sending human-oriented output through
// the configured pager. pagerClosed reports that the user quit the pager
// before the result was complete, which stops the query.
func (r *REPL) executeWithPager(intr *interrupt, stmt string) (res session.Result, pagerClosed bool, err error) {
	ctx := intr.ctx
	direct := func() (session.Result, bool, error) {
		res, err := r.session.Execute(ctx, stmt, r.out)
		return res, false, err
	}
	args := strings.Fields(r.opts.Pager)
	if len(args) == 0 || (r.session.Format != output.Table && r.session.Format != output.Vertical) {
		return direct()
	}
	// The pager is deliberately not bound to the query's context: after a
	// cancelled query the user still reads what has arrived so far.
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return direct()
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(r.errOut, "Warning: cannot start pager %q: %v\n", r.opts.Pager, err)
		return direct()
	}

	// Quitting the pager early stops the query instead of reading the rest
	// of the result into a closed pipe.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
		cancel()
	}()

	res, err = r.session.Execute(ctx, stmt, stdin)
	intr.disarm() // the query is over; the user may now be reading in the pager
	select {
	case <-exited:
		pagerClosed = true
	default:
	}
	_ = stdin.Close()
	<-exited
	if pagerClosed && err != nil {
		return res, true, nil // the user saw what they wanted; not an error
	}
	return res, false, err
}
