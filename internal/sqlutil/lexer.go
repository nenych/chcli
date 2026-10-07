// Package sqlutil contains a small, forgiving tokenizer for ClickHouse SQL and
// helpers built on top of it. It is deliberately not a parser: everything here
// has to work on incomplete input, because it is used while the user is typing
// (syntax highlighting, completion) as well as for splitting scripts.
package sqlutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenKind classifies a token.
type TokenKind int

const (
	Whitespace TokenKind = iota
	Comment
	Keyword
	Ident
	QuotedIdent
	String
	Number
	Operator
)

// Token is a lexical unit of SQL text. Start and End are byte offsets into the
// original input, with End exclusive.
type Token struct {
	Kind  TokenKind
	Text  string
	Start int
	End   int
	// Unterminated is set for strings, quoted identifiers and block comments
	// that reach the end of input without being closed.
	Unterminated bool
}

// Significant reports whether the token carries meaning (not whitespace or a comment).
func (t Token) Significant() bool {
	return t.Kind != Whitespace && t.Kind != Comment
}

// Upper returns the upper-cased token text.
func (t Token) Upper() string {
	return strings.ToUpper(t.Text)
}

// Is reports whether the token is the given keyword (case-insensitive).
func (t Token) Is(keyword string) bool {
	return t.Kind == Keyword && strings.EqualFold(t.Text, keyword)
}

// IsOp reports whether the token is the given operator.
func (t Token) IsOp(op string) bool {
	return t.Kind == Operator && t.Text == op
}

// IsIdentStart reports whether r can start a bare identifier.
func IsIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

// IsIdentPart reports whether r can appear inside a bare identifier.
func IsIdentPart(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

var multiCharOps = []string{"<=>", "::", "->", "<=", ">=", "<>", "!=", "==", "||"}

// Tokenize splits s into tokens. It never fails: malformed input produces
// tokens flagged as Unterminated, and unknown bytes become single-character
// operators. Concatenating the Text of all tokens yields s.
func Tokenize(s string) []Token {
	var tokens []Token
	i := 0
	emit := func(kind TokenKind, end int, unterminated bool) {
		tokens = append(tokens, Token{Kind: kind, Text: s[i:end], Start: i, End: end, Unterminated: unterminated})
		i = end
	}

	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case unicode.IsSpace(r):
			end := i + size
			for end < len(s) {
				r2, size2 := utf8.DecodeRuneInString(s[end:])
				if !unicode.IsSpace(r2) {
					break
				}
				end += size2
			}
			emit(Whitespace, end, false)

		case strings.HasPrefix(s[i:], "--"), r == '#' && isHashComment(s[i:]):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = len(s)
			} else {
				end += i
			}
			emit(Comment, end, false)

		case strings.HasPrefix(s[i:], "/*"):
			end, closed := scanBlockComment(s, i)
			emit(Comment, end, !closed)

		case r == '\'':
			end, closed := scanQuoted(s, i, '\'')
			emit(String, end, !closed)

		case r == '`' || r == '"':
			end, closed := scanQuoted(s, i, byte(r))
			emit(QuotedIdent, end, !closed)

		case unicode.IsDigit(r):
			end := i + size
			for end < len(s) {
				c := s[end]
				if c >= utf8.RuneSelf || !(c == '.' || c == '_' || isASCIIAlnum(c)) {
					// Allow a sign directly after an exponent marker: 1e-3.
					if (c == '+' || c == '-') && (s[end-1] == 'e' || s[end-1] == 'E') && !strings.HasPrefix(s[i:], "0x") {
						end++
						continue
					}
					break
				}
				end++
			}
			emit(Number, end, false)

		case IsIdentStart(r):
			end := i + size
			for end < len(s) {
				r2, size2 := utf8.DecodeRuneInString(s[end:])
				if !IsIdentPart(r2) {
					break
				}
				end += size2
			}
			kind := Ident
			if IsKeyword(s[i:end]) {
				kind = Keyword
			}
			emit(kind, end, false)

		default:
			end := i + size
			for _, op := range multiCharOps {
				if strings.HasPrefix(s[i:], op) {
					end = i + len(op)
					break
				}
			}
			emit(Operator, end, false)
		}
	}
	return tokens
}

func isASCIIAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isHashComment reports whether s (starting with '#') begins a comment.
// ClickHouse only treats "# " and "#!" as comment openers.
func isHashComment(s string) bool {
	return len(s) > 1 && (s[1] == ' ' || s[1] == '!')
}

// scanBlockComment scans a /* ... */ comment starting at s[start] and returns
// the offset just past it. ClickHouse nests block comments, so this must too:
// otherwise text the server treats as commented out would look like SQL to
// the statement splitter.
func scanBlockComment(s string, start int) (end int, closed bool) {
	depth := 1
	for i := start + 2; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(s[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i, true
			}
		default:
			i++
		}
	}
	return len(s), false
}

// scanQuoted scans a quoted run starting at s[start] (which is the quote
// character) and returns the offset just past it. Both backslash escapes and
// doubled quotes are recognised.
func scanQuoted(s string, start int, quote byte) (end int, closed bool) {
	i := start + 1
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case quote:
			if i+1 < len(s) && s[i+1] == quote {
				i += 2
				continue
			}
			return i + 1, true
		}
		i++
	}
	return len(s), false
}

// SignificantTokens returns the tokens of s without whitespace and comments.
func SignificantTokens(s string) []Token {
	all := Tokenize(s)
	out := all[:0:0]
	for _, t := range all {
		if t.Significant() {
			out = append(out, t)
		}
	}
	return out
}

// Unquote removes identifier quoting (backticks or double quotes) and
// resolves escapes. Bare identifiers are returned unchanged.
func Unquote(ident string) string {
	if len(ident) < 2 {
		return ident
	}
	q := ident[0]
	if q != '`' && q != '"' && q != '\'' {
		return ident
	}
	body := ident[1:]
	if body[len(body)-1] == q {
		body = body[:len(body)-1]
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\\' && i+1 < len(body) {
			i++
			switch body[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '0':
				b.WriteByte(0)
			default:
				b.WriteByte(body[i])
			}
			continue
		}
		if c == q && i+1 < len(body) && body[i+1] == q {
			i++
		}
		b.WriteByte(c)
	}
	return b.String()
}

// NeedsQuoting reports whether name must be quoted to be used as an identifier.
func NeedsQuoting(name string) bool {
	if name == "" {
		return true
	}
	for i, r := range name {
		if i == 0 && !IsIdentStart(r) {
			return true
		}
		if !IsIdentPart(r) || r == '$' {
			return true
		}
	}
	return false
}

// QuoteIdent quotes name with backticks when it cannot be written bare.
func QuoteIdent(name string) string {
	if !NeedsQuoting(name) {
		return name
	}
	r := strings.NewReplacer("\\", "\\\\", "`", "\\`")
	return "`" + r.Replace(name) + "`"
}
