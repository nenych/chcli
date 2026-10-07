package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/nenych/chcli/internal/chclient"
	"github.com/nenych/chcli/internal/sqlutil"
)

func TestFormatFooter(t *testing.T) {
	tests := []struct {
		res  Result
		want string
	}{
		{Result{HasResultSet: true, Rows: 1, Elapsed: 87 * time.Millisecond}, "1 row in set. 0.087 sec."},
		{Result{HasResultSet: true, Rows: 0, Elapsed: time.Millisecond}, "0 rows in set. 0.001 sec."},
		{Result{HasResultSet: true, Rows: 3, Elapsed: 1500 * time.Millisecond, Stats: chclient.Stats{Rows: 1382991, Bytes: 11063928}},
			"3 rows in set. 1.500 sec. Processed 1.38 million rows, 11.06 MB."},
		{Result{HasResultSet: true, Rows: 1, Elapsed: time.Millisecond, Stats: chclient.Stats{Rows: 1, Bytes: 16}},
			"1 row in set. 0.001 sec. Processed 1 row, 16 B."},
		{Result{Elapsed: 12 * time.Millisecond}, "Ok. 0.012 sec."},
	}
	for _, tt := range tests {
		if got := FormatFooter(tt.res); got != tt.want {
			t.Errorf("FormatFooter(%+v) = %q, want %q", tt.res, got, tt.want)
		}
	}
}

func TestFormatError(t *testing.T) {
	native := &clickhouse.Exception{Code: 60, Message: "Unknown table expression identifier 'x'"}
	if got, want := FormatError(native), "Code: 60. Unknown table expression identifier 'x'"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Over HTTP the message already carries the code name; do not repeat it.
	http := &clickhouse.Exception{Code: 60, CodeName: "UNKNOWN_TABLE", Message: "Unknown table x. (UNKNOWN_TABLE)"}
	if got, want := FormatError(http), "Code: 60. Unknown table x. (UNKNOWN_TABLE)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	named := &clickhouse.Exception{Code: 62, CodeName: "SYNTAX_ERROR", Message: "Syntax error"}
	if got, want := FormatError(named), "Code: 62. Syntax error (SYNTAX_ERROR)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// The server's hints about resetting a local password are dropped.
	auth := &chclient.AuthError{Err: &clickhouse.Exception{Code: 516, Message: "default: Authentication failed.\n\nIf you have installed ClickHouse and forgot password..."}}
	if got, want := FormatError(auth), "Code: 516. default: Authentication failed."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := FormatError(fmt.Errorf("query: %w", context.Canceled)); got != "Query cancelled." {
		t.Errorf("cancellation = %q", got)
	}
	if got := FormatError(errors.New("plain")); got != "plain" {
		t.Errorf("plain error = %q", got)
	}
}

func TestNeedsServerSession(t *testing.T) {
	for sql, want := range map[string]bool{
		"SET ROLE admin":                           true,
		"CREATE TEMPORARY TABLE t (a UInt8)":       true,
		"CREATE OR REPLACE TEMPORARY TABLE t AS x": true,
		"CREATE TABLE temporary_things (a UInt8)":  false,
		"SELECT 1": false,
		"CREATE TABLE t (temporary UInt8) ENGINE = Memory": false,
	} {
		if got := needsServerSession(sqlutil.ParseStatement(sql)); got != want {
			t.Errorf("needsServerSession(%q) = %v, want %v", sql, got, want)
		}
	}
}
