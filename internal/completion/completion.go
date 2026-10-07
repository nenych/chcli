// Package completion suggests completions for partially typed ClickHouse SQL.
//
// The engine is a pure function of the input text, the cursor position and a
// metadata snapshot: it performs no I/O and can therefore run on every
// keystroke. It works on tokens rather than a parse tree because the input is
// by definition incomplete; see context.go for the heuristics.
package completion

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nenych/chcli/internal/metadata"
	"github.com/nenych/chcli/internal/sqlutil"
)

// Kind says what a suggestion is.
type Kind string

const (
	KindKeyword       Kind = "keyword"
	KindDatabase      Kind = "database"
	KindTable         Kind = "table"
	KindView          Kind = "view"
	KindDictionary    Kind = "dictionary"
	KindColumn        Kind = "column"
	KindAlias         Kind = "alias"
	KindFunction      Kind = "function"
	KindAggregate     Kind = "aggregate function"
	KindTableFunction Kind = "table function"
	KindDataType      Kind = "type"
	KindEngine        Kind = "engine"
	KindFormat        Kind = "format"
	KindSetting       Kind = "setting"
	KindCommand       Kind = "command"
)

// Suggestion is one completion candidate.
type Suggestion struct {
	// Text replaces the word being completed.
	Text string
	Kind Kind
	// Detail is extra information for display, such as a column's type.
	Detail string
}

// Result is the outcome of a completion request.
type Result struct {
	Suggestions []Suggestion
	// Start is the byte offset of the word being completed; a chosen
	// suggestion replaces input[Start:cursor].
	Start int
}

// WordSeparators are the characters that end a completable word. The line
// editor re-derives the word being replaced from these, so suggestions must
// never contain one of them; names that would are not offered (see offerable).
const WordSeparators = " \t\n\r!\"#%&'()*+,-./:;<=>?@[]^{|}~"

// MaxSuggestions caps the size of a result.
const MaxSuggestions = 500

// ArgKind says what a meta command's argument refers to.
type ArgKind int

const (
	ArgNone ArgKind = iota
	ArgDatabase
	ArgTable
	ArgFormat
)

// MetaCommand describes a backslash command for completion purposes.
type MetaCommand struct {
	Name string // including the backslash
	Help string
	Arg  ArgKind
}

// Engine completes SQL and meta commands.
type Engine struct {
	Commands []MetaCommand
	// OutputFormats are the client-side formats offered after FORMAT.
	OutputFormats []string
}

