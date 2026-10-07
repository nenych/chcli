package sqlutil

import "strings"

// SplitStatements splits a script into statements on top-level semicolons
// (semicolons inside strings, quoted identifiers and comments are ignored).
// Statements are returned trimmed and without the terminating semicolon;
// empty statements are dropped. rest is the trailing text that has no
// terminating semicolon (possibly empty or whitespace/comment only).
//
// An INSERT that expects its data from the client (INSERT ... FORMAT CSV)
// swallows the remainder of the script: what follows is data, in which a
// semicolon means nothing, and must never be mistaken for statements.
func SplitStatements(script string) (stmts []string, rest string) {
	start := 0
	for _, t := range Tokenize(script) {
		if !t.IsOp(";") {
			continue
		}
		stmt := strings.TrimSpace(script[start:t.Start])
		if sig := SignificantTokens(stmt); len(sig) > 0 && sig[0].Is("INSERT") && ClassifyInsert(sig) == InsertFormat {
			return append(stmts, strings.TrimSpace(script[start:])), ""
		}
		if HasContent(stmt) {
			stmts = append(stmts, stmt)
		}
		start = t.End
	}
	return stmts, script[start:]
}

// InsertKind says where an INSERT statement gets its data from.
type InsertKind int

const (
	// InsertNoData: no data clause, or VALUES with nothing after it. The
	// server would wait for data that never comes.
	InsertNoData InsertKind = iota
	// InsertValues: INSERT ... VALUES (...), data inline.
	InsertValues
	// InsertSelect: INSERT ... SELECT (or WITH ... SELECT).
	InsertSelect
	// InsertFormat: INSERT ... FORMAT <name>, data supplied by the client.
	InsertFormat
)

// ClassifyInsert inspects the significant tokens of an INSERT statement. It
// goes by the position of the data clause, not by the mere presence of a
// keyword, because inline data may itself contain words like "select".
func ClassifyInsert(sig []Token) InsertKind {
	i := 1
	if i < len(sig) && sig[i].Is("INTO") {
		i++
	}
	if i < len(sig) && (sig[i].Is("TABLE") || sig[i].Is("FUNCTION")) {
		i++
	}
	// The target name, which may itself be a keyword (a table called "format").
	for i < len(sig) && (sig[i].Kind == Ident || sig[i].Kind == Keyword || sig[i].Kind == QuotedIdent) {
		i++
		if i < len(sig) && sig[i].IsOp(".") {
			i++
			continue
		}
		break
	}
	depth := 0
	for ; i < len(sig); i++ {
		t := sig[i]
		switch {
		case t.IsOp("("):
			if depth == 0 && i+1 < len(sig) && (sig[i+1].Is("SELECT") || sig[i+1].Is("WITH")) {
				return InsertSelect
			}
			depth++
		case t.IsOp(")"):
			depth--
		case depth != 0:
		case t.Is("VALUES"):
			if i+1 < len(sig) {
				return InsertValues
			}
			return InsertNoData
		case t.Is("SELECT"), t.Is("WITH"):
			return InsertSelect
		case t.Is("FORMAT"):
			return InsertFormat
		}
	}
	return InsertNoData
}

// HasContent reports whether s contains anything other than whitespace and comments.
func HasContent(s string) bool {
	for _, t := range Tokenize(s) {
		if t.Significant() {
			return true
		}
	}
	return false
}

// IsComplete reports whether the input ends a statement: its last significant
// token is a semicolon or the \G terminator. Input with an open string or
// comment is never complete.
func IsComplete(input string) bool {
	tokens := Tokenize(input)
	if n := len(tokens); n > 0 && tokens[n-1].Unterminated {
		return false
	}
	sig := significant(tokens)
	n := len(sig)
	if n == 0 {
		return false
	}
	if sig[n-1].IsOp(";") {
		return true
	}
	_, vertical := trimVerticalTerminator(sig)
	return vertical
}

func significant(tokens []Token) []Token {
	out := tokens[:0:0]
	for _, t := range tokens {
		if t.Significant() {
			out = append(out, t)
		}
	}
	return out
}

// trimVerticalTerminator detects a trailing \G and returns the tokens before it.
func trimVerticalTerminator(sig []Token) ([]Token, bool) {
	n := len(sig)
	if n >= 2 && sig[n-2].IsOp("\\") && sig[n-1].Text == "G" && sig[n-2].End == sig[n-1].Start {
		return sig[:n-2], true
	}
	return sig, false
}

// Statement is a single statement prepared for execution.
type Statement struct {
	// SQL is the statement text with terminators and any trailing FORMAT
	// clause removed.
	SQL string
	// Verb is the upper-cased first keyword (SELECT, INSERT, USE, ...).
	Verb string
	// Format is the name given in a trailing FORMAT clause, if any.
	Format string
	// Vertical is set when the statement was terminated with \G.
	Vertical bool
}

