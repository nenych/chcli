package repl

import (
	"strings"
	"testing"
	"time"

	"github.com/nenych/chcli/internal/completion"
)

func TestParseMeta(t *testing.T) {
	tests := []struct {
		in        string
		name, arg string
		ok        bool
	}{
		{`\q`, `\q`, "", true},
		{`  \quit  `, `\quit`, "", true},
		{`\d events`, `\d`, "events", true},
		{`\describe   chronicle.events ;`, `\describe`, "chronicle.events", true},
		{`\use chronicle;`, `\use`, "chronicle", true},
		{"\\d\tevents", `\d`, "events", true},
		{`\history 50`, `\history`, "50", true},
		{`\?`, `\?`, "", true},
		{`\nosuch thing`, `\nosuch`, "thing", true},
		{"SELECT 1", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		name, arg, ok := ParseMeta(tt.in)
		if name != tt.name || arg != tt.arg || ok != tt.ok {
			t.Errorf("ParseMeta(%q) = %q, %q, %v; want %q, %q, %v", tt.in, name, arg, ok, tt.name, tt.arg, tt.ok)
		}
	}
}

func TestCommandTable(t *testing.T) {
	required := []string{`\q`, `\quit`, `\l`, `\databases`, `\dt`, `\tables`, `\d`, `\describe`, `\use`,
		`\status`, `\refresh`, `\history`, `\help`, `\format`}
	for _, name := range required {
		if findCommand(name) == nil {
			t.Errorf("meta command %s is missing", name)
		}
	}
	if findCommand(`\nosuch`) != nil {
		t.Error("unknown command found")
	}
	seen := map[string]bool{}
	for _, cmd := range commands {
		if cmd.run == nil || cmd.help == "" {
			t.Errorf("command %v is incomplete", cmd.names)
		}
		for _, name := range cmd.names {
			if !strings.HasPrefix(name, `\`) || seen[name] {
				t.Errorf("bad or duplicate command name %q", name)
			}
			seen[name] = true
		}
	}
	if cmd := findCommand(`\d`); cmd.arg != completion.ArgTable {
		t.Error(`\d must complete table names`)
	}
	if cmd := findCommand(`\use`); cmd.arg != completion.ArgDatabase {
		t.Error(`\use must complete database names`)
	}
}

func TestShouldExecute(t *testing.T) {
	run := []string{"", "  ", "SELECT 1;", "SELECT 1; ", "SELECT 1\n;", "SELECT 1\\G", `\status`, `  \d events`, "exit", "QUIT", "help",
		"SELECT 1; SELECT 2;"}
	wait := []string{"SELECT 1", "SELECT count(*)\nFROM t", "SELECT ';'", "SELECT 1; -- note\nSELECT 2", "SELECT 1 /* ; */", "exit now"}
	for _, in := range run {
		if !shouldExecute(in) {
			t.Errorf("shouldExecute(%q) = false: Enter should run it", in)
		}
	}
	for _, in := range wait {
		if shouldExecute(in) {
			t.Errorf("shouldExecute(%q) = true: Enter should continue the input", in)
		}
	}
}

func TestHighlight(t *testing.T) {
	input := "SELECT count(x), 'str', 42 FROM `t` -- c"
	covered := map[string]bool{}
	for _, tok := range highlight(input) {
		covered[input[tok.FirstByteIndex():tok.LastByteIndex()+1]] = true
	}
	for _, want := range []string{"SELECT", "count", "'str'", "42", "FROM", "`t`", "-- c"} {
		if !covered[want] {
			t.Errorf("%q is not highlighted (highlighted: %v)", want, covered)
		}
	}
	if covered["x"] || covered[","] {
		t.Errorf("plain identifiers and punctuation must keep the default colour: %v", covered)
	}
	if got := highlight(`\d events`); got != nil {
		t.Errorf("meta commands must not be highlighted as SQL: %v", got)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 42 * time.Second: "42s", 43*time.Minute + 10*time.Second: "43m",
		2*time.Hour + 5*time.Minute: "2h 5m", 49 * time.Hour: "2d 1h",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
