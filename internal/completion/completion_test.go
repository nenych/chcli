package completion

import (
	"slices"
	"strings"
	"testing"

	"github.com/nenych/chcli/internal/metadata"
	"github.com/nenych/chcli/internal/sqlutil"
)

func testSnapshot() *metadata.Snapshot {
	return &metadata.Snapshot{
		Databases: []string{"chronicle", "chronicle_draft", "default", "my-db", "system"},
		Tables: map[string][]metadata.Table{
			"chronicle": {
				{Database: "chronicle", Name: "events", Engine: "MergeTree"},
				{Database: "chronicle", Name: "events_by_day", Engine: "MaterializedView"},
				{Database: "chronicle", Name: "users", Engine: "MergeTree"},
				{Database: "chronicle", Name: "user dict", Engine: "Dictionary"},
			},
			"default": {{Database: "default", Name: "t", Engine: "Memory"}},
			"system": {
				{Database: "system", Name: "tables", Engine: "SystemTables"},
				{Database: "system", Name: "parts", Engine: "SystemParts"},
			},
		},
		Columns: map[string][]metadata.Column{
			metadata.TableKey("chronicle", "events"): {
				{Name: "event_id", Type: "UInt64"}, {Name: "event_type", Type: "String"},
				{Name: "created_at", Type: "DateTime"}, {Name: "user_id", Type: "UInt64"},
			},
			metadata.TableKey("chronicle", "users"): {
				{Name: "user_id", Type: "UInt64"}, {Name: "email", Type: "String"}, {Name: "full name", Type: "String"},
			},
			metadata.TableKey("default", "t"):     {{Name: "x", Type: "UInt8"}, {Name: "event_ts", Type: "DateTime"}},
			metadata.TableKey("system", "tables"): {{Name: "database", Type: "String"}, {Name: "name", Type: "String"}, {Name: "engine", Type: "String"}},
			metadata.TableKey("system", "parts"):  {{Name: "database", Type: "String"}, {Name: "table", Type: "String"}, {Name: "partition", Type: "String"}, {Name: "rows", Type: "UInt64"}},
		},
		Functions: []metadata.Function{
			{Name: "count", Aggregate: true}, {Name: "countIf", Aggregate: true}, {Name: "empty"},
			{Name: "evalMLMethod"}, {Name: "toDate"}, {Name: "toDateTime"}, {Name: "uniq", Aggregate: true},
		},
		TableFunctions: []string{"numbers", "remote", "s3", "url"},
		DataTypes:      []string{"DateTime", "String", "UInt64", "UInt8"},
		Engines:        []string{"Memory", "MergeTree", "ReplacingMergeTree"},
		Formats:        []string{"CSV", "JSONEachRow", "Parquet"},
		Settings:       []string{"max_memory_usage", "max_threads", "use_uncompressed_cache"},
		Keywords:       sqlutil.Keywords,
	}
}

var testEngine = &Engine{
	OutputFormats: []string{"table", "vertical", "json"},
	Commands: []MetaCommand{
		{Name: `\d`, Help: "Describe a table", Arg: ArgTable},
		{Name: `\dt`, Help: "List tables", Arg: ArgDatabase},
		{Name: `\use`, Help: "Switch database", Arg: ArgDatabase},
		{Name: `\format`, Help: "Output format", Arg: ArgFormat},
		{Name: `\refresh`, Help: "Reload metadata"},
	},
}

// complete runs the engine on input, where "|" marks the cursor.
func complete(t *testing.T, input, database string) (Result, string) {
	t.Helper()
	cursor := strings.Index(input, "|")
	if cursor < 0 {
		t.Fatalf("no cursor marker in %q", input)
	}
	text := input[:cursor] + input[cursor+1:]
	res := testEngine.Complete(text, cursor, testSnapshot(), database)
	return res, text[res.Start:cursor]
}

func texts(res Result) []string {
	out := make([]string, len(res.Suggestions))
	for i, s := range res.Suggestions {
		out[i] = s.Text
	}
	return out
}

func kindOf(res Result, text string) Kind {
	for _, s := range res.Suggestions {
		if s.Text == text {
			return s.Kind
		}
	}
	return ""
}

