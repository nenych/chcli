package repl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/completion"
	"github.com/nenych/chcli/internal/output"
	"github.com/nenych/chcli/internal/session"
	"github.com/nenych/chcli/internal/sqlutil"
)

// command is a backslash meta command.
type command struct {
	names []string // aliases, each including the backslash
	usage string   // argument synopsis for \help
	help  string
	arg   completion.ArgKind
	run   func(r *REPL, ctx context.Context, arg string) error
}

var commands []command

func init() {
	commands = []command{
		{names: []string{`\q`, `\quit`}, help: "Exit (also: exit, quit, Ctrl+D)",
			run: func(r *REPL, _ context.Context, _ string) error { r.exit = true; return nil }},
		{names: []string{`\l`, `\databases`}, help: "List databases",
			run: func(r *REPL, ctx context.Context, _ string) error { return r.query(ctx, "SHOW DATABASES") }},
		{names: []string{`\dt`, `\tables`}, usage: "[database]", help: "List tables of the current or given database", arg: completion.ArgDatabase,
			run: (*REPL).listTables},
		{names: []string{`\d`, `\describe`}, usage: "table", help: "Describe a table", arg: completion.ArgTable,
			run: func(r *REPL, ctx context.Context, arg string) error {
				if arg == "" {
					return errors.New(`usage: \d table`)
				}
				return r.query(ctx, "DESCRIBE TABLE "+arg)
			}},
		{names: []string{`\use`}, usage: "database", help: "Switch the current database", arg: completion.ArgDatabase,
			run: func(r *REPL, ctx context.Context, arg string) error {
				if arg == "" {
					return errors.New(`usage: \use database`)
				}
				return r.query(ctx, "USE "+arg)
			}},
		{names: []string{`\status`}, help: "Show connection and authentication status", run: (*REPL).status},
		{names: []string{`\refresh`}, help: "Reload the metadata used for autocompletion", run: (*REPL).refresh},
		{names: []string{`\format`}, usage: "[name]", help: "Show or set the output format", arg: completion.ArgFormat,
			run: (*REPL).format},
		{names: []string{`\history`}, usage: "[n]", help: "Show the last n history entries (default 20)", run: (*REPL).showHistory},
		{names: []string{`\help`, `\?`}, help: "Show this help", run: (*REPL).help},
	}
}

