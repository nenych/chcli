package output

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// nullText is how NULL is shown in human-oriented formats, as in ClickHouse.
const nullText = "ᴺᵁᴸᴸ"

// colFormat holds what is needed to render the values of one column.
type colFormat struct {
	timeLayout string
	tuple      bool
	numeric    bool
}

func newColFormat(c Column) colFormat {
	base := baseType(c.Type)
	return colFormat{
		timeLayout: timeLayout(c.Type),
		tuple:      strings.HasPrefix(base, "Tuple("),
		numeric: strings.HasPrefix(base, "Int") || strings.HasPrefix(base, "UInt") ||
			strings.HasPrefix(base, "Float") || strings.HasPrefix(base, "Decimal"),
	}
}

func colFormats(cols []Column) []colFormat {
	out := make([]colFormat, len(cols))
	for i, c := range cols {
		out[i] = newColFormat(c)
	}
	return out
}

// baseType strips the Nullable and LowCardinality wrappers.
func baseType(t string) string {
	for {
		switch {
		case strings.HasPrefix(t, "Nullable(") && strings.HasSuffix(t, ")"):
			t = t[len("Nullable(") : len(t)-1]
		case strings.HasPrefix(t, "LowCardinality(") && strings.HasSuffix(t, ")"):
			t = t[len("LowCardinality(") : len(t)-1]
		default:
			return t
		}
	}
}

// timeLayout picks the layout for time values of a column from its type.
// The driver returns Date, DateTime and DateTime64 all as time.Time; for
// composite types the most precise temporal type they mention is used.
func timeLayout(typ string) string {
	if i := strings.Index(typ, "DateTime64("); i >= 0 {
		rest := typ[i+len("DateTime64("):]
		end := strings.IndexAny(rest, ",)")
		if end > 0 {
			if precision, err := strconv.Atoi(rest[:end]); err == nil && precision > 0 && precision <= 9 {
				return "2006-01-02 15:04:05." + strings.Repeat("0", precision)
			}
		}
		return "2006-01-02 15:04:05"
	}
	if strings.Contains(typ, "DateTime") || !strings.Contains(typ, "Date") {
		return "2006-01-02 15:04:05"
	}
	return "2006-01-02"
}

// text renders a value the way ClickHouse's text formats do. It returns
// ok=false for NULL so that each format can substitute its own marker.
func (f colFormat) text(v any) (s string, ok bool) {
	if v == nil {
		return "", false
	}
	if f.tuple {
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice {
			return f.sequence(rv, "(", ")"), true
		}
	}
	return f.render(v, false), true
}

// render formats v; quote says whether strings must be quoted, which is the
// case inside arrays, tuples and maps. Note that []byte is deliberately not
// treated as text: the driver returns strings as string, and a []uint8 is an
// Array(UInt8).
func (f colFormat) render(v any, quote bool) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		if quote {
			return quoteString(x)
		}
		return x
	case time.Time:
		s := x.Format(f.timeLayout)
		if quote {
			return "'" + s + "'"
		}
		return s
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return formatFloat(x, 64)
	case float32:
		return formatFloat(float64(x), 32)
	case fmt.Stringer: // UUID, Decimal, IP addresses, big integers
		return x.String()
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return "NULL"
		}
		return f.render(rv.Elem().Interface(), quote)
	case reflect.Slice, reflect.Array:
		return f.sequence(rv, "[", "]")
	case reflect.Map:
		keys := rv.MapKeys()
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = f.render(k.Interface(), true) + ":" + f.render(rv.MapIndex(k).Interface(), true)
		}
		sort.Strings(parts) // Go maps are unordered; keep the output stable
		return "{" + strings.Join(parts, ",") + "}"
	}
	return fmt.Sprint(v)
}

func (f colFormat) sequence(rv reflect.Value, open, closing string) string {
	var b strings.Builder
	b.WriteString(open)
	for i := 0; i < rv.Len(); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(f.render(rv.Index(i).Interface(), true))
	}
	b.WriteString(closing)
	return b.String()
}

func formatFloat(v float64, bits int) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	return strconv.FormatFloat(v, 'g', -1, bits)
}

func quoteString(s string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'", "\n", "\\n", "\t", "\\t").Replace(s) + "'"
}

// json converts a value into something encoding/json renders the way
// ClickHouse's JSON formats do: times as strings, non-finite floats as null,
// maps with string keys.
func (f colFormat) json(v any) any {
	switch x := v.(type) {
	case nil, string, bool:
		return x
	case time.Time:
		return x.Format(f.timeLayout)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return x
	case float32:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil
		}
		return x
	case fmt.Stringer:
		return x.String()
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return f.json(rv.Elem().Interface())
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = f.json(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		for _, k := range rv.MapKeys() {
			out[f.render(k.Interface(), false)] = f.json(rv.MapIndex(k).Interface())
		}
		return out
	}
	return v
}