func TestComplete(t *testing.T) {
	tests := []struct {
		name  string
		input string
		db    string
		// exact, when set, is the full expected suggestion list in order.
		exact []string
		// first, when set, must be the leading suggestions in order.
		first []string
		// has / hasNot are suggestions that must / must not be offered.
		has    []string
		hasNot []string
		word   string // expected word being replaced
	}{
		// The examples from the product brief.
		{name: "databases and tables after FROM", input: "SELECT *\nFROM chr|", db: "default",
			exact: []string{"chronicle", "chronicle_draft"}, word: "chr"},
		{name: "columns of the FROM table are prioritised", input: "SELECT *\nFROM chronicle.events\nWHERE ev|", db: "default",
			first: []string{"event_id", "event_type"}, has: []string{"evalMLMethod"}, word: "ev"},
		{name: "alias resolves to its table", input: "SELECT * FROM chronicle.events e WHERE e.|", db: "default",
			exact: []string{"event_id", "event_type", "created_at", "user_id"}, word: ""},
		{name: "alias with prefix", input: "SELECT * FROM chronicle.events AS e WHERE e.ev|", db: "default",
			exact: []string{"event_id", "event_type"}, word: "ev"},

		// Tables.
		{name: "tables of the current database", input: "SELECT * FROM |", db: "chronicle",
			first: []string{"events", "events_by_day", "users"}, has: []string{"chronicle", "system"}, hasNot: []string{"count", "SELECT"}},
		{name: "tables of a qualified database", input: "SELECT * FROM chronicle.|", db: "default",
			exact: []string{"events", "events_by_day", "users"}},
		{name: "table functions in FROM", input: "SELECT * FROM nu|", db: "default", exact: []string{"numbers"}},
		{name: "JOIN wants a table", input: "SELECT * FROM chronicle.events e JOIN chronicle.u|", db: "default", exact: []string{"users"}},
		{name: "comma-separated FROM list", input: "SELECT * FROM chronicle.events, chronicle.|", db: "default",
			exact: []string{"events", "events_by_day", "users"}},
		{name: "after the table comes a keyword", input: "SELECT * FROM chronicle.events WH|", db: "default",
			exact: []string{"WHEN", "WHERE"}},
		{name: "INSERT INTO", input: "INSERT INTO ev|", db: "chronicle", exact: []string{"events", "events_by_day"}},
		{name: "INSERT column list", input: "INSERT INTO chronicle.events (event_id, |", db: "default",
			exact: []string{"event_id", "event_type", "created_at", "user_id"}},
		{name: "DESCRIBE", input: "DESCRIBE TABLE us|", db: "chronicle", exact: []string{"users"}},
		{name: "DROP TABLE", input: "DROP TABLE IF EXISTS chronicle.ev|", db: "default", exact: []string{"events", "events_by_day"}},
		{name: "keyword-named system tables", input: "SELECT * FROM system.ta|", db: "default", exact: []string{"tables"}},

		// Columns.
		{name: "columns before FROM is typed further", input: "SELECT ev| FROM chronicle.events", db: "default",
			first: []string{"event_id", "event_type"}},
		{name: "empty word lists columns then aliases", input: "SELECT | FROM chronicle.events e", db: "default",
			exact: []string{"event_id", "event_type", "created_at", "user_id", "e"}},
		{name: "unqualified table uses the current database", input: "SELECT x FROM t WHERE ev|", db: "default",
			first: []string{"event_ts"}},
		{name: "table name as qualifier", input: "SELECT events.| FROM chronicle.events", db: "default",
			exact: []string{"event_id", "event_type", "created_at", "user_id"}},
		{name: "db.table.column", input: "SELECT chronicle.users.e|", db: "default", exact: []string{"email"}},
		{name: "columns of joined tables", input: "SELECT * FROM chronicle.events e JOIN chronicle.users u ON e.user_id = u.|", db: "default",
			exact: []string{"user_id", "email"}},
		{name: "duplicate column names are offered once", input: "SELECT user| FROM chronicle.events, chronicle.users", db: "default",
			first: []string{"user_id"}, hasNot: []string{}},
		{name: "GROUP BY", input: "SELECT count() FROM chronicle.events GROUP BY event_t|", db: "default", exact: []string{"event_type"}},
		{name: "inside a function call", input: "SELECT count(ev| FROM chronicle.events", db: "default", first: []string{"event_id", "event_type"}},
		{name: "subquery sees its own clause", input: "SELECT * FROM (SELECT ev| FROM chronicle.events)", db: "default",
			first: []string{"event_id", "event_type"}},
		{name: "ALTER ... DROP COLUMN", input: "ALTER TABLE chronicle.events DROP COLUMN |", db: "default",
			exact: []string{"event_id", "event_type", "created_at", "user_id"}},

		// Keyword-named columns of system tables must not derail the context.
		{name: "columns named like keywords", input: "SELECT database, table, | FROM system.parts", db: "default",
			exact: []string{"database", "table", "partition", "rows"}},
		{name: "filter on a column named engine", input: "SELECT name FROM system.tables WHERE engine = 'x' AND da|", db: "default",
			first: []string{"database"}},

		// Functions, aliases, keywords.
		{name: "functions", input: "SELECT toD|", db: "default", exact: []string{"toDate", "toDateTime"}},
		{name: "column alias", input: "SELECT count() AS total FROM t ORDER BY tot|", db: "default", first: []string{"total"}},
		{name: "CAST target is not an alias", input: "SELECT CAST(x AS UInt64) FROM t WHERE UI|", db: "default", hasNot: []string{"UInt64"}},
		{name: "statement start", input: "sel|", db: "default", exact: []string{"select"}},
		{name: "keywords follow the typed case", input: "SEL|", db: "default", exact: []string{"SELECT"}},
		{name: "second statement is independent", input: "SELECT 1 FROM chronicle.events; SELECT ev|", db: "default",
			hasNot: []string{"event_id"}, has: []string{"evalMLMethod"}},
		{name: "nothing on an empty expression without tables", input: "SELECT |", db: "default", exact: []string{}},

		// Other object kinds.
		{name: "USE", input: "USE ch|", db: "default", exact: []string{"chronicle", "chronicle_draft"}},
		{name: "DROP DATABASE", input: "DROP DATABASE sy|", db: "default", exact: []string{"system"}},
		{name: "engines", input: "CREATE TABLE x (a UInt8) ENGINE = M|", db: "default", exact: []string{"Memory", "MergeTree"}},
		{name: "types in CREATE TABLE", input: "CREATE TABLE x (a UInt8, b S|", db: "default", exact: []string{"String"}},
		{name: "types after ::", input: "SELECT x::U|", db: "default", exact: []string{"UInt64", "UInt8"}},
		{name: "types in CAST", input: "SELECT CAST(x AS D|", db: "default", exact: []string{"DateTime"}},
		{name: "types after ADD COLUMN", input: "ALTER TABLE t ADD COLUMN c Da|", db: "default", exact: []string{"DateTime"}},
		{name: "formats", input: "SELECT 1 FORMAT J|", db: "default", exact: []string{"json", "JSONEachRow"}},
		{name: "SET", input: "SET max_|", db: "default", exact: []string{"max_memory_usage", "max_threads"}},
		{name: "SETTINGS clause", input: "SELECT 1 SETTINGS max_threads = 1, use|", db: "default", exact: []string{"use_uncompressed_cache"}},
		{name: "no alias suggestions after AS", input: "SELECT count() AS c|", db: "default", exact: []string{}},

		// Places where nothing must be suggested.
		{name: "inside a string", input: "SELECT 'FROM chr|", db: "default", exact: []string{}},
		{name: "inside a closed string", input: "SELECT 'sel|' FROM t", db: "default", exact: []string{}},
		{name: "inside a comment", input: "SELECT 1 -- FROM chr|", db: "default", exact: []string{}},
		{name: "inside a quoted identifier", input: "SELECT `ev| FROM chronicle.events", db: "default", exact: []string{}},
		{name: "typing a number", input: "SELECT 12|", db: "default", exact: []string{}},
		{name: "the \\G terminator", input: "SELECT 1\\G|", db: "default", exact: []string{}},

		// Meta commands.
		{name: "meta command names", input: `\d|`, db: "default", exact: []string{`\d`, `\dt`}, word: `\d`},
		{name: "meta command table argument", input: `\d ev|`, db: "chronicle", exact: []string{"events", "events_by_day"}, word: "ev"},
		{name: "meta command qualified table", input: `\d chronicle.u|`, db: "default", exact: []string{"users"}},
		{name: "meta command database argument", input: `\use chronicle_|`, db: "default", exact: []string{"chronicle_draft"}},
		{name: "meta command format argument", input: `\format |`, db: "default", exact: []string{"table", "vertical", "json"}},
		{name: "meta command without arguments", input: `\refresh |`, db: "default", exact: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, word := complete(t, tt.input, tt.db)
			got := texts(res)
			if tt.exact != nil && !slices.Equal(got, tt.exact) {
				t.Errorf("suggestions = %q, want %q", got, tt.exact)
			}
			if len(tt.first) > 0 && (len(got) < len(tt.first) || !slices.Equal(got[:len(tt.first)], tt.first)) {
				t.Errorf("suggestions = %q, want them to start with %q", got, tt.first)
			}
			for _, want := range tt.has {
				if !slices.Contains(got, want) {
					t.Errorf("suggestions %q lack %q", got, want)
				}
			}
			for _, unwanted := range tt.hasNot {
				if slices.Contains(got, unwanted) {
					t.Errorf("suggestions %q must not contain %q", got, unwanted)
				}
			}
			if tt.word != "" && word != tt.word {
				t.Errorf("replaced word = %q, want %q", word, tt.word)
			}
			seen := map[string]bool{}
			for _, s := range res.Suggestions {
				key := string(s.Kind) + "/" + s.Text
				if seen[key] {
					t.Errorf("duplicate suggestion %s", key)
				}
				seen[key] = true
			}
		})
	}
}

