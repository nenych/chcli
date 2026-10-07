// Package output renders query results. Writers are streaming: rows are
// written as they arrive and nothing is held back beyond what a format needs
// to lay itself out.
package output

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Column describes a result column.
type Column struct {
	Name string
	Type string // ClickHouse type name
}

// Writer renders one result set.
type Writer interface {
	// Begin is called once, before the first row.
	Begin(cols []Column) error
	// Row writes one row. The values slice may be reused by the caller.
	Row(values []any) error
	// End finishes the result set and flushes buffered output.
	End() error
}

// TotalsWriter is implemented by writers that can show the extra row of a
// WITH TOTALS query. Totals is called at most once, after the last Row and
// before End.
type TotalsWriter interface {
	Totals(values []any) error
}

// Output format names.
const (
	Table        = "table"
	Vertical     = "vertical"
	TSV          = "tsv"
	TSVWithNames = "tsvwithnames"
	CSV          = "csv"
	CSVWithNames = "csvwithnames"
	JSON         = "json"
	JSONLines    = "jsonlines"
	Null         = "null"
)

// Formats lists the supported formats in display order.
var Formats = []string{Table, Vertical, TSV, TSVWithNames, CSV, CSVWithNames, JSON, JSONLines, Null}

// aliases maps ClickHouse format names (lower-cased) onto ours, so that both
// --format and a trailing FORMAT clause accept the spellings users know.
var aliases = map[string]string{
	"pretty":                 Table,
	"prettycompact":          Table,
	"prettycompactmonoblock": Table,
	"prettymonoblock":        Table,
	"prettynoescapes":        Table,
	"prettycompactnoescapes": Table,
	"prettyspace":            Table,
	"tabseparated":           TSV,
	"tabseparatedwithnames":  TSVWithNames,
	"tsv-with-names":         TSVWithNames,
	"csv-with-names":         CSVWithNames,
	"jsoneachrow":            JSONLines,
	"ndjson":                 JSONLines,
	"jsonl":                  JSONLines,
}

// Normalize maps a user-supplied format name to a canonical one.
func Normalize(name string) (string, error) {
	lower := strings.ToLower(name)
	if canonical, ok := aliases[lower]; ok {
		return canonical, nil
	}
	for _, f := range Formats {
		if f == lower {
			return f, nil
		}
	}
	return "", fmt.Errorf("unsupported output format %q (supported: %s)", name, strings.Join(Formats, ", "))
}

// New returns a writer for the given format, which must be canonical or an
// accepted alias (see Normalize).
func New(format string, w io.Writer) (Writer, error) {
	format, err := Normalize(format)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(w, 64<<10)
	switch format {
	case Table:
		return &tableWriter{w: bw}, nil
	case Vertical:
		return &verticalWriter{w: bw}, nil
	case TSV, TSVWithNames:
		return &tsvWriter{w: bw, header: format == TSVWithNames}, nil
	case CSV, CSVWithNames:
		return newCSVWriter(bw, format == CSVWithNames), nil
	case JSON:
		return &jsonWriter{w: bw, document: true}, nil
	case JSONLines:
		return &jsonWriter{w: bw}, nil
	default:
		return nullWriter{}, nil
	}
}

type nullWriter struct{}

func (nullWriter) Begin([]Column) error { return nil }
func (nullWriter) Row([]any) error      { return nil }
func (nullWriter) End() error           { return nil }
