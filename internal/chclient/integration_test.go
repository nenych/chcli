//go:build integration

// Integration tests against a real ClickHouse server. Run with:
//
//	CHCLI_TEST_ADDR=127.0.0.1:9000 CHCLI_TEST_HTTP_ADDR=127.0.0.1:8123 \
//	CHCLI_TEST_PASSWORD=secret go test -tags integration ./...
//
// The token authentication test additionally needs an Altinity Antalya
// server configured with testdata/antalya (CHCLI_TEST_TOKEN_ADDR and
// CHCLI_TEST_TOKEN_HTTP_ADDR). Tests whose server is not configured are
// skipped. "make clickhouse-up antalya-up test-integration" runs everything.
package chclient_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/chclient"
	"github.com/nenych/chcli/internal/config"
	"github.com/nenych/chcli/internal/metadata"
	"github.com/nenych/chcli/internal/session"
)

func options(t *testing.T, envVar, protocol string) chclient.Options {
	t.Helper()
	addr := os.Getenv(envVar)
	if addr == "" {
		t.Skipf("%s is not set", envVar)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	return chclient.Options{Host: host, Port: port, Database: "default", Protocol: protocol, ClientVersion: "test"}
}

func provider(password string) auth.Provider {
	user := os.Getenv("CHCLI_TEST_USER")
	if user == "" {
		user = "default"
	}
	return &auth.PasswordProvider{Username: user, Password: password}
}

func connect(t *testing.T, opts chclient.Options) *chclient.Client {
	t.Helper()
	c := chclient.New(opts, provider(os.Getenv("CHCLI_TEST_PASSWORD")))
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c
}

// Every test runs over both transports.
func forEachProtocol(t *testing.T, fn func(t *testing.T, c *chclient.Client)) {
	t.Run("native", func(t *testing.T) { fn(t, connect(t, options(t, "CHCLI_TEST_ADDR", config.ProtocolNative))) })
	t.Run("http", func(t *testing.T) { fn(t, connect(t, options(t, "CHCLI_TEST_HTTP_ADDR", config.ProtocolHTTP))) })
}

func run(t *testing.T, c *chclient.Client, format, sql string) string {
	t.Helper()
	var out bytes.Buffer
	s := &session.Session{Client: c, Format: format}
	if _, err := s.Execute(context.Background(), sql, &out); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out.String()
}

func TestConnect(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		info := c.Info()
		if info.Version == "" || info.User == "" {
			t.Errorf("server info = %+v", info)
		}
	})
}

func TestTypesRoundTrip(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		got := run(t, c, "tsv", `SELECT
			toUInt64(18446744073709551615), toInt8(-5), 1.5::Float64, 'text', toFixedString('ab', 2),
			toDate('2026-01-02'), toDateTime('2026-01-02 03:04:05', 'UTC'), toDateTime64('2026-01-02 03:04:05.678', 3, 'UTC'),
			[1, 2]::Array(UInt8), ['a', 'b'], map('k', 1), (1, 'x'), CAST(NULL AS Nullable(String)),
			toDecimal64(12.34, 2), toUUID('00000000-0000-0000-0000-000000000001'), toIPv4('10.0.0.1'),
			true, toLowCardinality('lc'), [toDate('2026-01-02')]`)
		want := strings.Join([]string{
			"18446744073709551615", "-5", "1.5", "text", "ab",
			"2026-01-02", "2026-01-02 03:04:05", "2026-01-02 03:04:05.678",
			"[1,2]", "['a','b']", "{'k':1}", "(1,'x')", `\N`,
			"12.34", "00000000-0000-0000-0000-000000000001", "10.0.0.1",
			"true", "lc", "['2026-01-02']",
		}, "\t") + "\n"
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	})
}