func TestSuggestionKindsAndDetails(t *testing.T) {
	res, _ := complete(t, "SELECT * FROM |", "chronicle")
	for text, kind := range map[string]Kind{"events": KindTable, "events_by_day": KindView, "chronicle": KindDatabase} {
		if got := kindOf(res, text); got != kind {
			t.Errorf("kind of %s = %q, want %q", text, got, kind)
		}
	}
	res, _ = complete(t, "SELECT co| FROM chronicle.events", "default")
	if kindOf(res, "count") != KindAggregate {
		t.Errorf("count should be an aggregate function: %+v", res.Suggestions)
	}
	res, _ = complete(t, "SELECT ev| FROM chronicle.events", "default")
	if s := res.Suggestions[0]; s.Kind != KindColumn || s.Detail != "UInt64" {
		t.Errorf("column suggestion = %+v", s)
	}
}

// Names that need quoting are quoted; those the line editor could not insert
// correctly (they contain a word separator) are not offered at all.
func TestIdentifiersNeedingQuotes(t *testing.T) {
	snap := testSnapshot()
	snap.Tables["chronicle"] = append(snap.Tables["chronicle"],
		metadata.Table{Database: "chronicle", Name: "2024_archive", Engine: "MergeTree"},
		metadata.Table{Database: "chronicle", Name: "таблица", Engine: "MergeTree"})
	res := testEngine.Complete("SELECT * FROM ", 14, snap, "chronicle")
	got := texts(res)
	if !slices.Contains(got, "`2024_archive`") || !slices.Contains(got, "таблица") {
		t.Errorf("suggestions = %q", got)
	}
	for _, s := range got {
		if strings.ContainsAny(s, WordSeparators) {
			t.Errorf("suggestion %q contains a word separator", s)
		}
	}
	if slices.Contains(got, "`user dict`") || slices.Contains(got, "`my-db`") {
		t.Errorf("names with separators must not be offered: %q", got)
	}
}

