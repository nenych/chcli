// Package chclient is the ClickHouse transport layer. It wraps the official
// clickhouse-go driver and knows nothing about how credentials were acquired
// or how results are rendered.
package chclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/config"
)

// Options describe the server to connect to.
type Options struct {
	Host               string
	Port               int
	Database           string
	Protocol           string // config.ProtocolNative or config.ProtocolHTTP
	Secure             bool
	InsecureSkipVerify bool
	CACert             string
	// ClientVersion is reported to the server (visible in system.query_log).
	ClientVersion string
}

// OptionsFrom maps a resolved configuration to transport options.
func OptionsFrom(r *config.Resolved, clientVersion string) Options {
	return Options{
		Host:               r.Host,
		Port:               r.Port,
		Database:           r.Database,
		Protocol:           r.Protocol,
		Secure:             r.Secure,
		InsecureSkipVerify: r.InsecureSkipVerify,
		CACert:             r.CACert,
		ClientVersion:      clientVersion,
	}
}

// Addr returns host:port.
func (o Options) Addr() string { return net.JoinHostPort(o.Host, strconv.Itoa(o.Port)) }

// TLSConfig builds the TLS configuration, or nil for plaintext connections.
// Certificates are verified unless InsecureSkipVerify was explicitly set.
func (o Options) TLSConfig() (*tls.Config, error) {
	if !o.Secure {
		return nil, nil
	}
	cfg := &tls.Config{ServerName: o.Host, InsecureSkipVerify: o.InsecureSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // explicit user opt-in
	if o.CACert != "" {
		pem, err := os.ReadFile(o.CACert)
		if err != nil {
			return nil, fmt.Errorf("read CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", o.CACert)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// AuthError reports that credentials could not be obtained or were rejected.
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return e.Err.Error() }
func (e *AuthError) Unwrap() error { return e.Err }

// ConnError reports that the server could not be reached or the connection broke.
type ConnError struct {
	Addr string
	Err  error
}

func (e *ConnError) Error() string { return fmt.Sprintf("connection to %s failed: %v", e.Addr, e.Err) }
func (e *ConnError) Unwrap() error { return e.Err }

// Client is a session with one ClickHouse server. All statements of a session
// run on a single pooled connection, so session state set by statements the
// client does not track itself (SET ROLE, temporary tables) survives until
// that connection is re-established. The current database and plain settings
// are tracked by the client and survive reconnects.
type Client struct {
	opts Options
	auth auth.Provider

	mu       sync.Mutex
	conn     driver.Conn
	token    string // bearer token the current pool was opened with
	database string
	settings clickhouse.Settings
	info     ServerInfo
}

// ServerInfo is what the server reported about itself and the session.
type ServerInfo struct {
	Version string
	User    string // the ClickHouse user the session is authenticated as
}

// New creates a client. No connection is made until Connect or the first query.
func New(opts Options, provider auth.Provider) *Client {
	return &Client{opts: opts, auth: provider, database: opts.Database, settings: clickhouse.Settings{}}
}

// Connect authenticates and verifies that the server accepts the session.
func (c *Client) Connect(ctx context.Context) error {
	conn, err := c.acquire(ctx)
	if err != nil {
		return err
	}
	var info ServerInfo
	if err := conn.QueryRow(ctx, "SELECT version(), currentUser()").Scan(&info.Version, &info.User); err != nil {
		return c.wrap(ctx, err)
	}
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	return nil
}

// Info returns the server information gathered by Connect.
func (c *Client) Info() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Database returns the session's current database.
func (c *Client) Database() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.database
}

// Options returns the options the client was created with.
func (c *Client) Options() Options { return c.opts }

// Close releases the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// acquire returns a connection pool that authenticates with current
// credentials. The provider is consulted every time: when it hands out a new
// token (after a refresh or a new login) the pool is rebuilt, so an expired
// token is never presented to the server.
func (c *Client) acquire(ctx context.Context) (driver.Conn, error) {
	creds, err := c.auth.Authenticate(ctx)
	if err != nil {
		return nil, &AuthError{err}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.token == creds.Token {
		return c.conn, nil
	}
	conn, err := c.open(creds, c.database)
	if err != nil {
		return nil, err
	}
	if c.conn != nil {
		slog.Debug("clickhouse: credentials changed, reconnecting")
		_ = c.conn.Close()
	}
	c.conn, c.token = conn, creds.Token
	return conn, nil
}

func (c *Client) open(creds *auth.Credentials, database string) (driver.Conn, error) {
	tlsConfig, err := c.opts.TLSConfig()
	if err != nil {
		return nil, err
	}
	o := &clickhouse.Options{
		Addr:        []string{c.opts.Addr()},
		Protocol:    clickhouse.Native,
		TLS:         tlsConfig,
		Auth:        clickhouse.Auth{Database: database},
		DialTimeout: 10 * time.Second,
		// One connection, kept for as long as possible: see the Client doc.
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: 24 * time.Hour,
		Compression:     &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		ClientInfo: clickhouse.ClientInfo{Products: []struct {
			Name    string
			Version string
		}{{Name: "chcli", Version: c.opts.ClientVersion}}},
	}
	if c.opts.Protocol == config.ProtocolHTTP {
		o.Protocol = clickhouse.HTTP
	}

	switch {
	case !creds.UsesToken():
		o.Auth.Username, o.Auth.Password = creds.Username, creds.Password
	case o.Protocol == clickhouse.HTTP && tlsConfig == nil:
		// The driver only attaches bearer tokens to HTTPS requests; plain
		// HTTP (local testing, TLS-terminating proxies) needs the header set
		// explicitly.
		token := creds.Token
		o.TransportFunc = func(t *http.Transport) (http.RoundTripper, error) {
			return bearerTransport{token: token, next: t}, nil
		}
	default:
		// Native: sent in the handshake. HTTPS: "Authorization: Bearer".
		token := creds.Token
		o.GetJWT = func(context.Context) (string, error) { return token, nil }
	}

	slog.Debug("clickhouse: opening connection", "addr", c.opts.Addr(), "protocol", c.opts.Protocol,
		"database", database, "tls", c.opts.Secure, "token_auth", creds.UsesToken())
	return clickhouse.Open(o)
}

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(req)
}

// UseDatabase switches the session to another database. The switch is
// verified against the server first; on failure the session is unchanged.
func (c *Client) UseDatabase(ctx context.Context, database string) error {
	creds, err := c.auth.Authenticate(ctx)
	if err != nil {
		return &AuthError{err}
	}
	conn, err := c.open(creds, database)
	if err != nil {
		return err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return c.wrap(ctx, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn, c.token, c.database = conn, creds.Token, database
	return nil
}

// Set applies session settings to all subsequent statements. The settings
// are validated against the server first; on failure nothing changes.
func (c *Client) Set(ctx context.Context, settings map[string]string) error {
	conn, err := c.acquire(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	merged := clickhouse.Settings{}
	for k, v := range c.settings {
		merged[k] = v
	}
	c.mu.Unlock()
	for k, v := range settings {
		merged[k] = v
	}
	if err := conn.Exec(clickhouse.Context(ctx, clickhouse.WithSettings(merged)), "SELECT 1"); err != nil {
		return c.wrap(ctx, err)
	}
	c.mu.Lock()
	c.settings = merged
	c.mu.Unlock()
	return nil
}

// Stats are the server-reported read statistics of a statement.
type Stats struct {
	Rows  uint64
	Bytes uint64
}

type statsCounter struct{ rows, bytes atomic.Uint64 }

func (s *statsCounter) snapshot() Stats { return Stats{Rows: s.rows.Load(), Bytes: s.bytes.Load()} }

// queryContext attaches the session settings, a progress collector and,
// over HTTP, a query ID by which the statement can be killed.
func (c *Client) queryContext(ctx context.Context) (qctx context.Context, stats *statsCounter, queryID string) {
	stats = &statsCounter{}
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()
	opts := []clickhouse.QueryOption{
		clickhouse.WithSettings(settings),
		clickhouse.WithProgress(func(p *clickhouse.Progress) {
			stats.rows.Add(p.Rows)
			stats.bytes.Add(p.Bytes)
		}),
	}
	if c.opts.Protocol == config.ProtocolHTTP {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		queryID = "chcli-" + hex.EncodeToString(buf)
		opts = append(opts, clickhouse.WithQueryID(queryID))
	}
	return clickhouse.Context(ctx, opts...), stats, queryID
}

// stopOnServer makes sure a statement whose context was cancelled does not
// keep running. The native protocol cancels in-band. Over HTTP the server
// only notices a vanished client for read-only users, so the query is killed
// explicitly; failing to do so is not an error for the user (they asked to
// stop waiting, and they got that).
func (c *Client) stopOnServer(ctx context.Context, queryID string) {
	if queryID == "" || ctx.Err() == nil {
		return
	}
	killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.acquire(killCtx)
	if err == nil {
		// The ID is generated above from hex digits; no quoting concerns.
		err = conn.Exec(killCtx, "KILL QUERY WHERE query_id = '"+queryID+"' ASYNC")
	}
	if err != nil {
		slog.Debug("clickhouse: could not kill the cancelled query", "query_id", queryID, "error", err)
	}
}

// Exec runs a statement that returns no result set.
func (c *Client) Exec(ctx context.Context, sql string) (Stats, error) {
	conn, err := c.acquire(ctx)
	if err != nil {
		return Stats{}, err
	}
	qctx, stats, queryID := c.queryContext(ctx)
	if err := conn.Exec(qctx, sql); err != nil {
		c.stopOnServer(ctx, queryID)
		return Stats{}, c.wrap(ctx, err)
	}
	return stats.snapshot(), nil
}

// Query runs a statement and returns a streaming cursor over its result.
// The caller must Close the cursor before running another statement.
func (c *Client) Query(ctx context.Context, sql string) (*Rows, error) {
	conn, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	qctx, stats, queryID := c.queryContext(ctx)
	rows, err := conn.Query(qctx, sql)
	if err != nil {
		c.stopOnServer(ctx, queryID)
		return nil, c.wrap(ctx, err)
	}
	r := &Rows{rows: rows, client: c, stats: stats, ctx: ctx, queryID: queryID}
	for _, ct := range rows.ColumnTypes() {
		r.cols = append(r.cols, Column{Name: ct.Name(), Type: ct.DatabaseTypeName()})
		r.dest = append(r.dest, reflect.New(ct.ScanType()).Interface())
	}
	r.vals = make([]any, len(r.dest))
	return r, nil
}

// QueryStrings runs a query and returns every value rendered as a string.
// It buffers the whole result and is meant for small metadata queries.
func (c *Client) QueryStrings(ctx context.Context, sql string) ([][]string, error) {
	rows, err := c.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		rec := make([]string, len(vals))
		for i, v := range vals {
			rec[i] = fmt.Sprint(v)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Column describes a result column.
type Column struct {
	Name string
	Type string // ClickHouse type name, for example Nullable(DateTime64(3))
}

// Rows is a forward-only cursor over a query result. Rows are streamed from
// the server; nothing is buffered beyond the driver's current block.
type Rows struct {
	rows    driver.Rows
	client  *Client
	stats   *statsCounter
	ctx     context.Context
	queryID string
	cols    []Column
	dest    []any
	vals    []any
}

// Columns returns the result columns. It is empty for statements that
// return no result set (DDL and the like).
func (r *Rows) Columns() []Column { return r.cols }

// Next advances to the next row.
func (r *Rows) Next() bool { return r.rows.Next() }

// Values returns the current row. The slice is reused by the next call.
// NULLs are returned as nil.
func (r *Rows) Values() ([]any, error) {
	if err := r.rows.Scan(r.dest...); err != nil {
		return nil, r.client.wrap(r.ctx, err)
	}
	return deref(r.dest, r.vals), nil
}

// deref copies the scanned values out of their destinations into out,
// turning nil pointers (NULL) into nil and other pointers into their values.
func deref(dest, out []any) []any {
	for i, d := range dest {
		v := reflect.ValueOf(d).Elem()
		if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
			out[i] = nil
			continue
		}
		if v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		out[i] = v.Interface()
	}
	return out
}

// Totals returns the extra row produced by WITH TOTALS, if the query has one.
// It is available once all rows have been read.
func (r *Rows) Totals() (values []any, ok bool, err error) {
	dest := make([]any, len(r.dest))
	for i, d := range r.dest {
		dest[i] = reflect.New(reflect.TypeOf(d).Elem()).Interface()
	}
	if err := r.rows.Totals(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, r.client.wrap(r.ctx, err)
	}
	return deref(dest, make([]any, len(dest))), true, nil
}

// Err returns the error that ended iteration, if any.
func (r *Rows) Err() error { return r.client.wrap(r.ctx, r.rows.Err()) }

// Close releases the cursor and its connection. If the query was cancelled
// it is also stopped on the server.
func (r *Rows) Close() error {
	err := r.rows.Close()
	r.client.stopOnServer(r.ctx, r.queryID)
	return err
}

// Stats returns the read statistics reported by the server so far.
func (r *Rows) Stats() Stats { return r.stats.snapshot() }

// Authentication-related server error codes.
const (
	codeUnknownUser          = 192
	codeWrongPassword        = 193
	codeRequiredPassword     = 194
	codeAuthenticationFailed = 516
)

// wrap classifies driver errors: server exceptions pass through (except
// authentication failures), cancellations stay recognisable, and everything
// that looks like a broken or refused connection becomes a ConnError.
func (c *Client) wrap(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	// Whatever the driver reports once the caller's context is done is a
	// consequence of that (a closed socket, an aborted request, ...), and
	// transports word it differently. Report the cause, not the symptom.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// HTTP client errors embed the request URL; keep only the cause.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		switch ex.Code {
		case codeUnknownUser, codeWrongPassword, codeRequiredPassword, codeAuthenticationFailed:
			return &AuthError{err}
		}
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return &ConnError{Addr: c.opts.Addr(), Err: fmt.Errorf("%w (use --ca-cert for a private CA, or --insecure-skip-verify to disable verification)", err)}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, clickhouse.ErrAcquireConnTimeout) {
		return &ConnError{Addr: c.opts.Addr(), Err: err}
	}
	return err
}
