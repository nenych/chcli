// Package metadata caches the server objects that completion needs:
// databases, tables, columns, functions and friends. The cache is filled
// from system tables in the background; readers always get an immutable
// snapshot and never wait for the server.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nenych/chcli/internal/sqlutil"
)

// Table is a table-like object: a table, view or dictionary.
type Table struct {
	Database string
	Name     string
	Engine   string
}

// Kind classifies the table by its engine: "view", "dictionary" or "table".
func (t Table) Kind() string {
	switch {
	case strings.HasSuffix(t.Engine, "View"):
		return "view"
	case t.Engine == "Dictionary":
		return "dictionary"
	}
	return "table"
}

// Column is a table column.
type Column struct {
	Name string
	Type string
}

// Function is a SQL function.
type Function struct {
	Name      string
	Aggregate bool
}

// Snapshot is an immutable view of the cached metadata.
type Snapshot struct {
	Databases      []string
	Tables         map[string][]Table  // by database
	Columns        map[string][]Column // by TableKey
	Functions      []Function
	TableFunctions []string
	DataTypes      []string
	Engines        []string
	Formats        []string
	Settings       []string
	Keywords       []string
	// LoadedAt is when the snapshot was fetched; zero for the built-in fallback.
	LoadedAt time.Time
}

// TableKey is the key of Snapshot.Columns.
func TableKey(database, table string) string { return database + "\x00" + table }

// HasDatabase reports whether a database with this exact name is known.
func (s *Snapshot) HasDatabase(name string) bool {
	return slices.Contains(s.Databases, name)
}

// fallback is served until the first successful load: enough static
// knowledge for keyword and type completion to work offline.
var fallback = &Snapshot{
	Tables:   map[string][]Table{},
	Columns:  map[string][]Column{},
	Keywords: sqlutil.Keywords,
	DataTypes: []string{
		"Array", "Bool", "Date", "Date32", "DateTime", "DateTime64", "Decimal", "Enum8", "Enum16",
		"FixedString", "Float32", "Float64", "IPv4", "IPv6", "Int8", "Int16", "Int32", "Int64", "Int128",
		"Int256", "JSON", "LowCardinality", "Map", "Nullable", "String", "Tuple", "UInt8", "UInt16",
		"UInt32", "UInt64", "UInt128", "UInt256", "UUID",
	},
}

// Querier runs a metadata query and returns all values as strings.
type Querier interface {
	QueryStrings(ctx context.Context, sql string) ([][]string, error)
}

// Cache holds the current snapshot and refreshes it on demand.
type Cache struct {
	q        Querier
	snapshot atomic.Pointer[Snapshot]

	mu         sync.Mutex
	refreshing bool
	again      bool // a refresh was requested while one was running
	lastErr    error
}

// NewCache creates a cache that loads through q.
func NewCache(q Querier) *Cache {
	c := &Cache{q: q}
	c.snapshot.Store(fallback)
	return c
}

// Snapshot returns the current metadata. It never blocks and never returns nil.
func (c *Cache) Snapshot() *Snapshot { return c.snapshot.Load() }

// LastError returns the error of the most recent refresh, if it had one.
func (c *Cache) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// RefreshAsync starts a background refresh unless one is already running, in
// which case one more refresh is queued so that the request is not lost.
func (c *Cache) RefreshAsync() {
	c.mu.Lock()
	if c.refreshing {
		c.again = true
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.mu.Unlock()

	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := c.load(ctx)
			cancel()

			c.mu.Lock()
			c.lastErr = err
			if !c.again {
				c.refreshing = false
				c.mu.Unlock()
				return
			}
			c.again = false
			c.mu.Unlock()
		}
	}()
}

// RefreshIfOlder starts a background refresh when the snapshot is older than maxAge.
func (c *Cache) RefreshIfOlder(maxAge time.Duration) {
	if time.Since(c.Snapshot().LoadedAt) > maxAge {
		c.RefreshAsync()
	}
}

// Refresh reloads the metadata and waits for the result.
func (c *Cache) Refresh(ctx context.Context) error {
	err := c.load(ctx)
	c.mu.Lock()
	c.lastErr = err
	c.mu.Unlock()
	return err
}

// load fetches every section. Sections fail independently: a user without
// access to one system table still gets completion for the rest, and a
// failed section keeps its previous contents. The error describes whatever
// could not be loaded.
func (c *Cache) load(ctx context.Context) error {
	started := time.Now()
	prev := c.Snapshot()
	next := &Snapshot{
		Databases: prev.Databases, Tables: prev.Tables, Columns: prev.Columns,
		Functions: prev.Functions, TableFunctions: prev.TableFunctions, DataTypes: prev.DataTypes,
		Engines: prev.Engines, Formats: prev.Formats, Settings: prev.Settings, Keywords: prev.Keywords,
	}
	var errs []error
	section := func(name, sql string, apply func(rows [][]string)) {
		rows, err := c.q.QueryStrings(ctx, sql)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return
		}
		apply(rows)
	}
	names := func(dst *[]string) func([][]string) {
		return func(rows [][]string) {
			out := make([]string, len(rows))
			for i, r := range rows {
				out[i] = r[0]
			}
			*dst = out
		}
	}

	section("databases", "SELECT name FROM system.databases ORDER BY name", names(&next.Databases))
	section("tables", "SELECT database, name, engine FROM system.tables WHERE NOT is_temporary ORDER BY database, name",
		func(rows [][]string) {
			tables := map[string][]Table{}
			for _, r := range rows {
				if strings.HasPrefix(r[1], ".inner") { // storage of materialized views
					continue
				}
				tables[r[0]] = append(tables[r[0]], Table{Database: r[0], Name: r[1], Engine: r[2]})
			}
			next.Tables = tables
		})
	section("columns", "SELECT database, table, name, type FROM system.columns ORDER BY database, table, position",
		func(rows [][]string) {
			columns := map[string][]Column{}
			for _, r := range rows {
				key := TableKey(r[0], r[1])
				columns[key] = append(columns[key], Column{Name: r[2], Type: r[3]})
			}
			next.Columns = columns
		})
	section("functions", "SELECT name, toString(is_aggregate) FROM system.functions ORDER BY name",
		func(rows [][]string) {
			functions := make([]Function, len(rows))
			for i, r := range rows {
				functions[i] = Function{Name: r[0], Aggregate: r[1] == "1"}
			}
			next.Functions = functions
		})
	section("table functions", "SELECT name FROM system.table_functions ORDER BY name", names(&next.TableFunctions))
	section("data types", "SELECT name FROM system.data_type_families ORDER BY name", names(&next.DataTypes))
	section("engines", "SELECT name FROM system.table_engines ORDER BY name", names(&next.Engines))
	section("formats", "SELECT name FROM system.formats ORDER BY name", names(&next.Formats))
	section("settings", "SELECT name FROM system.settings ORDER BY name", names(&next.Settings))
	// system.keywords only exists on newer servers; the static list stays otherwise.
	if rows, err := c.q.QueryStrings(ctx, "SELECT keyword FROM system.keywords WHERE NOT match(keyword, '[^A-Za-z]') ORDER BY keyword"); err == nil && len(rows) > 0 {
		names(&next.Keywords)(rows)
	}

	next.LoadedAt = time.Now()
	c.snapshot.Store(next)
	err := errors.Join(errs...)
	slog.Debug("metadata: refreshed", "elapsed", time.Since(started).Round(time.Millisecond),
		"databases", len(next.Databases), "functions", len(next.Functions), "error", err)
	return err
}