func TestDDLInsertAndSessionState(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		db := "chcli_it_" + strconv.FormatInt(time.Now().UnixNano(), 36)
		run(t, c, "tsv", "CREATE DATABASE "+db)
		t.Cleanup(func() { run(t, c, "tsv", "DROP DATABASE IF EXISTS "+db) })
		run(t, c, "tsv", "CREATE TABLE "+db+".t (id UInt64, s String) ENGINE = MergeTree ORDER BY id")
		run(t, c, "tsv", "INSERT INTO "+db+".t VALUES (1, 'a'), (2, 'b')")
		run(t, c, "tsv", "INSERT INTO "+db+".t SELECT number + 10, 'gen' FROM numbers(3)")

		// USE and SET are tracked by the client and apply to later statements.
		run(t, c, "tsv", "USE "+db)
		if c.Database() != db {
			t.Errorf("Database() = %q after USE", c.Database())
		}
		run(t, c, "tsv", "SET max_threads = 3")
		if got := run(t, c, "tsv", "SELECT count(), currentDatabase(), getSetting('max_threads') FROM t"); got != "5\t"+db+"\t3\n" {
			t.Errorf("got %q", got)
		}

		// A failed USE or SET leaves the session untouched.
		s := &session.Session{Client: c, Format: "tsv"}
		if _, err := s.Execute(context.Background(), "USE no_such_database_xyz", &bytes.Buffer{}); err == nil {
			t.Error("USE of a missing database must fail")
		}
		if _, err := s.Execute(context.Background(), "SET no_such_setting_xyz = 1", &bytes.Buffer{}); err == nil {
			t.Error("SET of an unknown setting must fail")
		}
		if got := run(t, c, "tsv", "SELECT currentDatabase()"); got != db+"\n" {
			t.Errorf("session damaged by a failed USE/SET: %q", got)
		}

		// Formats and the FORMAT clause.
		if got := run(t, c, "tsv", "SELECT id, s FROM t WHERE id <= 2 ORDER BY id FORMAT JSONEachRow"); got != `{"id":1,"s":"a"}`+"\n"+`{"id":2,"s":"b"}`+"\n" {
			t.Errorf("jsonlines = %q", got)
		}
		table := run(t, c, "table", "SELECT id, s FROM t WHERE id = 1")
		if want := "┌─id─┬─s─┐\n│  1 │ a │\n└────┴───┘\n"; table != want {
			t.Errorf("table = %q, want %q", table, want)
		}
		run(t, c, "tsv", "USE default")
	})
}

func TestServerErrors(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		s := &session.Session{Client: c, Format: "tsv"}
		_, err := s.Execute(context.Background(), "SELECT * FROM no_such_table_xyz", &bytes.Buffer{})
		if err == nil || !strings.Contains(session.FormatError(err), "Code: 60") {
			t.Errorf("error = %v", err)
		}
		// The session survives a failed statement.
		if got := run(t, c, "tsv", "SELECT 1"); got != "1\n" {
			t.Errorf("got %q", got)
		}
	})
}

func TestStreamingLargeResult(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		rows, err := c.Query(context.Background(), "SELECT number FROM numbers(2000000)")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var n, sum uint64
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				t.Fatal(err)
			}
			sum += vals[0].(uint64)
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if n != 2000000 || sum != 1999999000000 {
			t.Errorf("n = %d, sum = %d", n, sum)
		}
	})
}

func TestCancellation(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(300*time.Millisecond, cancel)
		started := time.Now()
		s := &session.Session{Client: c, Format: "tsv"}
		_, err := s.Execute(ctx, "SELECT sleepEachRow(1) FROM numbers(30) SETTINGS max_block_size = 1", &bytes.Buffer{})
		if err == nil {
			t.Fatal("a cancelled query must return an error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("cancellation took %v", elapsed)
		}
		// The query must stop on the server too, not just on our side.
		deadline := time.Now().Add(10 * time.Second)
		for {
			running := run(t, c, "tsv", "SELECT count() FROM system.processes WHERE query LIKE '%sleepEachRow(1) FROM numbers(30)%' AND query NOT LIKE '%system.processes%'")
			if running == "0\n" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the cancelled query is still running on the server (%s)", strings.TrimSpace(running))
			}
			time.Sleep(200 * time.Millisecond)
		}
		// The client recovers and the next statement works.
		if got := run(t, c, "tsv", "SELECT 1"); got != "1\n" {
			t.Errorf("after cancellation: %q", got)
		}
	})
}

