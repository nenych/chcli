package metadata

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeQuerier answers metadata queries from canned rows, keyed by the system
// table the query reads.
type fakeQuerier struct {
	mu      sync.Mutex
	rows    map[string][][]string
	fail    map[string]error
	queries int
	block   chan struct{} // when set, every query waits for it
}

func (f *fakeQuerier) QueryStrings(ctx context.Context, sql string) ([][]string, error) {
	f.mu.Lock()
	f.queries++
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for table, err := range f.fail {
		if strings.Contains(sql, "FROM "+table+" ") {
			return nil, err
		}
	}
	for table, rows := range f.rows {
		if strings.Contains(sql, "FROM "+table+" ") {
			return rows, nil
		}
	}
	return nil, nil
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		fail: map[string]error{},
		rows: map[string][][]string{
			"system.databases": {{"chronicle"}, {"default"}, {"system"}},
			"system.tables": {
				{"chronicle", ".inner_id.1234", "MergeTree"},
				{"chronicle", "events", "MergeTree"},
				{"chronicle", "events_mv", "MaterializedView"},
				{"chronicle", "users_dict", "Dictionary"},
				{"default", "t", "Memory"},
			},
			"system.columns": {
				{"chronicle", "events", "event_id", "UInt64"},
				{"chronicle", "events", "event_type", "String"},
				{"default", "t", "x", "UInt8"},
			},
			"system.functions":          {{"count", "1"}, {"toDate", "0"}},
			"system.table_functions":    {{"numbers"}, {"s3"}},
			"system.data_type_families": {{"String"}, {"UInt64"}},
			"system.table_engines":      {{"MergeTree"}},
			"system.formats":            {{"CSV"}},
			"system.settings":           {{"max_threads"}},
			"system.keywords":           {{"SELECT"}, {"FROM"}},
		},
	}
}

func TestFallbackSnapshot(t *testing.T) {
	snap := NewCache(newFakeQuerier()).Snapshot()
	if snap == nil || len(snap.Keywords) == 0 || len(snap.DataTypes) == 0 {
		t.Fatalf("the fallback snapshot must offer keywords and types: %+v", snap)
	}
	if !snap.LoadedAt.IsZero() || len(snap.Databases) != 0 {
		t.Errorf("the fallback snapshot must be recognisable as not loaded")
	}
}

func TestRefresh(t *testing.T) {
	c := NewCache(newFakeQuerier())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := c.Snapshot()
	if !reflect.DeepEqual(snap.Databases, []string{"chronicle", "default", "system"}) {
		t.Errorf("databases = %v", snap.Databases)
	}
	tables := snap.Tables["chronicle"]
	if len(tables) != 3 {
		t.Fatalf("chronicle tables = %+v (inner tables must be skipped)", tables)
	}
	kinds := map[string]string{}
	for _, tbl := range tables {
		kinds[tbl.Name] = tbl.Kind()
	}
	if want := map[string]string{"events": "table", "events_mv": "view", "users_dict": "dictionary"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	if cols := snap.Columns[TableKey("chronicle", "events")]; !reflect.DeepEqual(cols, []Column{{"event_id", "UInt64"}, {"event_type", "String"}}) {
		t.Errorf("columns = %+v", cols)
	}
	if !reflect.DeepEqual(snap.Functions, []Function{{"count", true}, {"toDate", false}}) {
		t.Errorf("functions = %+v", snap.Functions)
	}
	if !reflect.DeepEqual(snap.Keywords, []string{"SELECT", "FROM"}) || !reflect.DeepEqual(snap.TableFunctions, []string{"numbers", "s3"}) {
		t.Errorf("keywords = %v, table functions = %v", snap.Keywords, snap.TableFunctions)
	}
	if snap.LoadedAt.IsZero() || !snap.HasDatabase("chronicle") || snap.HasDatabase("nope") {
		t.Errorf("snapshot bookkeeping wrong")
	}
}

// One unreadable system table must not take completion down with it.
func TestRefreshToleratesPartialFailure(t *testing.T) {
	q := newFakeQuerier()
	c := NewCache(q)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	q.mu.Lock()
	q.fail["system.columns"] = errors.New("ACCESS_DENIED")
	q.fail["system.keywords"] = errors.New("UNKNOWN_TABLE") // older server
	q.rows["system.databases"] = [][]string{{"chronicle"}, {"default"}, {"new_db"}, {"system"}}
	q.mu.Unlock()

	err := c.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "columns: ACCESS_DENIED") {
		t.Errorf("error = %v, want it to name the failed section", err)
	}
	if strings.Contains(err.Error(), "UNKNOWN_TABLE") {
		t.Errorf("a missing system.keywords table is expected on older servers and is not an error: %v", err)
	}
	if c.LastError() == nil {
		t.Error("LastError must report the failed refresh")
	}
	snap := c.Snapshot()
	if !snap.HasDatabase("new_db") {
		t.Error("sections that loaded must be updated")
	}
	if len(snap.Columns[TableKey("chronicle", "events")]) != 2 {
		t.Error("a failed section must keep its previous contents")
	}
	if len(snap.Keywords) == 0 {
		t.Error("keywords must survive a missing system.keywords")
	}
}

func TestRefreshAsyncCoalesces(t *testing.T) {
	q := newFakeQuerier()
	q.block = make(chan struct{})
	c := NewCache(q)

	for range 10 { // a burst of DDL statements
		c.RefreshAsync()
	}
	close(q.block)
	waitFor(t, func() bool { return !c.Snapshot().LoadedAt.IsZero() && !refreshing(c) })

	q.mu.Lock()
	queries := q.queries
	q.mu.Unlock()
	// Ten requests collapse into the running refresh plus one follow-up.
	if queries != 20 {
		t.Errorf("ran %d queries, want 20 (two full refreshes of 10 queries)", queries)
	}
	if !c.Snapshot().HasDatabase("chronicle") {
		t.Error("snapshot not loaded")
	}
}

func TestRefreshIfOlder(t *testing.T) {
	q := newFakeQuerier()
	c := NewCache(q)
	c.RefreshIfOlder(time.Hour) // never loaded: refreshes
	waitFor(t, func() bool { return !c.Snapshot().LoadedAt.IsZero() && !refreshing(c) })
	q.mu.Lock()
	before := q.queries
	q.mu.Unlock()

	c.RefreshIfOlder(time.Hour) // fresh: nothing to do
	time.Sleep(20 * time.Millisecond)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queries != before {
		t.Error("a fresh snapshot must not be reloaded")
	}
}

func refreshing(c *Cache) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshing
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
