package history

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestPersistsBetweenSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "history")
	h, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	multiline := "SELECT count(*)\nFROM chronicle.events\nWHERE s = 'a\\nb';"
	for _, entry := range []string{"SELECT 1;", multiline, `\status`} {
		if recorded, err := h.Add(entry); err != nil || !recorded {
			t.Fatalf("Add(%q) = %v, %v", entry, recorded, err)
		}
	}

	reopened, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"SELECT 1;", multiline, `\status`}; !reflect.DeepEqual(reopened.Entries(), want) {
		t.Errorf("entries = %q, want %q", reopened.Entries(), want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("history file mode = %o, want 600", perm)
		}
	}
}

func TestSkipsSensitiveAndNoise(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	h, _ := Open(path, 100)
	skipped := []string{
		"CREATE USER bob IDENTIFIED BY 'hunter2';",
		"ALTER USER bob IDENTIFIED WITH sha256_password BY 'x'",
		// Credentials passed positionally, with nothing in the text naming them.
		"SELECT * FROM s3('https://b/k', 'AKIAIOSFODNN7EXAMPLE', 'wJalrXUtnFEMI/K7MDENG', 'CSV')",
		"INSERT INTO FUNCTION s3('https://b/k', 'AKIAIOSFODNN7EXAMPLE', 'wJalrXUtnFEMI/K7MDENG') SELECT 1",
		"SELECT * FROM mysql('h:3306', 'db', 't', 'reader', 'hunter2-mysql')",
		"SELECT * FROM remote('h:9000', db, t, 'reader', 'hunter2-remote')",
		"CREATE TABLE p (a Int) ENGINE = PostgreSQL('h:5432', 'db', 't', 'reader', 'hunter2-pg')",
		"SET jwt_token = 'abc'  -- token",
		" SELECT 'leading space means do not record';",
		"",
		"   ",
	}
	for _, entry := range skipped {
		if recorded, err := h.Add(entry); err != nil || recorded {
			t.Errorf("Add(%q) = %v, %v; want it skipped", entry, recorded, err)
		}
	}
	h.Add("SELECT 1;")
	if recorded, _ := h.Add("SELECT 1;"); recorded {
		t.Error("an immediate repeat must be skipped")
	}
	// Statements that merely resemble the above are kept.
	kept := []string{
		"SELECT tokens FROM t;", // "tokens" is not the word "token"
		"SELECT * FROM s3('https://bucket/public.parquet');",
		"SELECT * FROM s3('https://bucket/public.csv', 'CSV');",
		"SELECT * FROM remote('h:9000', db.t);",
		"SELECT mysql FROM engines;",
	}
	for _, entry := range kept {
		if recorded, err := h.Add(entry); err != nil || !recorded {
			t.Errorf("Add(%q) = %v, %v; want it recorded", entry, recorded, err)
		}
	}

	data, _ := os.ReadFile(path)
	if got, want := string(data), "SELECT 1;\n"+strings.Join(kept, "\n")+"\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	for _, s := range []string{"hunter2", "AKIA", "wJalr", "abc"} {
		if strings.Contains(string(data), s) {
			t.Errorf("history file contains %q", s)
		}
	}
}

func TestMaxEntriesAndCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	h, _ := Open(path, 5)
	for i := range 20 {
		h.Add("SELECT " + string(rune('a'+i)) + ";")
	}
	if got := h.Entries(); len(got) != 5 || got[4] != "SELECT t;" || got[0] != "SELECT p;" {
		t.Errorf("in-memory entries = %q", got)
	}

	// The append-only file is compacted on the next start.
	reopened, err := Open(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Entries(); len(got) != 5 || got[0] != "SELECT p;" {
		t.Errorf("reloaded entries = %q", got)
	}
	data, _ := os.ReadFile(path)
	if lines := strings.Count(string(data), "\n"); lines != 5 {
		t.Errorf("file has %d lines after compaction, want 5", lines)
	}
}

func TestOpenMissingFile(t *testing.T) {
	h, err := Open(filepath.Join(t.TempDir(), "none"), 10)
	if err != nil || len(h.Entries()) != 0 {
		t.Errorf("Open(missing) = %v, %v", h.Entries(), err)
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"production": "production", "clickhouse.example.com": "clickhouse.example.com",
		"../../etc/passwd": ".._.._etc_passwd", "a b:c": "a_b_c", "": "default",
	} {
		if got := FileName(in); got != want {
			t.Errorf("FileName(%q) = %q, want %q", in, got, want)
		}
	}
}
