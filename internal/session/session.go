// Package session executes single statements against a ClickHouse client and
// renders their results. It is the common core of the interactive shell and
// of scripted (non-interactive) execution.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/nenych/chcli/internal/chclient"
	"github.com/nenych/chcli/internal/config"
	"github.com/nenych/chcli/internal/output"
	"github.com/nenych/chcli/internal/sqlutil"
)

// Session runs statements and writes results in the selected format.
type Session struct {
	Client *chclient.Client
	// Format is the output format used unless a statement overrides it with
	// a FORMAT clause or the \G terminator.
	Format string
}

// Result summarises an executed statement.
type Result struct {
	// Verb is the statement's first keyword, upper-cased.
	Verb string
	// HasResultSet is false for statements that return nothing (DDL, INSERT, USE).
	HasResultSet bool
	Rows         int64
	Elapsed      time.Duration
	Stats        chclient.Stats
}

// Execute runs one statement and writes its result set, if any, to out.
// Partial output is flushed even when the statement fails or is cancelled
// midway.
func (s *Session) Execute(ctx context.Context, input string, out io.Writer) (Result, error) {
	st := sqlutil.ParseStatement(input)
	res := Result{Verb: st.Verb}
	if st.SQL == "" {
		return res, nil
	}

	format := s.Format
	if st.Vertical {
		format = output.Vertical
	}
	if st.Format != "" {
		// Results are formatted by the client, so the FORMAT clause never
		// reaches the server; it selects one of our formats instead.
		f, err := output.Normalize(st.Format)
		if err != nil {
			return res, err
		}
		format = f
	}

	started := time.Now()
	err := s.run(ctx, st, format, out, &res)
	res.Elapsed = time.Since(started)
	return res, err
}

func (s *Session) run(ctx context.Context, st sqlutil.Statement, format string, out io.Writer, res *Result) error {
	switch st.Verb {
	case "USE":
		// Tracked client-side so the database survives reconnects.
		if db, ok := sqlutil.ParseUse(st.SQL); ok {
			return s.Client.UseDatabase(ctx, db)
		}
	case "SET":
		if settings, ok := sqlutil.ParseSet(st.SQL); ok {
			return s.Client.Set(ctx, settings)
		}
	case "INSERT":
		// Without inline data the server waits for rows the client never sends.
		switch sqlutil.ClassifyInsert(sqlutil.SignificantTokens(st.SQL)) {
		case sqlutil.InsertFormat:
			return errors.New("INSERT ... FORMAT with data supplied by the client is not supported; use INSERT ... VALUES (...) or INSERT ... SELECT")
		case sqlutil.InsertNoData:
			return errors.New("this INSERT carries no data; use INSERT ... VALUES (...) or INSERT ... SELECT")
		}
	}
	if s.Client.Options().Protocol == config.ProtocolHTTP && needsServerSession(st) {
		// Over HTTP every statement is its own request; saying "Ok." here
		// would be a lie, because the statement changes nothing that lasts.
		return errors.New("this statement only has an effect within a server session, which the HTTP protocol does not provide; use --protocol native")
	}
	if !sqlutil.ReturnsRows(st.Verb) {
		stats, err := s.Client.Exec(ctx, st.SQL)
		res.Stats = stats
		return err
	}

	rows, err := s.Client.Query(ctx, st.SQL)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols := rows.Columns()
	res.HasResultSet = true

	w, err := output.New(format, out)
	if err != nil {
		return err
	}
	outCols := make([]output.Column, len(cols))
	for i, c := range cols {
		outCols[i] = output.Column{Name: c.Name, Type: c.Type}
	}
	if err := w.Begin(outCols); err != nil {
		return err
	}
	for rows.Next() {
		// The driver keeps handing out rows it has already received after a
		// cancellation; stop at once, so that Ctrl+C stops the output too.
		if err := ctx.Err(); err != nil {
			_ = w.End()
			return err
		}
		values, err := rows.Values()
		if err != nil {
			_ = w.End()
			return err
		}
		if err := w.Row(values); err != nil {
			return err
		}
		res.Rows++
	}
	res.Stats = rows.Stats()
	if err := rows.Err(); err != nil {
		return errors.Join(err, w.End())
	}
	if tw, ok := w.(output.TotalsWriter); ok {
		totals, has, err := rows.Totals()
		if err != nil {
			return errors.Join(err, w.End())
		}
		if has {
			if err := tw.Totals(totals); err != nil {
				return err
			}
		}
	}
	return w.End()
}

// needsServerSession reports whether a statement only makes sense within a
// server-side session: session-level SET variants the client does not track
// itself (SET ROLE, ...) and temporary tables.
func needsServerSession(st sqlutil.Statement) bool {
	sig := sqlutil.SignificantTokens(st.SQL)
	switch st.Verb {
	case "SET":
		return true // plain "SET name = value" was handled before this point
	case "CREATE":
		for _, t := range sig[1:min(len(sig), 4)] { // CREATE [OR REPLACE] TEMPORARY TABLE
			if t.Is("TEMPORARY") {
				return true
			}
		}
	}
	return false
}

// FormatError renders an error for the user. Server exceptions are shown the
// way ClickHouse tools show them; a cancellation is not an error message.
func FormatError(err error) string {
	var ex *clickhouse.Exception
	switch {
	case errors.As(err, &ex):
		msg := fmt.Sprintf("Code: %d. %s", ex.Code, ex.Message)
		var authErr *chclient.AuthError
		if errors.As(err, &authErr) {
			// The server appends instructions for resetting the password of
			// a local installation; they are noise for a client user.
			msg, _, _ = strings.Cut(msg, "\n")
		}
		if ex.CodeName != "" && !strings.Contains(msg, ex.CodeName) {
			msg += " (" + ex.CodeName + ")"
		}
		return msg
	case errors.Is(err, context.Canceled):
		return "Query cancelled."
	}
	return err.Error()
}

// FormatFooter renders the status line shown after a statement in
// interactive mode, for example "3 rows in set. 0.087 sec.".
func FormatFooter(res Result) string {
	var s string
	switch {
	case !res.HasResultSet:
		s = "Ok."
	case res.Rows == 1:
		s = "1 row in set."
	default:
		s = fmt.Sprintf("%d rows in set.", res.Rows)
	}
	s += fmt.Sprintf(" %.3f sec.", res.Elapsed.Seconds())
	switch {
	case res.Stats.Rows == 1:
		s += fmt.Sprintf(" Processed 1 row, %s.", humanBytes(res.Stats.Bytes))
	case res.Stats.Rows > 1:
		s += fmt.Sprintf(" Processed %s rows, %s.", humanCount(res.Stats.Rows), humanBytes(res.Stats.Bytes))
	}
	return s
}

func humanCount(n uint64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2f billion", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.2f million", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.2f thousand", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func humanBytes(n uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(n), 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}
