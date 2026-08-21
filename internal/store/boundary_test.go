package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"grok-gateway-proxy/internal/config"
)

// Concurrent inserts from multiple goroutines must all succeed without
// "database is locked" errors. SQLite with WAL + MaxOpenConns(1) serializes
// writes, so this test verifies the busy_timeout is effective.
func TestStoreConcurrentInserts(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const goroutines = 8
	const perGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines*perGoroutine)

	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				log := RequestLog{
					ID:        fmt.Sprintf("g%d-%d", gid, i),
					StartedAt: time.Now().UTC(),
					GatewayID: "oc",
					Model:     "test-model",
				}
				if err := store.Insert(context.Background(), log); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent insert failed: %v", err)
	}

	count, err := store.Count(context.Background(), LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if count != goroutines*perGoroutine {
		t.Fatalf("expected %d rows, got %d", goroutines*perGoroutine, count)
	}
}

// Concurrent reads while writes are happening must not block or corrupt data.
func TestStoreConcurrentReadWrite(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed with some initial data.
	for i := 0; i < 20; i++ {
		_ = store.Insert(context.Background(), RequestLog{
			ID:        fmt.Sprintf("seed-%d", i),
			StartedAt: time.Now().UTC(),
			GatewayID: "oc",
		})
	}

	const writers = 4
	const readers = 4
	var wg sync.WaitGroup
	wg.Add(writers + readers)
	errs := make(chan error, writers+readers)

	// Writers keep inserting.
	for w := 0; w < writers; w++ {
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if err := store.Insert(context.Background(), RequestLog{
					ID:        fmt.Sprintf("w%d-%d", wid, i),
					StartedAt: time.Now().UTC(),
					GatewayID: "st",
				}); err != nil {
					errs <- err
				}
			}
		}(w)
	}

	// Readers keep querying.
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				logs, err := store.List(context.Background(), LogFilter{Limit: 10})
				if err != nil {
					errs <- err
					return
				}
				if len(logs) > 10 {
					errs <- fmt.Errorf("List returned %d rows, limit was 10", len(logs))
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read/write failed: %v", err)
	}
}

// Inserting a log with an empty body must not fail.
func TestStoreInsertEmptyBody(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	log := RequestLog{
		ID:                   "empty-body",
		StartedAt:            time.Now().UTC(),
		GatewayID:            "oc",
		RequestBody:          nil,
		UpstreamBody:         nil,
		ResponseBody:         nil,
		UpstreamResponseBody: nil,
	}
	if err := store.Insert(context.Background(), log); err != nil {
		t.Fatalf("insert with nil body failed: %v", err)
	}

	got, err := store.Get(context.Background(), "empty-body")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RequestBody) != 0 || len(got.ResponseBody) != 0 {
		t.Fatalf("empty bodies were not stored as empty: request=%q response=%q", got.RequestBody, got.ResponseBody)
	}
}

// Inserting a log with a duplicate ID must fail (primary key constraint).
func TestStoreInsertDuplicateIDFails(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	log := RequestLog{ID: "dup", StartedAt: time.Now().UTC(), GatewayID: "oc"}
	if err := store.Insert(context.Background(), log); err != nil {
		t.Fatalf("first insert failed: %v", err)
	}
	if err := store.Insert(context.Background(), log); err == nil {
		t.Fatal("expected error on duplicate ID insert, got nil")
	}
}

// Metrics with zero rows must return zero values, not nil pointers that
// could cause nil dereferences in the API layer.
func TestStoreMetricsEmpty(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	m, err := store.Metrics(context.Background(), LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Requests != 0 || m.Successes != 0 || m.Failures != 0 {
		t.Fatalf("expected zero metrics, got %+v", m)
	}
	if m.CacheHitRate != nil {
		t.Fatalf("expected nil hit rate for empty store, got %v", *m.CacheHitRate)
	}
}

// List with a limit larger than the row count must return all available
// rows without error.
func TestStoreListLimitExceedsRowCount(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for i := 0; i < 3; i++ {
		if err := store.Insert(context.Background(), RequestLog{
			ID:        fmt.Sprintf("row-%d", i),
			StartedAt: time.Now().UTC(),
			GatewayID: "oc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	logs, err := store.List(context.Background(), LogFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(logs))
	}
}

// Delete with a cutoff time before all rows must delete nothing.
func TestStoreDeleteWithEarlyCutoff(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	if err := store.Insert(context.Background(), RequestLog{ID: "r1", StartedAt: now}); err != nil {
		t.Fatal(err)
	}

	early := now.Add(-time.Hour)
	n, err := store.Delete(context.Background(), &early)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 deletions with early cutoff, got %d", n)
	}
}

// Delete with a nil cutoff must remove all rows and return the count.
func TestStoreDeleteAllRemovesEverything(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if err := store.Insert(context.Background(), RequestLog{
			ID:        fmt.Sprintf("d-%d", i),
			StartedAt: now.Add(-time.Duration(i) * time.Hour),
			GatewayID: "oc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := store.Delete(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("expected 5 deletions, got %d", n)
	}
	count, _ := store.Count(context.Background(), LogFilter{})
	if count != 0 {
		t.Fatalf("expected 0 rows after delete all, got %d", count)
	}
}

// Reopening a store on an existing database must preserve all data.
func TestStoreReopenPreservesData(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy.db")
	store1, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	log := RequestLog{
		ID:               "persist-test",
		StartedAt:        time.Now().UTC(),
		GatewayID:        "oc",
		GatewayName:      "OpenCode Zen",
		Prefix:           "/oc",
		IngressProtocol:  config.ProtocolResponses,
		UpstreamProtocol: config.ProtocolResponses,
		Model:            "test-model",
		StatusCode:       200,
		Success:          true,
		Usage:            UsageMetrics{InputTokens: 10, OutputTokens: 5, UsagePresent: true},
	}
	if err := store1.Insert(context.Background(), log); err != nil {
		t.Fatal(err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	got, err := store2.Get(context.Background(), "persist-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.GatewayName != "OpenCode Zen" || got.Model != "test-model" || !got.Success {
		t.Fatalf("data was not preserved after reopen: %+v", got)
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 {
		t.Fatalf("usage was not preserved: %+v", got.Usage)
	}
}