func TestAuthenticationFailure(t *testing.T) {
	opts := options(t, "CHCLI_TEST_ADDR", config.ProtocolNative)
	c := chclient.New(opts, provider("definitely-the-wrong-password"))
	defer c.Close()
	err := c.Connect(context.Background())
	var authErr *chclient.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %v (%T), want AuthError", err, err)
	}
	if strings.Contains(err.Error(), "definitely-the-wrong-password") {
		t.Error("the error message contains the password")
	}
}

func TestConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // nothing listens here any more

	c := chclient.New(chclient.Options{Host: "127.0.0.1", Port: port, Database: "default", Protocol: config.ProtocolNative}, provider(""))
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var connErr *chclient.ConnError
	if err := c.Connect(ctx); !errors.As(err, &connErr) {
		t.Fatalf("error = %v (%T), want ConnError", err, err)
	}
}

func TestMetadataFromRealServer(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		cache := metadata.NewCache(c)
		if err := cache.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		snap := cache.Snapshot()
		if !snap.HasDatabase("system") || len(snap.Tables["system"]) == 0 {
			t.Error("system database or its tables missing")
		}
		if len(snap.Columns[metadata.TableKey("system", "tables")]) == 0 {
			t.Error("columns of system.tables missing")
		}
		for name, n := range map[string]int{
			"functions": len(snap.Functions), "table functions": len(snap.TableFunctions), "data types": len(snap.DataTypes),
			"engines": len(snap.Engines), "formats": len(snap.Formats), "settings": len(snap.Settings), "keywords": len(snap.Keywords),
		} {
			if n == 0 {
				t.Errorf("no %s loaded", name)
			}
		}
		aggregates := 0
		for _, f := range snap.Functions {
			if f.Aggregate {
				aggregates++
			}
		}
		if aggregates == 0 {
			t.Error("no aggregate functions recognised")
		}
	})
}

// antalyaSigningKey is the HS256 key configured in testdata/antalya/token-auth.xml.
const antalyaSigningKey = "chcli-integration-test-signing-key-0123456789"

func mintToken(t *testing.T, key string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + enc(claims)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Token authentication against Altinity Antalya ("make antalya-up"): the user
// exists only in the token, is named by its e-mail claim and gets its roles
// from the server's token user directory.
func TestTokenAuthentication(t *testing.T) {
	for name, tc := range map[string]struct{ env, protocol string }{
		"native": {"CHCLI_TEST_TOKEN_ADDR", config.ProtocolNative},
		"http":   {"CHCLI_TEST_TOKEN_HTTP_ADDR", config.ProtocolHTTP},
	} {
		t.Run(name, func(t *testing.T) {
			opts := options(t, tc.env, tc.protocol)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			claims := map[string]any{"sub": "user-1", "email": "alice@example.com", "exp": time.Now().Add(time.Hour).Unix()}
			c := chclient.New(opts, &auth.JWTProvider{Token: mintToken(t, antalyaSigningKey, claims)})
			defer c.Close()
			if err := c.Connect(ctx); err != nil {
				t.Fatalf("connect with a valid token: %v", err)
			}
			if got := c.Info().User; got != "alice@example.com" {
				t.Errorf("ClickHouse user = %q, want the token's e-mail claim", got)
			}
			if got := run(t, c, "tsv", "SELECT currentUser(), currentRoles()"); got != "alice@example.com\t['chcli_token_reader']\n" {
				t.Errorf("identity and roles = %q", got)
			}

			// A token signed with another key is rejected as an authentication failure.
			forged := chclient.New(opts, &auth.JWTProvider{Token: mintToken(t, "some-other-key-some-other-key-0123", claims)})
			defer forged.Close()
			var authErr *chclient.AuthError
			if err := forged.Connect(ctx); !errors.As(err, &authErr) {
				t.Errorf("forged token: error = %v (%T), want AuthError", err, err)
			}
		})
	}
}

// slowWriter throttles output the way a terminal or a pipe does.
type slowWriter struct {
	lines atomic.Int64
}

func (w *slowWriter) Write(p []byte) (int, error) {
	w.lines.Add(int64(bytes.Count(p, []byte("\n"))))
	time.Sleep(time.Millisecond)
	return len(p), nil
}

// Cancelling a query that produces rows faster than they can be shown must
// stop the output at once: the driver has megabytes of rows buffered by then.
func TestCancellationStopsOutput(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := &slowWriter{}
		done := make(chan error, 1)
		go func() {
			s := &session.Session{Client: c, Format: "tsv"}
			_, err := s.Execute(ctx, "SELECT number FROM system.numbers", out)
			done <- err
		}()

		time.Sleep(300 * time.Millisecond)
		cancel()
		atCancel := out.lines.Load()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the statement did not return after cancellation")
		}
		// One output buffer may still be flushed; not hundreds of thousands of rows.
		if extra := out.lines.Load() - atCancel; extra > 20000 {
			t.Errorf("%d more lines were written after cancellation", extra)
		}
		if got := run(t, c, "tsv", "SELECT 1"); got != "1\n" {
			t.Errorf("after cancellation: %q", got)
		}
	})
}

