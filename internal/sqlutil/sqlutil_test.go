package sqlutil

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokenizeRoundTrips(t *testing.T) {
	inputs := []string{
		"SELECT count(*) FROM chronicle.events WHERE created_at > now() - INTERVAL 1 HOUR;",
		"select 'it''s', 'a\\'b', `weird name`, \"quoted\" -- trailing comment\n/* block */ FROM t",
		"SELECT 1e-3, 0x1F, 1.5, x::UInt8, a->b, c <=> d",
		"unterminated 'string",
		"unterminated /* comment",
		"# hash comment\nSELECT 1",
		"кириллица, 日本語",
		"",
	}
	for _, in := range inputs {
		var b strings.Builder
		prevEnd := 0
		for _, tok := range Tokenize(in) {
			if tok.Start != prevEnd {
				t.Errorf("%q: gap before token %q", in, tok.Text)
			}
			prevEnd = tok.End
			b.WriteString(tok.Text)
		}
		if b.String() != in {
			t.Errorf("tokens of %q reassemble to %q", in, b.String())
		}
	}
}

func TestTokenizeKinds(t *testing.T) {
	got := SignificantTokens("SELECT `my col`, 'str', 42, foo(x) -- c\nFROM t")
	want := []struct {
		kind TokenKind
		text string
	}{
		{Keyword, "SELECT"}, {QuotedIdent, "`my col`"}, {Operator, ","}, {String, "'str'"}, {Operator, ","},
		{Number, "42"}, {Operator, ","}, {Ident, "foo"}, {Operator, "("}, {Ident, "x"}, {Operator, ")"},
		{Keyword, "FROM"}, {Ident, "t"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tokens, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Text != w.text {
			t.Errorf("token %d = (%v, %q), want (%v, %q)", i, got[i].Kind, got[i].Text, w.kind, w.text)
		}
	}
}

func TestTokenizeUnterminated(t *testing.T) {
	for _, in := range []string{"SELECT 'abc", "SELECT `abc", "SELECT /* abc"} {
		toks := Tokenize(in)
		if last := toks[len(toks)-1]; !last.Unterminated {
			t.Errorf("%q: last token %q not flagged unterminated", in, last.Text)
		}
	}
	// A hash only starts a comment when followed by a space or '!'.
	if toks := SignificantTokens("a #b"); len(toks) != 3 {
		t.Errorf("'#b' must not be a comment, got %+v", toks)
	}
}

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		in    string
		stmts []string
		rest  string
	}{
		{"SELECT 1; SELECT 2", []string{"SELECT 1"}, " SELECT 2"},
		{"SELECT 1;\nSELECT 2;\n", []string{"SELECT 1", "SELECT 2"}, "\n"},
		{"SELECT 'a;b'; SELECT `c;d`;", []string{"SELECT 'a;b'", "SELECT `c;d`"}, ""},
		{"SELECT 1 -- not; a split\n; /* nor; this */ SELECT 2;", []string{"SELECT 1 -- not; a split", "/* nor; this */ SELECT 2"}, ""},
		{";;  ;", nil, ""},
		// ClickHouse nests block comments: everything up to the outer */ is
		// commented out on the server, so it must not be split off and run.
		{"SELECT 1 /* a /* nested */ ; DROP TABLE x; */; SELECT 2;", []string{"SELECT 1 /* a /* nested */ ; DROP TABLE x; */", "SELECT 2"}, ""},
		// After INSERT ... FORMAT comes data, not SQL: nothing in it is a statement.
		{"SELECT 1; INSERT INTO t FORMAT CSV\n1,with love; DROP TABLE x\n;", []string{"SELECT 1", "INSERT INTO t FORMAT CSV\n1,with love; DROP TABLE x\n;"}, ""},
		{"INSERT INTO t VALUES (1, 'a;b'); SELECT 2;", []string{"INSERT INTO t VALUES (1, 'a;b')", "SELECT 2"}, ""},
		{"-- only a comment;\n", nil, "-- only a comment;\n"},
	}
	for _, tt := range tests {
		stmts, rest := SplitStatements(tt.in)
		if !reflect.DeepEqual(stmts, tt.stmts) || rest != tt.rest {
			t.Errorf("SplitStatements(%q) = %q, %q; want %q, %q", tt.in, stmts, rest, tt.stmts, tt.rest)
		}
	}
}