// ParseMeta splits a meta command line into the command name and its
// argument. A trailing semicolon is ignored. ok is false when the line is not
// a meta command.
func ParseMeta(line string) (name, arg string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, `\`) {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimSuffix(line, ";"))
	name, arg, _ = strings.Cut(line, " ")
	if i := strings.IndexByte(name, '\t'); i >= 0 {
		name, arg = name[:i], name[i+1:]+" "+arg
	}
	return name, strings.TrimSpace(arg), true
}

func findCommand(name string) *command {
	for i := range commands {
		for _, n := range commands[i].names {
			if n == name {
				return &commands[i]
			}
		}
	}
	return nil
}

func (r *REPL) runMeta(line string) {
	name, arg, _ := ParseMeta(line)
	cmd := findCommand(name)
	if cmd == nil {
		fmt.Fprintf(r.errOut, "Unknown command %s. Type \\help for a list of commands.\n", name)
		return
	}
	intr := r.interruptible()
	defer intr.stop()
	if err := cmd.run(r, intr.ctx, arg); err != nil {
		r.printError(err)
	}
}

// query runs SQL on behalf of a meta command and prints the result.
func (r *REPL) query(ctx context.Context, sql string) error {
	res, err := r.session.Execute(ctx, sql, r.out)
	if err != nil {
		return err
	}
	fmt.Fprintln(r.out, session.FormatFooter(res))
	return nil
}

func (r *REPL) listTables(ctx context.Context, arg string) error {
	database := r.opts.Client.Database()
	if arg != "" {
		database = sqlutil.Unquote(arg)
	}
	quoted := "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(database) + "'"
	return r.query(ctx, "SELECT name, engine FROM system.tables WHERE database = "+quoted+" ORDER BY name")
}

func (r *REPL) status(ctx context.Context, _ string) error {
	cfg := r.opts.Config
	info := r.opts.Client.Info()
	tls := "disabled"
	switch {
	case cfg.Secure && cfg.InsecureSkipVerify:
		tls = "enabled (certificate verification disabled)"
	case cfg.Secure:
		tls = "enabled"
	}
	lines := [][2]string{}
	if cfg.Profile != "" {
		lines = append(lines, [2]string{"Profile", cfg.Profile})
	}
	lines = append(lines,
		[2]string{"Host", cfg.Addr()},
		[2]string{"Database", r.opts.Client.Database()},
		[2]string{"Protocol", cfg.Protocol},
		[2]string{"Authentication", auth.Label(cfg.Auth.Type)},
	)

	// Credentials are only inspected for display; no token is printed.
	if creds, err := r.opts.Auth.Authenticate(ctx); err == nil {
		user := creds.Identity
		if user == "" {
			user = info.User
		}
		lines = append(lines, [2]string{"User", user})
		// With token authentication the server maps the identity to a
		// ClickHouse user of its own choosing; show it when it differs.
		if info.User != "" && info.User != user {
			lines = append(lines, [2]string{"ClickHouse user", info.User})
		}
		if !creds.Expiry.IsZero() {
			lines = append(lines, [2]string{"Token expires", "in " + FormatDuration(time.Until(creds.Expiry))})
		}
	}
	lines = append(lines, [2]string{"TLS", tls}, [2]string{"Server version", info.Version})

	snap := r.opts.Meta.Snapshot()
	metaState := "not loaded yet"
	if !snap.LoadedAt.IsZero() {
		metaState = fmt.Sprintf("loaded %s ago", FormatDuration(time.Since(snap.LoadedAt)))
	}
	if err := r.opts.Meta.LastError(); err != nil {
		metaState += " (incomplete: " + firstLine(err.Error()) + ")"
	}
	lines = append(lines, [2]string{"Completion metadata", metaState})

	for _, l := range lines {
		fmt.Fprintf(r.out, "%-20s %s\n", l[0]+":", l[1])
	}
	return nil
}

func (r *REPL) refresh(ctx context.Context, _ string) error {
	err := r.opts.Meta.Refresh(ctx)
	snap := r.opts.Meta.Snapshot()
	tables, columns := 0, 0
	for _, t := range snap.Tables {
		tables += len(t)
	}
	for _, c := range snap.Columns {
		columns += len(c)
	}
	fmt.Fprintf(r.out, "Metadata refreshed: %d databases, %d tables, %d columns, %d functions.\n",
		len(snap.Databases), tables, columns, len(snap.Functions))
	if err != nil {
		// Completion keeps working with whatever could be loaded.
		fmt.Fprintf(r.errOut, "Some metadata could not be loaded:\n%s\n", session.FormatError(err))
	}
	return nil
}

func (r *REPL) format(_ context.Context, arg string) error {
	if arg == "" {
		fmt.Fprintf(r.out, "Output format: %s (available: %s)\n", r.session.Format, strings.Join(output.Formats, ", "))
		return nil
	}
	format, err := output.Normalize(arg)
	if err != nil {
		return err
	}
	r.session.Format = format
	fmt.Fprintf(r.out, "Output format set to %s.\n", format)
	return nil
}

func (r *REPL) showHistory(_ context.Context, arg string) error {
	if r.opts.History == nil {
		fmt.Fprintln(r.out, "History is disabled.")
		return nil
	}
	n := 20
	if arg != "" {
		v, err := strconv.Atoi(arg)
		if err != nil || v < 1 {
			return errors.New(`usage: \history [n]`)
		}
		n = v
	}
	entries := r.opts.History.Entries()
	first := max(len(entries)-n, 0)
	for i, e := range entries[first:] {
		fmt.Fprintf(r.out, "%5d  %s\n", first+i+1, strings.ReplaceAll(e, "\n", "\n       "))
	}
	return nil
}

func (r *REPL) help(context.Context, string) error {
	fmt.Fprintln(r.out, "Meta commands:")
	for _, cmd := range commands {
		left := strings.Join(cmd.names, ", ")
		if cmd.usage != "" {
			left += " " + cmd.usage
		}
		fmt.Fprintf(r.out, "  %-24s %s\n", left, cmd.help)
	}
	fmt.Fprint(r.out, `
SQL statements end with a semicolon; Enter without one continues on the next
line. End a query with \G instead to show the result vertically, or add
FORMAT <name> to pick an output format for one query.

Keys: Tab completes, Up/Down browse history, Ctrl+C clears the line or
cancels a running query, Ctrl+D exits.
`)
	return nil
}

// FormatDuration renders a duration coarsely, the way people say it: "43m", "2h 5m".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
