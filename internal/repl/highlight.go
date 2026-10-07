package repl

import (
	"strings"

	prompt "github.com/elk-language/go-prompt"
	istrings "github.com/elk-language/go-prompt/strings"

	"github.com/nenych/chcli/internal/sqlutil"
)

// highlight colours SQL as it is typed. Only tokens that get a colour are
// returned; the editor renders everything in between in the default colour.
func highlight(input string) []prompt.Token {
	if strings.HasPrefix(strings.TrimSpace(input), `\`) {
		return nil // meta command
	}
	tokens := sqlutil.Tokenize(input)
	var out []prompt.Token
	for i, t := range tokens {
		var color prompt.Color
		switch t.Kind {
		case sqlutil.Keyword:
			color = prompt.Blue
		case sqlutil.String:
			color = prompt.DarkGreen
		case sqlutil.Number:
			color = prompt.Purple
		case sqlutil.Comment:
			color = prompt.DarkGray
		case sqlutil.QuotedIdent:
			color = prompt.Brown
		case sqlutil.Ident:
			if i+1 < len(tokens) && tokens[i+1].IsOp("(") {
				color = prompt.Cyan // function call
			}
		}
		if color == prompt.DefaultColor {
			continue
		}
		out = append(out, prompt.NewSimpleToken(
			istrings.ByteNumber(t.Start), istrings.ByteNumber(t.End-1), prompt.SimpleTokenWithColor(color)))
	}
	return out
}
