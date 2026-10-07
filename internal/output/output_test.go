package output

import (
	"bytes"
	"encoding/json"
	"math"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var (
	testCols = []Column{
		{Name: "id", Type: "UInt64"},
		{Name: "name", Type: "String"},
		{Name: "score", Type: "Nullable(Float64)"},
		{Name: "created", Type: "DateTime"},
	}
	testTime = time.Date(2026, 1, 2, 3, 4, 5, 678000000, time.UTC)
	testRows = [][]any{
		{uint64(1), "alice", 1.5, testTime},
		{uint64(20), "tab\there", nil, testTime},
	}
)

func render(t *testing.T, format string, cols []Column, rows [][]any) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := New(format, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Begin(cols); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := w.Row(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.End(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestTable(t *testing.T) {
	got := render(t, Table, testCols, testRows)
	want := "" +
		"┌─id─┬─name──────┬─score─┬─created─────────────┐\n" +
		"│  1 │ alice     │   1.5 │ 2026-01-02 03:04:05 │\n" +
		"│ 20 │ tab\\there │  ᴺᵁᴸᴸ │ 2026-01-02 03:04:05 │\n" +
		"└────┴───────────┴───────┴─────────────────────┘\n"
	if got != want {
		t.Errorf("table output:\n%s\nwant:\n%s", got, want)
	}
}

// The example from the product brief.
func TestTableSingleValue(t *testing.T) {
	got := render(t, Table, []Column{{Name: "count()", Type: "UInt64"}}, [][]any{{uint64(1382991)}})
	want := "" +
		"┌─count()─┐\n" +
		"│ 1382991 │\n" +
		"└─────────┘\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableWideCharactersAndEmptyResult(t *testing.T) {
	got := render(t, Table, []Column{{Name: "s", Type: "String"}}, [][]any{{"日本"}, {"ab"}})
	want := "" +
		"┌─s────┐\n" +
		"│ 日本 │\n" +
		"│ ab   │\n" +
		"└──────┘\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if out := render(t, Table, testCols, nil); out != "" {
		t.Errorf("an empty result must print nothing, got %q", out)
	}
}

// Data must not be able to inject terminal escape sequences.
func TestTableEscapesControlCharacters(t *testing.T) {
	got := render(t, Table, []Column{{Name: "s", Type: "String"}}, [][]any{{"a\x1b[31mred\nline\r\x00"}})
	if strings.ContainsAny(got, "\x1b\r\x00") || strings.Count(got, "\n") != 3 {
		t.Errorf("control characters leaked into table output: %q", got)
	}
	if !strings.Contains(got, `a\x1b[31mred\nline\r\x00`) {
		t.Errorf("escaped form missing: %q", got)
	}
}

func TestVertical(t *testing.T) {
	got := render(t, Vertical, testCols, testRows)
	want := "" +
		"Row 1:\n" +
		"──────\n" +
		"id:      1\n" +
		"name:    alice\n" +
		"score:   1.5\n" +
		"created: 2026-01-02 03:04:05\n" +
		"\n" +
		"Row 2:\n" +
		"──────\n" +
		"id:      20\n" +
		"name:    tab\\there\n" +
		"score:   ᴺᵁᴸᴸ\n" +
		"created: 2026-01-02 03:04:05\n"
	if got != want {
		t.Errorf("vertical output:\n%s\nwant:\n%s", got, want)
	}
}

func TestTSV(t *testing.T) {
	got := render(t, TSV, testCols, testRows)
	want := "1\talice\t1.5\t2026-01-02 03:04:05\n20\ttab\\there\t\\N\t2026-01-02 03:04:05\n"
	if got != want {
		t.Errorf("tsv = %q, want %q", got, want)
	}
	withNames := render(t, TSVWithNames, testCols, testRows[:1])
	if !strings.HasPrefix(withNames, "id\tname\tscore\tcreated\n1\talice") {
		t.Errorf("tsvwithnames = %q", withNames)
	}
	// Newlines and backslashes must not break the one-row-per-line contract.
	tricky := render(t, TSV, []Column{{Name: "s", Type: "String"}}, [][]any{{"a\nb\\c"}})
	if tricky != "a\\nb\\\\c\n" {
		t.Errorf("tsv escaping = %q", tricky)
	}
}

func TestCSV(t *testing.T) {
	cols := []Column{{Name: "a", Type: "String"}, {Name: "b", Type: "Nullable(String)"}, {Name: "c", Type: "Array(String)"}}
	rows := [][]any{{"x,y", nil, []string{"p", "q"}}, {`say "hi"`, "multi\nline", []string{}}}
	got := render(t, CSVWithNames, cols, rows)
	want := "a,b,c\n\"x,y\",\\N,\"['p','q']\"\n\"say \"\"hi\"\"\",\"multi\nline\",[]\n"
	if got != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
	if plain := render(t, CSV, cols, rows[:1]); strings.HasPrefix(plain, "a,b,c") {
		t.Errorf("csv must not have a header: %q", plain)
	}
}

func TestJSONDocument(t *testing.T) {
	got := render(t, JSON, testCols, testRows)
	var doc struct {
		Meta []struct{ Name, Type string }
		Data []map[string]any
		Rows int
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	if doc.Rows != 2 || len(doc.Data) != 2 || len(doc.Meta) != 4 || doc.Meta[2].Type != "Nullable(Float64)" {
		t.Errorf("document = %+v", doc)
	}
	if doc.Data[0]["name"] != "alice" || doc.Data[0]["id"] != float64(1) || doc.Data[1]["score"] != nil ||
		doc.Data[0]["created"] != "2026-01-02 03:04:05" {
		t.Errorf("data = %v", doc.Data)
	}
	// Keys keep the column order.
	if !strings.Contains(got, `{"id":1,"name":"alice","score":1.5,"created":"2026-01-02 03:04:05"}`) {
		t.Errorf("row object not in column order:\n%s", got)
	}

	empty := render(t, JSON, testCols, nil)
	if err := json.Unmarshal([]byte(empty), &doc); err != nil || doc.Rows != 0 {
		t.Errorf("empty result is not a valid document: %v\n%s", err, empty)
	}
}

func TestJSONLines(t *testing.T) {
	cols := []Column{{Name: "n", Type: "UInt64"}, {Name: "f", Type: "Float64"}, {Name: "m", Type: "Map(String, UInt8)"}, {Name: "h", Type: "String"}}
	got := render(t, JSONLines, cols, [][]any{
		{uint64(18446744073709551615), math.NaN(), map[string]uint8{"k": 1}, "<a&b>"},
		{uint64(0), math.Inf(1), map[string]uint8{}, ""},
	})
	want := `{"n":18446744073709551615,"f":null,"m":{"k":1},"h":"<a&b>"}` + "\n" + `{"n":0,"f":null,"m":{},"h":""}` + "\n"
	if got != want {
		t.Errorf("jsonlines:\n%s\nwant:\n%s", got, want)
	}
}

func TestValueFormatting(t *testing.T) {
	str := "x"
	tests := []struct {
		typ  string
		v    any
		want string
	}{
		{"String", "plain 'text'", "plain 'text'"},
		{"Array(String)", []string{"a", "it's"}, `['a','it\'s']`},
		{"Array(Nullable(String))", []*string{&str, nil}, "['x',NULL]"},
		{"Array(Array(UInt8))", [][]uint8{{1, 2}, {}}, "[[1,2],[]]"},
		{"Map(String, UInt64)", map[string]uint64{"b": 2, "a": 1}, "{'a':1,'b':2}"},
		{"Tuple(UInt8, String)", []any{uint8(1), "x"}, "(1,'x')"},
		{"Date", testTime, "2026-01-02"},
		{"Nullable(Date32)", testTime, "2026-01-02"},
		{"DateTime('UTC')", testTime, "2026-01-02 03:04:05"},
		{"DateTime64(3)", testTime, "2026-01-02 03:04:05.678"},
		{"DateTime64(6, 'UTC')", testTime, "2026-01-02 03:04:05.678000"},
		{"Array(DateTime)", []time.Time{testTime}, "['2026-01-02 03:04:05']"},
		{"Bool", true, "true"},
		{"Float64", math.Inf(-1), "-inf"},
		{"Float64", math.NaN(), "nan"},
		{"Float32", float32(0.1), "0.1"},
		{"Int8", int8(-5), "-5"},
		{"IPv4", netip.MustParseAddr("10.0.0.1"), "10.0.0.1"},
		{"Array(UInt8)", []uint8{1, 2}, "[1,2]"},
	}
	for _, tt := range tests {
		got, ok := newColFormat(Column{Type: tt.typ}).text(tt.v)
		if !ok || got != tt.want {
			t.Errorf("%s %v = %q (ok=%v), want %q", tt.typ, tt.v, got, ok, tt.want)
		}
	}
	if _, ok := newColFormat(Column{Type: "Nullable(String)"}).text(nil); ok {
		t.Error("NULL must be reported as such")
	}
}

func TestNumericAlignment(t *testing.T) {
	for typ, numeric := range map[string]bool{
		"UInt64": true, "Nullable(Int8)": true, "LowCardinality(Nullable(Float32))": true, "Decimal(10, 2)": true,
		"String": false, "Array(UInt8)": false, "DateTime": false, "IPv4": false,
	} {
		if got := newColFormat(Column{Type: typ}).numeric; got != numeric {
			t.Errorf("numeric(%s) = %v, want %v", typ, got, numeric)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"table": Table, "TABLE": Table, "PrettyCompact": Table, "Vertical": Vertical, "TabSeparated": TSV,
		"TSVWithNames": TSVWithNames, "CSV": CSV, "CSVWithNames": CSVWithNames, "JSON": JSON,
		"JSONEachRow": JSONLines, "jsonl": JSONLines, "Null": Null,
	} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Normalize("Parquet"); err == nil || !strings.Contains(err.Error(), "supported: table") {
		t.Errorf("unsupported format error = %v", err)
	}
	if _, err := New("nope", &bytes.Buffer{}); err == nil {
		t.Error("New must reject unknown formats")
	}
}

func TestNullFormatDiscardsEverything(t *testing.T) {
	if out := render(t, Null, testCols, testRows); out != "" {
		t.Errorf("null format wrote %q", out)
	}
}

// Results larger than the chunk size are rendered as consecutive tables, so
// memory use does not grow with the result.
func TestTableChunking(t *testing.T) {
	rows := make([][]any, tableChunkRows+1)
	for i := range rows {
		rows[i] = []any{uint64(i)}
	}
	got := render(t, Table, []Column{{Name: "n", Type: "UInt64"}}, rows)
	if n := strings.Count(got, "┌─n"); n != 2 {
		t.Errorf("expected 2 tables, got %d", n)
	}
	if n := strings.Count(got, "│"); n != 2*(tableChunkRows+1) {
		t.Errorf("row count wrong: %d cell borders", n)
	}
}

// WITH TOTALS produces one extra row, which every format with a place for it shows.
func TestTotals(t *testing.T) {
	cols := []Column{{Name: "k", Type: "String"}, {Name: "n", Type: "UInt64"}}
	rows := [][]any{{"a", uint64(1)}, {"b", uint64(2)}}
	totals := []any{"", uint64(3)}
	withTotals := func(format string) string {
		var buf bytes.Buffer
		w, err := New(format, &buf)
		if err != nil {
			t.Fatal(err)
		}
		w.Begin(cols)
		for _, r := range rows {
			w.Row(r)
		}
		if tw, ok := w.(TotalsWriter); ok {
			if err := tw.Totals(totals); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.End(); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	wantTable := "" +
		"┌─k─┬─n─┐\n" +
		"│ a │ 1 │\n" +
		"│ b │ 2 │\n" +
		"└───┴───┘\n" +
		"\nTotals:\n" +
		"┌─k─┬─n─┐\n" +
		"│   │ 3 │\n" +
		"└───┴───┘\n"
	if got := withTotals(Table); got != wantTable {
		t.Errorf("table:\n%s\nwant:\n%s", got, wantTable)
	}
	if got := withTotals(Vertical); !strings.HasSuffix(got, "\nTotals:\n───────\nk: \nn: 3\n") {
		t.Errorf("vertical:\n%s", got)
	}
	if got, want := withTotals(TSV), "a\t1\nb\t2\n\n\t3\n"; got != want {
		t.Errorf("tsv = %q, want %q", got, want)
	}
	if got, want := withTotals(CSV), "a,1\nb,2\n\n,3\n"; got != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
	var doc struct {
		Data   []map[string]any
		Totals map[string]any
		Rows   int
	}
	if err := json.Unmarshal([]byte(withTotals(JSON)), &doc); err != nil || doc.Totals["n"] != float64(3) || doc.Rows != 2 || len(doc.Data) != 2 {
		t.Errorf("json document = %+v, %v", doc, err)
	}
	if got, want := withTotals(JSONLines), "{\"k\":\"a\",\"n\":1}\n{\"k\":\"b\",\"n\":2}\n"; got != want {
		t.Errorf("jsonlines = %q, want %q", got, want)
	}
}

func TestVerticalEscapesColumnNames(t *testing.T) {
	got := render(t, Vertical, []Column{{Name: "a\x1b[31m\nb", Type: "UInt8"}}, [][]any{{uint8(1)}})
	if strings.Contains(got, "\x1b") || !strings.Contains(got, `a\x1b[31m\nb: 1`) {
		t.Errorf("column name not escaped: %q", got)
	}
}