func TestSuggestionLimit(t *testing.T) {
	snap := testSnapshot()
	for i := range 2 * MaxSuggestions {
		snap.Settings = append(snap.Settings, "setting_"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+strings.Repeat("y", i/26))
	}
	res := testEngine.Complete("SET ", 4, snap, "default")
	if len(res.Suggestions) != MaxSuggestions {
		t.Errorf("got %d suggestions, want the cap of %d", len(res.Suggestions), MaxSuggestions)
	}
}

func TestCompleteNeverPanicsOnPartialInput(t *testing.T) {
	inputs := []string{
		"", " ", ".", "..", "a.", ".a", "a..b", "(", ")", "((", "SELECT (", "SELECT )", "FROM", "FROM ,", "FROM a,",
		"SELECT * FROM a.b.c.d.", "\\", "\\ ", "\\d ", "\\d a.b.c", ";", ";;", "SELECT 1;", "'", "`", "/*", "--",
		"SELECT * FROM t AS", "INSERT INTO", "INSERT INTO t (", "CREATE TABLE", "CREATE TABLE t (", "ENGINE =", "x::", "é", "日本.",
	}
	snap := testSnapshot()
	for _, in := range inputs {
		for cursor := 0; cursor <= len(in); cursor++ {
			if !strings.HasPrefix(in[cursor:], "") || !isRuneBoundary(in, cursor) {
				continue
			}
			res := testEngine.Complete(in, cursor, snap, "default")
			if res.Start < 0 || res.Start > cursor {
				t.Errorf("Complete(%q, %d): Start = %d out of range", in, cursor, res.Start)
			}
		}
	}
}

func isRuneBoundary(s string, i int) bool {
	return i == 0 || i == len(s) || (s[i]&0xC0) != 0x80
}

func TestExtractTables(t *testing.T) {
	refs, aliases := extractTables(sqlutil.SignificantTokens(
		"SELECT a AS initial, count() AS total FROM db1.t1 AS x, t2 y LEFT JOIN `my db`.`t 3` z ON 1 WHERE b IN (SELECT c FROM numbers(10))"))
	want := []tableRef{{Database: "db1", Table: "t1", Alias: "x"}, {Table: "t2", Alias: "y"}, {Database: "my db", Table: "t 3", Alias: "z"}}
	if !slices.Equal(refs, want) {
		t.Errorf("refs = %+v, want %+v", refs, want)
	}
	if !slices.Equal(aliases, []string{"initial", "total"}) {
		t.Errorf("column aliases = %v", aliases)
	}
}
