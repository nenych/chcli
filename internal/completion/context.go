package completion

import "github.com/nenych/chcli/internal/sqlutil"

// context is what kind of thing is expected at the cursor.
type context int

const (
	ctxNone            context = iota // nothing useful to suggest
	ctxStart                          // beginning of a statement
	ctxKeyword                        // a keyword continues the statement
	ctxExpression                     // columns, functions, aliases, keywords
	ctxColumn                         // columns of the referenced tables only
	ctxTable                          // a table (or a database to qualify one)
	ctxTableOrFunction                // FROM / JOIN: tables and table functions
	ctxDatabase
	ctxEngine
	ctxFormat
	ctxSetting
	ctxDataType
)

// analyze decides the context from the significant tokens that precede the
// word being completed (and its qualifier) within the current statement.
//
// It first looks at the token right before the cursor, which settles the
// unambiguous cases ("FROM |", "USE |", "x::|"), and otherwise walks back to
// the clause keyword that governs the cursor position, skipping over
// parenthesised groups.
//
// Many ClickHouse keywords double as column names in system tables
// (database, table, engine, format, ...). To keep "SELECT database, table, |"
// working, such words only count as keywords directly before the cursor and
// only in statements where they can be keywords.
func analyze(before []sqlutil.Token) context {
	n := len(before)
	if n == 0 {
		return ctxStart
	}
	last, verb := before[n-1], before[0].Upper()
	query := verb == "SELECT" || verb == "WITH" || verb == "INSERT" || verb == "EXPLAIN"

	if last.IsOp("::") {
		return ctxDataType
	}
	if last.Kind == sqlutil.Keyword {
		switch last.Upper() {
		case "FROM", "JOIN":
			return ctxTableOrFunction
		case "INTO", "UPDATE", "DESCRIBE", "EXISTS", "TRUNCATE", "OPTIMIZE", "DETACH", "ATTACH", "RENAME", "EXCHANGE":
			return ctxTable
		case "TABLE", "DICTIONARY", "VIEW":
			if !query || (n >= 2 && before[n-2].Is("INTO")) {
				return ctxTable
			}
		case "DESC":
			if n == 1 {
				return ctxTable
			}
		case "USE":
			return ctxDatabase
		case "DATABASE":
			if !query {
				return ctxDatabase
			}
		case "ENGINE":
			if !query {
				return ctxEngine
			}
		case "FORMAT":
			if n > 1 {
				return ctxFormat
			}
		case "SETTINGS":
			return ctxSetting
		case "COLUMN":
			if !query {
				return ctxColumn
			}
		case "AS":
			if inCast(before) {
				return ctxDataType
			}
			return ctxNone // a new alias name
		}
	}
	if last.IsOp("=") && n >= 2 && before[n-2].Is("ENGINE") && !query {
		return ctxEngine
	}
	if verb == "SET" {
		if n == 1 || last.IsOp(",") {
			return ctxSetting
		}
		return ctxNone
	}

	// Walk back to the governing clause keyword.
	depth, inside := 0, false
	for i := n - 1; i >= 0; i-- {
		t := before[i]
		switch {
		case t.IsOp(")"):
			depth++
			continue
		case t.IsOp("("):
			if depth > 0 {
				depth--
			} else {
				inside = true // the cursor is within this parenthesis
			}
			continue
		}
		if depth > 0 || t.Kind != sqlutil.Keyword {
			continue
		}
		switch t.Upper() {
		case "SELECT", "WHERE", "PREWHERE", "HAVING", "ON", "USING", "BY", "WITH", "QUALIFY", "VALUES":
			return ctxExpression
		case "FROM", "JOIN":
			switch {
			case inside: // table function arguments or a subquery
				return ctxExpression
			case last.IsOp(","):
				return ctxTableOrFunction
			}
			return ctxKeyword // after the table: an alias or the next clause
		case "INTO":
			if inside { // INSERT INTO t (col, ...
				return ctxColumn
			}
			return ctxKeyword
		case "SETTINGS":
			if last.IsOp(",") {
				return ctxSetting
			}
			return ctxNone
		case "LIMIT", "OFFSET":
			return ctxKeyword
		}
	}

	// No clause found: DDL and other statements.
	if inside && verb == "CREATE" {
		if last.Kind == sqlutil.Ident || last.Kind == sqlutil.QuotedIdent {
			return ctxDataType // CREATE TABLE t (name |
		}
		return ctxNone // a new column name
	}
	if n >= 2 && before[n-2].Is("COLUMN") && !query {
		return ctxDataType // ALTER TABLE t ADD COLUMN name |
	}
	return ctxKeyword
}

