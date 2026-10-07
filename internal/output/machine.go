package output

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
)

// machineNull is how NULL is written in TSV and CSV, as ClickHouse does.
const machineNull = `\N`

var tsvEscaper = strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "\\r")

// tsvWriter writes ClickHouse-compatible TabSeparated output.
type tsvWriter struct {
	w      *bufio.Writer
	header bool
	fmts   []colFormat
}

func (t *tsvWriter) Begin(cols []Column) error {
	t.fmts = colFormats(cols)
	if !t.header {
		return nil
	}
	for i, c := range cols {
		if i > 0 {
			t.w.WriteByte('\t')
		}
		t.w.WriteString(tsvEscaper.Replace(c.Name))
	}
	return t.w.WriteByte('\n')
}

func (t *tsvWriter) Row(values []any) error {
	for i, v := range values {
		if i > 0 {
			t.w.WriteByte('\t')
		}
		if s, ok := t.fmts[i].text(v); ok {
			t.w.WriteString(tsvEscaper.Replace(s))
		} else {
			t.w.WriteString(machineNull)
		}
	}
	return t.w.WriteByte('\n')
}

// Totals writes the totals row after an empty line, as ClickHouse does.
func (t *tsvWriter) Totals(values []any) error {
	t.w.WriteByte('\n')
	return t.Row(values)
}

func (t *tsvWriter) End() error { return t.w.Flush() }

// csvWriter writes RFC 4180 CSV.
type csvWriter struct {
	out    *bufio.Writer
	w      *csv.Writer
	header bool
	fmts   []colFormat
	record []string
}

func newCSVWriter(w *bufio.Writer, header bool) *csvWriter {
	return &csvWriter{out: w, w: csv.NewWriter(w), header: header}
}

func (c *csvWriter) Begin(cols []Column) error {
	c.fmts = colFormats(cols)
	c.record = make([]string, len(cols))
	if !c.header {
		return nil
	}
	for i, col := range cols {
		c.record[i] = col.Name
	}
	return c.w.Write(c.record)
}

func (c *csvWriter) Row(values []any) error {
	for i, v := range values {
		s, ok := c.fmts[i].text(v)
		if !ok {
			s = machineNull
		}
		c.record[i] = s
	}
	return c.w.Write(c.record)
}

// Totals writes the totals row after an empty line, as ClickHouse does.
func (c *csvWriter) Totals(values []any) error {
	c.w.Flush()
	if _, err := c.out.WriteString("\n"); err != nil {
		return err
	}
	return c.Row(values)
}

func (c *csvWriter) End() error {
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		return err
	}
	return c.out.Flush()
}

// jsonWriter writes either one JSON object per line (JSONEachRow) or, with
// document set, a single document shaped like ClickHouse's JSON format:
//
//	{"meta": [{"name": ..., "type": ...}], "data": [...], "rows": N}
//
// Both variants are streamed; object keys keep the column order.
type jsonWriter struct {
	w        *bufio.Writer
	document bool
	fmts     []colFormat
	keys     [][]byte // pre-encoded `"name":`
	buf      bytes.Buffer
	totals   []byte
	n        int
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func (j *jsonWriter) Begin(cols []Column) error {
	j.fmts = colFormats(cols)
	j.keys = make([][]byte, len(cols))
	for i, c := range cols {
		key, err := encodeJSON(c.Name)
		if err != nil {
			return err
		}
		j.keys[i] = append(key, ':')
	}
	if !j.document {
		return nil
	}
	type meta struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	metas := make([]meta, len(cols))
	for i, c := range cols {
		metas[i] = meta(c)
	}
	encoded, err := encodeJSON(metas)
	if err != nil {
		return err
	}
	j.w.WriteString(`{"meta":`)
	j.w.Write(encoded)
	_, err = j.w.WriteString(`,"data":[`)
	return err
}

// object encodes one row as a JSON object into j.buf.
func (j *jsonWriter) object(values []any) error {
	j.buf.Reset()
	j.buf.WriteByte('{')
	for i, v := range values {
		if i > 0 {
			j.buf.WriteByte(',')
		}
		j.buf.Write(j.keys[i])
		encoded, err := encodeJSON(j.fmts[i].json(v))
		if err != nil {
			return err
		}
		j.buf.Write(encoded)
	}
	j.buf.WriteByte('}')
	return nil
}

// Totals records the totals row; the document carries it under "totals".
// JSON Lines has no place for it (as in ClickHouse's JSONEachRow).
func (j *jsonWriter) Totals(values []any) error {
	if !j.document {
		return nil
	}
	if err := j.object(values); err != nil {
		return err
	}
	j.totals = append([]byte(nil), j.buf.Bytes()...)
	return nil
}

func (j *jsonWriter) Row(values []any) error {
	if err := j.object(values); err != nil {
		return err
	}

	if j.document {
		if j.n > 0 {
			j.w.WriteByte(',')
		}
		j.w.WriteByte('\n')
	}
	j.n++
	j.w.Write(j.buf.Bytes())
	if !j.document {
		return j.w.WriteByte('\n')
	}
	return nil
}

func (j *jsonWriter) End() error {
	if j.document {
		if j.n > 0 {
			j.w.WriteByte('\n')
		}
		j.w.WriteString(`]`)
		if j.totals != nil {
			j.w.WriteString(`,"totals":`)
			j.w.Write(j.totals)
		}
		j.w.WriteString(`,"rows":`)
		encoded, _ := encodeJSON(j.n)
		j.w.Write(encoded)
		j.w.WriteString("}\n")
	}
	return j.w.Flush()
}