func TestWithTotals(t *testing.T) {
	// The HTTP interface returns results in the Native format, which carries
	// no totals block; this is a documented limitation there.
	c := connect(t, options(t, "CHCLI_TEST_ADDR", config.ProtocolNative))
	got := run(t, c, "tsv", "SELECT number % 2 AS k, count() AS n FROM numbers(10) GROUP BY k WITH TOTALS ORDER BY k")
	if want := "0\t5\n1\t5\n\n0\t10\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := run(t, c, "tsv", "SELECT 1"); got != "1\n" {
		t.Errorf("a query without totals: %q", got)
	}
}

// Statements that cannot work must fail loudly rather than hang or pretend.
func TestUnsupportedStatementsAreRejected(t *testing.T) {
	forEachProtocol(t, func(t *testing.T, c *chclient.Client) {
		db := "chcli_it_" + strconv.FormatInt(time.Now().UnixNano(), 36)
		run(t, c, "tsv", "CREATE DATABASE "+db)
		t.Cleanup(func() { run(t, c, "tsv", "DROP DATABASE IF EXISTS "+db) })
		run(t, c, "tsv", "CREATE TABLE "+db+".t (id UInt64, s String) ENGINE = MergeTree ORDER BY id")

		s := &session.Session{Client: c, Format: "tsv"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, stmt := range []string{
			"INSERT INTO " + db + ".t VALUES",     // data would come on stdin: must not hang
			"INSERT INTO " + db + ".t FORMAT CSV", // likewise
			"INSERT INTO " + db + ".t FORMAT CSV\n1,with love",
		} {
			if _, err := s.Execute(ctx, stmt, &bytes.Buffer{}); err == nil || ctx.Err() != nil {
				t.Errorf("%q: error = %v (context: %v), want an immediate rejection", stmt, err, ctx.Err())
			}
		}
		if got := run(t, c, "tsv", "SELECT count() FROM "+db+".t"); got != "0\n" {
			t.Errorf("rejected INSERTs inserted rows: %q", got)
		}

		// Session-only statements have no lasting effect over HTTP, so they
		// are refused there; over the native protocol they work.
		_, err := s.Execute(ctx, "CREATE TEMPORARY TABLE chcli_tmp (a UInt8)", &bytes.Buffer{})
		if c.Options().Protocol == config.ProtocolHTTP {
			if err == nil || !strings.Contains(err.Error(), "server session") {
				t.Errorf("temporary table over HTTP: error = %v", err)
			}
		} else {
			if err != nil {
				t.Fatalf("temporary table over native: %v", err)
			}
			run(t, c, "tsv", "INSERT INTO chcli_tmp VALUES (7)")
			if got := run(t, c, "tsv", "SELECT a FROM chcli_tmp"); got != "7\n" {
				t.Errorf("temporary table did not persist in the session: %q", got)
			}
		}
	})
}