// Complete returns suggestions for the word that ends at cursor (a byte
// offset into input). database is the session's current database.
func (e *Engine) Complete(input string, cursor int, snap *metadata.Snapshot, database string) Result {
	start := wordStart(input, cursor)
	word := input[start:cursor]
	res := Result{Start: start}

	tokens := sqlutil.Tokenize(input)
	stmtStart, stmtEnd := statementBounds(tokens, start, len(input))

	if lead := strings.TrimLeft(input[stmtStart:cursor], " \t\r\n"); strings.HasPrefix(lead, `\`) {
		return e.completeMeta(input, stmtStart, cursor, snap, database)
	}
	if insideLiteral(tokens, cursor) || startsWithDigit(word) || strings.HasSuffix(input[:start], `\`) {
		return res // nothing to complete in literals, numbers or the \G terminator
	}

	var before, stmt []sqlutil.Token
	for _, t := range tokens {
		if !t.Significant() || t.Start < stmtStart || t.End > stmtEnd {
			continue
		}
		stmt = append(stmt, t)
		if t.End <= start {
			before = append(before, t)
		}
	}
	before, qualifier := splitQualifier(before, start)

	c := &collector{snap: snap, database: database, word: word}
	refs, columnAliases := extractTables(stmt)
	ctx := analyze(before)

	if len(qualifier) > 0 {
		c.qualified(qualifier, ctx, refs)
		res.Suggestions = c.out
		return res
	}

	switch ctx {
	case ctxStart, ctxKeyword:
		if word != "" {
			c.keywords()
		}
	case ctxExpression:
		if word != "" || len(refs) > 0 {
			c.refColumns(refs)
			c.tableAliases(refs)
			c.columnAliases(columnAliases)
		}
		if word != "" {
			c.functions()
			c.keywords()
		}
	case ctxColumn:
		c.refColumns(refs)
	case ctxTable:
		c.tables(database)
		c.databases()
	case ctxTableOrFunction:
		c.tables(database)
		c.databases()
		if word != "" {
			c.names(snap.TableFunctions, KindTableFunction)
		}
	case ctxDatabase:
		c.databases()
	case ctxEngine:
		c.names(snap.Engines, KindEngine)
	case ctxFormat:
		c.names(e.OutputFormats, KindFormat)
		c.names(snap.Formats, KindFormat)
	case ctxSetting:
		c.names(snap.Settings, KindSetting)
	case ctxDataType:
		c.names(snap.DataTypes, KindDataType)
	}
	res.Suggestions = c.out
	return res
}

// wordStart returns the offset where the identifier ending at cursor begins.
func wordStart(input string, cursor int) int {
	start := cursor
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(input[:start])
		if !sqlutil.IsIdentPart(r) {
			break
		}
		start -= size
	}
	return start
}

func startsWithDigit(word string) bool {
	return word != "" && word[0] >= '0' && word[0] <= '9'
}

// statementBounds returns the byte range of the statement containing pos.
func statementBounds(tokens []sqlutil.Token, pos, end int) (int, int) {
	start := 0
	for _, t := range tokens {
		if !t.IsOp(";") {
			continue
		}
		if t.End <= pos {
			start = t.End
		} else {
			return start, t.Start
		}
	}
	return start, end
}

// insideLiteral reports whether the cursor is inside a string, a comment or
// a quoted identifier, where nothing should be completed.
func insideLiteral(tokens []sqlutil.Token, cursor int) bool {
	for _, t := range tokens {
		if t.Start >= cursor {
			break
		}
		switch t.Kind {
		case sqlutil.String, sqlutil.QuotedIdent:
			if cursor < t.End || (cursor == t.End && t.Unterminated) {
				return true
			}
		case sqlutil.Comment:
			lineComment := !strings.HasPrefix(t.Text, "/*")
			if cursor < t.End || (cursor == t.End && (t.Unterminated || lineComment)) {
				return true
			}
		}
	}
	return false
}

// splitQualifier peels a dotted prefix ("db.", "db.table.", "alias.") that
// directly precedes the word off the end of the tokens.
func splitQualifier(before []sqlutil.Token, wordStart int) (rest []sqlutil.Token, qualifier []string) {
	end := wordStart
	i := len(before)
	for i >= 2 && before[i-1].IsOp(".") && before[i-1].End == end && isName(before[i-2]) && before[i-2].End == before[i-1].Start {
		qualifier = append([]string{sqlutil.Unquote(before[i-2].Text)}, qualifier...)
		end = before[i-2].Start
		i -= 2
	}
	return before[:i], qualifier
}

func isName(t sqlutil.Token) bool {
	return t.Kind == sqlutil.Ident || t.Kind == sqlutil.Keyword || (t.Kind == sqlutil.QuotedIdent && !t.Unterminated)
}

// completeMeta completes backslash commands and their arguments.
func (e *Engine) completeMeta(input string, stmtStart, cursor int, snap *metadata.Snapshot, database string) Result {
	cmdStart := stmtStart + strings.Index(input[stmtStart:cursor], `\`)
	text := input[cmdStart:cursor]

	space := strings.IndexAny(text, " \t")
	if space < 0 {
		res := Result{Start: cmdStart}
		for _, cmd := range e.Commands {
			if strings.HasPrefix(cmd.Name, text) {
				res.Suggestions = append(res.Suggestions, Suggestion{Text: cmd.Name, Kind: KindCommand, Detail: cmd.Help})
			}
		}
		return res
	}

	start := wordStart(input, cursor)
	res := Result{Start: start}
	c := &collector{snap: snap, database: database, word: input[start:cursor]}
	var arg ArgKind
	for _, cmd := range e.Commands {
		if cmd.Name == text[:space] {
			arg = cmd.Arg
		}
	}
	// A single "db." qualifier is supported for table arguments.
	if _, qualifier := splitQualifier(sqlutil.SignificantTokens(input[:start]), start); len(qualifier) == 1 && arg == ArgTable {
		c.tables(qualifier[0])
		res.Suggestions = c.out
		return res
	}
	switch arg {
	case ArgDatabase:
		c.databases()
	case ArgTable:
		c.tables(database)
		c.databases()
	case ArgFormat:
		c.names(e.OutputFormats, KindFormat)
	}
	res.Suggestions = c.out
	return res
}

// collector accumulates matching suggestions.
type collector struct {
	snap     *metadata.Snapshot
	database string
	word     string
	out      []Suggestion
	seen     map[string]struct{}
}

func (c *collector) matches(name string) bool {
	return len(name) >= len(c.word) && strings.EqualFold(name[:len(c.word)], c.word)
}

// offerable reports whether text can be inserted by the line editor: it must
// not contain a word separator. This excludes identifiers that need quoting
// because of spaces or punctuation; they still work when typed by hand.
func offerable(text string) bool {
	return !strings.ContainsAny(text, WordSeparators)
}

// add records a suggestion for an object name, quoting it when necessary.
func (c *collector) add(name string, kind Kind, detail string) {
	if !c.matches(name) || len(c.out) >= MaxSuggestions {
		return
	}
	text := sqlutil.QuoteIdent(name)
	if !offerable(text) {
		return
	}
	c.addText(text, kind, detail)
}

func (c *collector) addText(text string, kind Kind, detail string) {
	key := string(kind) + "\x00" + text
	if c.seen == nil {
		c.seen = map[string]struct{}{}
	}
	if _, dup := c.seen[key]; dup {
		return
	}
	c.seen[key] = struct{}{}
	c.out = append(c.out, Suggestion{Text: text, Kind: kind, Detail: detail})
}

// names adds plain words (types, engines, formats, settings) verbatim.
func (c *collector) names(list []string, kind Kind) {
	for _, name := range list {
		if c.matches(name) && len(c.out) < MaxSuggestions && offerable(name) {
			c.addText(name, kind, "")
		}
	}
}

func (c *collector) databases() {
	for _, db := range c.snap.Databases {
		c.add(db, KindDatabase, "")
	}
}

func (c *collector) tables(database string) {
	for _, t := range c.snap.Tables[database] {
		kind := KindTable
		switch t.Kind() {
		case "view":
			kind = KindView
		case "dictionary":
			kind = KindDictionary
		}
		c.add(t.Name, kind, t.Engine)
	}
}

func (c *collector) columns(database, table string) {
	for _, col := range c.snap.Columns[metadata.TableKey(database, table)] {
		c.add(col.Name, KindColumn, col.Type)
	}
}

func (c *collector) refColumns(refs []tableRef) {
	for _, ref := range refs {
		c.columns(ref.databaseOr(c.database), ref.Table)
	}
}

func (c *collector) tableAliases(refs []tableRef) {
	for _, ref := range refs {
		if ref.Alias != "" {
			c.add(ref.Alias, KindAlias, ref.Table)
		}
	}
}

func (c *collector) columnAliases(aliases []string) {
	for _, alias := range aliases {
		if !slices.Contains(c.snap.DataTypes, alias) { // CAST(x AS Type) is not an alias
			c.add(alias, KindAlias, "")
		}
	}
}

func (c *collector) functions() {
	for _, f := range c.snap.Functions {
		if !c.matches(f.Name) || len(c.out) >= MaxSuggestions || !offerable(f.Name) {
			continue
		}
		kind := KindFunction
		if f.Aggregate {
			kind = KindAggregate
		}
		c.addText(f.Name, kind, "")
	}
}

// keywords adds keywords in the case the user is typing in.
func (c *collector) keywords() {
	lower := c.word != "" && c.word == strings.ToLower(c.word)
	for _, kw := range c.snap.Keywords {
		if !c.matches(kw) || len(c.out) >= MaxSuggestions {
			continue
		}
		if lower {
			kw = strings.ToLower(kw)
		}
		c.addText(kw, KindKeyword, "")
	}
}

// qualified completes after a dotted prefix.
func (c *collector) qualified(qualifier []string, ctx context, refs []tableRef) {
	if len(qualifier) >= 2 { // db.table.<column>
		c.columns(qualifier[len(qualifier)-2], qualifier[len(qualifier)-1])
		return
	}
	name := qualifier[0]
	if ctx == ctxTable || ctx == ctxTableOrFunction {
		c.tables(name) // db.<table>
		return
	}
	// alias.<column> or table.<column>
	for _, ref := range refs {
		if ref.Alias == name || (ref.Alias == "" && ref.Table == name) {
			c.columns(ref.databaseOr(c.database), ref.Table)
		}
	}
	if len(c.out) == 0 {
		c.columns(c.database, name)
	}
	// db.<table>, on the way to db.table.column
	c.tables(name)
}