// ParseStatement prepares one statement for execution. It strips a trailing
// semicolon or \G and, for statements that return rows, the "FORMAT <name>"
// clause: result formatting is done by the client, so the clause must not
// reach the server.
func ParseStatement(input string) Statement {
	sig := SignificantTokens(input)
	for len(sig) > 0 && sig[len(sig)-1].IsOp(";") {
		sig = sig[:len(sig)-1]
	}
	var st Statement
	sig, st.Vertical = trimVerticalTerminator(sig)
	for len(sig) > 0 && sig[len(sig)-1].IsOp(";") {
		sig = sig[:len(sig)-1]
	}
	if len(sig) == 0 {
		return st
	}
	st.Verb = sig[0].Upper()

	end := sig[len(sig)-1].End
	st.SQL = strings.TrimSpace(input[sig[0].Start:end])
	if at := formatClause(sig, st.Verb); at > 0 {
		st.Format = sig[at+1].Text
		st.SQL = input[sig[0].Start:sig[at-1].End]
		if tail := strings.TrimSpace(input[sig[at+1].End:end]); tail != "" {
			st.SQL += " " + tail
		}
	}
	return st
}

// formatClause returns the index of the FORMAT keyword of an output format
// clause, or 0 if the statement has none. Only statements that return rows
// have one (in "ALTER TABLE t ADD COLUMN format String" the word is a column
// name). The clause is the last top-level "FORMAT <name>" that is followed by
// nothing or by SETTINGS, and the name must be a plain identifier (or Null),
// which keeps a column called "format" (ORDER BY format DESC) from matching.
func formatClause(sig []Token, verb string) int {
	if !ReturnsRows(verb) {
		return 0
	}
	at, depth := 0, 0
	for i := 1; i+1 < len(sig); i++ {
		switch {
		case sig[i].IsOp("("):
			depth++
		case sig[i].IsOp(")"):
			depth--
		}
		if depth != 0 || !sig[i].Is("FORMAT") {
			continue
		}
		if name := sig[i+1]; name.Kind != Ident && !name.Is("NULL") {
			continue
		}
		if i+2 < len(sig) && !sig[i+2].Is("SETTINGS") {
			continue
		}
		at = i
	}
	return at
}

// ParseUse extracts the database name from a "USE db" statement.
func ParseUse(sql string) (db string, ok bool) {
	sig := SignificantTokens(sql)
	if len(sig) != 2 || !sig[0].Is("USE") {
		return "", false
	}
	switch sig[1].Kind {
	case Ident, Keyword:
		return sig[1].Text, true
	case QuotedIdent:
		return Unquote(sig[1].Text), !sig[1].Unterminated
	}
	return "", false
}

// ParseSet extracts the assignments of a plain "SET name = value [, ...]"
// statement. It returns ok=false for anything else (SET ROLE, SET DEFAULT
// ROLE, query parameters, expressions), which callers should send to the
// server unchanged.
func ParseSet(sql string) (settings map[string]string, ok bool) {
	sig := SignificantTokens(sql)
	if len(sig) < 4 || !sig[0].Is("SET") {
		return nil, false
	}
	settings = map[string]string{}
	rest := sig[1:]
	for {
		if len(rest) < 3 || rest[0].Kind != Ident || !rest[1].IsOp("=") {
			return nil, false
		}
		name, value := rest[0].Text, rest[2]
		if strings.HasPrefix(name, "param_") {
			return nil, false
		}
		sign := ""
		if (value.IsOp("-") || value.IsOp("+")) && len(rest) > 3 && rest[3].Kind == Number {
			sign, value, rest = value.Text, rest[3], rest[1:]
		}
		switch value.Kind {
		case String:
			if value.Unterminated {
				return nil, false
			}
			settings[name] = Unquote(value.Text)
		case Number:
			settings[name] = sign + value.Text
		case Ident, Keyword:
			settings[name] = value.Text
		default:
			return nil, false
		}
		rest = rest[3:]
		if len(rest) == 0 {
			return settings, true
		}
		if !rest[0].IsOp(",") {
			return nil, false
		}
		rest = rest[1:]
	}
}

// ReturnsRows reports whether a statement starting with verb produces a
// result set. The driver needs to know up front: statements without one must
// be executed, not queried.
func ReturnsRows(verb string) bool {
	switch verb {
	case "SELECT", "WITH", "SHOW", "DESCRIBE", "DESC", "EXISTS", "EXPLAIN", "CHECK", "KILL", "WATCH", "FROM", "VALUES", "(":
		return true
	}
	return false
}