// inCast reports whether the tokens end inside an unclosed CAST( ... .
func inCast(before []sqlutil.Token) bool {
	depth := 0
	for i := len(before) - 1; i >= 0; i-- {
		switch {
		case before[i].IsOp(")"):
			depth++
		case before[i].IsOp("("):
			if depth == 0 {
				return i > 0 && before[i-1].Is("CAST")
			}
			depth--
		}
	}
	return false
}

// tableRef is a table mentioned in a statement.
type tableRef struct {
	Database string // empty when unqualified
	Table    string
	Alias    string
}

func (r tableRef) databaseOr(current string) string {
	if r.Database != "" {
		return r.Database
	}
	return current
}

// extractTables finds the tables a statement refers to, with their aliases,
// by looking at what follows FROM, JOIN, INTO, UPDATE and TABLE anywhere in
// the statement (including after the cursor, so "SELECT | FROM t" works).
// It also returns the column aliases introduced with AS.
func extractTables(stmt []sqlutil.Token) (refs []tableRef, columnAliases []string) {
	tableAlias := map[int]bool{} // token indexes of table aliases
	for i, t := range stmt {
		list := t.Is("FROM")
		insert := t.Is("INTO")
		if !(list || insert || t.Is("JOIN") || t.Is("UPDATE") || t.Is("TABLE") ||
			(i == 0 && (t.Is("DESCRIBE") || t.Is("DESC")))) {
			continue
		}
		j := i + 1
		if j < len(stmt) && stmt[j].Is("TABLE") { // INSERT INTO TABLE t, DESCRIBE TABLE t
			continue
		}
		for {
			ref, aliasAt, next, ok := parseTableRef(stmt, j, insert)
			if !ok {
				break
			}
			refs = append(refs, ref)
			if aliasAt >= 0 {
				tableAlias[aliasAt] = true
			}
			if !list || next >= len(stmt) || !stmt[next].IsOp(",") {
				break
			}
			j = next + 1
		}
	}
	for i := 1; i < len(stmt); i++ {
		if stmt[i-1].Is("AS") && stmt[i].Kind == sqlutil.Ident && !tableAlias[i] {
			columnAliases = append(columnAliases, stmt[i].Text)
		}
	}
	return refs, columnAliases
}

// parseTableRef parses "[db.]table [[AS] alias]" at stmt[j]. A name followed
// by "(" is a table function call, not a table, unless allowParen is set
// (the column list of INSERT INTO t (...)).
func parseTableRef(stmt []sqlutil.Token, j int, allowParen bool) (ref tableRef, aliasAt, next int, ok bool) {
	aliasAt = -1
	if j >= len(stmt) || !isName(stmt[j]) {
		return ref, aliasAt, j, false
	}
	ref.Table = sqlutil.Unquote(stmt[j].Text)
	j++
	if j+1 < len(stmt) && stmt[j].IsOp(".") && isName(stmt[j+1]) {
		ref.Database, ref.Table = ref.Table, sqlutil.Unquote(stmt[j+1].Text)
		j += 2
	}
	if j < len(stmt) && stmt[j].IsOp("(") && !allowParen {
		return ref, aliasAt, j, false
	}
	isAlias := func(t sqlutil.Token) bool {
		return t.Kind == sqlutil.Ident || (t.Kind == sqlutil.QuotedIdent && !t.Unterminated)
	}
	switch {
	case j+1 < len(stmt) && stmt[j].Is("AS") && isAlias(stmt[j+1]):
		ref.Alias, aliasAt = sqlutil.Unquote(stmt[j+1].Text), j+1
		j += 2
	case j < len(stmt) && isAlias(stmt[j]):
		ref.Alias, aliasAt = sqlutil.Unquote(stmt[j].Text), j
		j++
	}
	return ref, aliasAt, j, true
}