func TestIsComplete(t *testing.T) {
	complete := []string{"SELECT 1;", "SELECT 1;  ", "SELECT 1; -- done", "SELECT 1\\G", "SELECT ';' ;"}
	incomplete := []string{"", "SELECT 1", "SELECT ';", "SELECT 1 -- ;", "SELECT 1; /* open", "SELECT 1 \\ G"}
	for _, in := range complete {
		if !IsComplete(in) {
			t.Errorf("IsComplete(%q) = false", in)
		}
	}
	for _, in := range incomplete {
		if IsComplete(in) {
			t.Errorf("IsComplete(%q) = true", in)
		}
	}
}

func TestParseStatement(t *testing.T) {
	tests := []struct {
		in   string
		want Statement
	}{
		{"SELECT 1;", Statement{SQL: "SELECT 1", Verb: "SELECT"}},
		{"  select 1 ;; ", Statement{SQL: "select 1", Verb: "SELECT"}},
		{"SELECT 1\\G", Statement{SQL: "SELECT 1", Verb: "SELECT", Vertical: true}},
		{"SELECT 1 FORMAT JSONEachRow;", Statement{SQL: "SELECT 1", Verb: "SELECT", Format: "JSONEachRow"}},
		{"SELECT 1 FORMAT Null", Statement{SQL: "SELECT 1", Verb: "SELECT", Format: "Null"}},
		{"SELECT 1 FORMAT Vertical\\G", Statement{SQL: "SELECT 1", Verb: "SELECT", Format: "Vertical", Vertical: true}},
		// A column called "format" is not a FORMAT clause.
		{"SELECT name FROM t ORDER BY format DESC", Statement{SQL: "SELECT name FROM t ORDER BY format DESC", Verb: "SELECT"}},
		// INSERT keeps its FORMAT: it describes the data, not the output.
		{"INSERT INTO t FORMAT CSV", Statement{SQL: "INSERT INTO t FORMAT CSV", Verb: "INSERT"}},
		// SETTINGS may follow the format; only the FORMAT clause is removed.
		{"SELECT 1 FORMAT TSV SETTINGS max_threads = 1;", Statement{SQL: "SELECT 1 SETTINGS max_threads = 1", Verb: "SELECT", Format: "TSV"}},
		{"SELECT 1 SETTINGS max_threads = 1 FORMAT TSV", Statement{SQL: "SELECT 1 SETTINGS max_threads = 1", Verb: "SELECT", Format: "TSV"}},
		// Only statements that return rows have an output format: here
		// "format" is a column name and the statement must stay intact.
		{"ALTER TABLE t ADD COLUMN format String", Statement{SQL: "ALTER TABLE t ADD COLUMN format String", Verb: "ALTER"}},
		{"CREATE TABLE t (format String) ENGINE = Memory", Statement{SQL: "CREATE TABLE t (format String) ENGINE = Memory", Verb: "CREATE"}},
		{"SELECT format x FROM t", Statement{SQL: "SELECT format x FROM t", Verb: "SELECT"}},
		{"SELECT (SELECT 1 FORMAT) AS format", Statement{SQL: "SELECT (SELECT 1 FORMAT) AS format", Verb: "SELECT"}},
		{"-- nothing here", Statement{}},
		{"(SELECT 1) UNION ALL (SELECT 2)", Statement{SQL: "(SELECT 1) UNION ALL (SELECT 2)", Verb: "("}},
	}
	for _, tt := range tests {
		if got := ParseStatement(tt.in); got != tt.want {
			t.Errorf("ParseStatement(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseUse(t *testing.T) {
	tests := []struct {
		in string
		db string
		ok bool
	}{
		{"USE chronicle", "chronicle", true},
		{"use `my-db`", "my-db", true},
		{"USE system", "system", true},
		{"USE", "", false},
		{"USE a b", "", false},
		{"SELECT 1", "", false},
	}
	for _, tt := range tests {
		if db, ok := ParseUse(tt.in); db != tt.db || ok != tt.ok {
			t.Errorf("ParseUse(%q) = %q, %v; want %q, %v", tt.in, db, ok, tt.db, tt.ok)
		}
	}
}

func TestParseSet(t *testing.T) {
	got, ok := ParseSet("SET max_threads = 4, join_algorithm = 'hash', allow_x = true")
	want := map[string]string{"max_threads": "4", "join_algorithm": "hash", "allow_x": "true"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseSet = %v, %v; want %v", got, ok, want)
	}
	// Everything that is not a plain setting assignment goes to the server as is.
	signed, ok := ParseSet("SET distributed_ddl_task_timeout = -1, x = +2")
	if !ok || signed["distributed_ddl_task_timeout"] != "-1" || signed["x"] != "+2" {
		t.Errorf("signed values = %v, %v", signed, ok)
	}
	for _, in := range []string{"SET ROLE admin", "SET DEFAULT ROLE x TO y", "SET param_id = 5", "SET x = 1 + 2", "SET x", "SET x = -y", "SET x = -"} {
		if _, ok := ParseSet(in); ok {
			t.Errorf("ParseSet(%q) should not be handled client-side", in)
		}
	}
}

func TestReturnsRows(t *testing.T) {
	for _, verb := range []string{"SELECT", "WITH", "SHOW", "DESCRIBE", "DESC", "EXISTS", "EXPLAIN", "("} {
		if !ReturnsRows(verb) {
			t.Errorf("ReturnsRows(%q) = false", verb)
		}
	}
	for _, verb := range []string{"CREATE", "DROP", "ALTER", "INSERT", "SYSTEM", "GRANT", "OPTIMIZE", ""} {
		if ReturnsRows(verb) {
			t.Errorf("ReturnsRows(%q) = true", verb)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	tests := map[string]string{
		"events":    "events",
		"_a1":       "_a1",
		"таблица":   "таблица",
		"my-db":     "`my-db`",
		"1abc":      "`1abc`",
		"has space": "`has space`",
		"back`tick": "`back\\`tick`",
		"":          "``",
	}
	for in, want := range tests {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", in, got, want)
		}
		if got := Unquote(QuoteIdent(in)); got != in {
			t.Errorf("Unquote(QuoteIdent(%q)) = %q", in, got)
		}
	}
}

func TestNestedBlockComments(t *testing.T) {
	toks := SignificantTokens("SELECT /* outer /* inner */ still a comment */ 1")
	if len(toks) != 2 || toks[1].Text != "1" {
		t.Errorf("tokens = %+v", toks)
	}
	all := Tokenize("/* open /* closed */ never closed")
	if len(all) != 1 || !all[0].Unterminated {
		t.Errorf("an unbalanced nested comment must be unterminated: %+v", all)
	}
	if IsComplete("SELECT 1 /* /* */ ;") {
		t.Error("a semicolon inside an open nested comment does not end the statement")
	}
}

func TestClassifyInsert(t *testing.T) {
	tests := map[string]InsertKind{
		"INSERT INTO t VALUES (1, 'a')":                          InsertValues,
		"INSERT INTO db.t (a, b) VALUES (1, 2), (3, 4)":          InsertValues,
		"INSERT INTO t SETTINGS async_insert = 1 VALUES (1)":     InsertValues,
		"INSERT INTO t SELECT * FROM s":                          InsertSelect,
		"INSERT INTO t (a) WITH x AS (SELECT 1) SELECT * FROM x": InsertSelect,
		"INSERT INTO t (SELECT 1)":                               InsertSelect,
		"INSERT INTO TABLE FUNCTION s3('u') SELECT 1":            InsertSelect,
		"INSERT INTO format VALUES (1)":                          InsertValues, // a table called "format"
		"INSERT INTO t (format, values) VALUES (1, 2)":           InsertValues,
		"INSERT INTO t FORMAT CSV":                               InsertFormat,
		"INSERT INTO t (a, b) FORMAT JSONEachRow":                InsertFormat,
		"INSERT INTO t FORMAT Values (1)":                        InsertFormat,
		// Data that mentions SQL keywords does not make the INSERT self-contained.
		"INSERT INTO t FORMAT CSV\n1,with love and select values": InsertFormat,
		// The piping idiom: the data would come on standard input.
		"INSERT INTO t VALUES":     InsertNoData,
		"INSERT INTO t (a) VALUES": InsertNoData,
		"INSERT INTO t":            InsertNoData,
	}
	for sql, want := range tests {
		if got := ClassifyInsert(SignificantTokens(sql)); got != want {
			t.Errorf("ClassifyInsert(%q) = %v, want %v", sql, got, want)
		}
	}
}
