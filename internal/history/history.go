// Package history persists REPL input between sessions.
package history

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nenych/chcli/internal/sqlutil"
)

// History is an append-only log of executed inputs, one entry per line with
// newlines escaped, stored with owner-only permissions.
type History struct {
	path    string
	max     int
	entries []string
}

// Open loads the history file at path, keeping at most max entries. A
// missing file is an empty history. When the file has grown well past max it
// is compacted.
func Open(path string, max int) (*History, error) {
	h := &History{path: path, max: max}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	total := 0
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			h.entries = append(h.entries, unescape(line))
			total++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(h.entries) > max {
		h.entries = h.entries[len(h.entries)-max:]
	}
	if total > max+max/5 {
		if err := h.rewrite(); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// Entries returns the entries, oldest first.
func (h *History) Entries() []string { return h.entries }

// Add appends an entry and persists it. Entries that look like they contain
// credentials, entries starting with a space (the shell convention for "do
// not record this") and immediate repeats are skipped; it reports whether
// the entry was recorded.
func (h *History) Add(entry string) (bool, error) {
	if strings.HasPrefix(entry, " ") {
		return false, nil
	}
	entry = strings.TrimSpace(entry)
	if entry == "" || IsSensitive(entry) {
		return false, nil
	}
	if n := len(h.entries); n > 0 && h.entries[n-1] == entry {
		return false, nil
	}
	h.entries = append(h.entries, entry)
	if len(h.entries) > h.max {
		h.entries = h.entries[len(h.entries)-h.max:]
	}

	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return true, err
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return true, err
	}
	_, err = f.WriteString(escape(entry) + "\n")
	return true, errors.Join(err, f.Close())
}

func (h *History) rewrite() error {
	var b strings.Builder
	for _, e := range h.entries {
		b.WriteString(escape(e))
		b.WriteByte('\n')
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

var (
	escaper   = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	unescaper = strings.NewReplacer(`\\`, `\`, `\n`, "\n")
)

func escape(s string) string   { return escaper.Replace(s) }
func unescape(s string) string { return unescaper.Replace(s) }

// sensitive matches statements that name what they carry: user management
// (IDENTIFIED BY / WITH) and anything mentioning a password, secret, access
// key or token.
var sensitive = regexp.MustCompile(`(?i)\bidentified\b|password|secret|access_key|\btoken\b`)

// credentialFunctions are table functions and engines that take credentials
// as plain positional arguments, with the number of arguments from which a
// call is assumed to include them: s3(url, key, secret, ...),
// mysql(host, db, table, user, password), remote(addr, db, table, user, password).
var credentialFunctions = map[string]int{
	"s3": 3, "s3cluster": 4, "gcs": 3, "oss": 3, "cosn": 3, "azureblobstorage": 3,
	"iceberg": 3, "icebergs3": 3, "deltalake": 3, "hudi": 3,
	"mysql": 4, "postgresql": 4, "mongodb": 4, "redis": 4,
	"remote": 4, "remotesecure": 4,
}

// IsSensitive reports whether a statement should be kept out of the history
// because it probably contains credentials. This is a heuristic; starting a
// line with a space keeps anything out for certain.
func IsSensitive(statement string) bool {
	if sensitive.MatchString(statement) {
		return true
	}
	tokens := sqlutil.SignificantTokens(statement)
	for i := 0; i+1 < len(tokens); i++ {
		minArgs, ok := credentialFunctions[strings.ToLower(tokens[i].Text)]
		if !ok || !tokens[i+1].IsOp("(") {
			continue
		}
		args, depth := 1, 0
		for _, t := range tokens[i+1:] {
			switch {
			case t.IsOp("("):
				depth++
			case t.IsOp(")"):
				depth--
			case t.IsOp(",") && depth == 1:
				args++
			}
			if depth == 0 {
				break
			}
		}
		if args >= minArgs {
			return true
		}
	}
	return false
}

// FileName returns a safe file name for the history of a connection label.
func FileName(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}
