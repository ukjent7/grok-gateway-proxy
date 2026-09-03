package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreTimeSeriesEmptyDatabase(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	series, err := store.TimeSeries(context.Background(), LogFilter{}, 48)
	if err != nil {
		t.Fatalf("empty store should answer with an empty series, not an error: %v", err)
	}
	if len(series.Buckets) != 0 {
		t.Fatalf("expected no buckets for an empty store, got %d", len(series.Buckets))
	}
}

func TestStoreTimeSeriesBucketsWindow(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	step := int64(30)
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	base = time.Unix(base.Unix()-mod(base.Unix(), step), 0)
	mk := func(id string, at time.Time, success bool) RequestLog {
		return RequestLog{ID: id, StartedAt: at, GatewayID: "oc", Success: success,
			Usage: UsageMetrics{PromptTokens: 100, InputTokens: 100, OutputTokens: 20,
				ReasoningTokens: 5, CacheReadTokens: 60, CacheSupported: true, UsagePresent: true},
		}
	}
	logs := []RequestLog{
		mk("a", base.Add(10*time.Second), true),
		mk("b", base.Add(10*time.Second), false),
		mk("c", base.Add(95*time.Second), true),
	}
	for _, log := range logs {
		if err := store.Insert(context.Background(), log); err != nil {
			t.Fatal(err)
		}
	}

	from := base
	to := base.Add(120 * time.Second)
	series, err := store.TimeSeries(context.Background(), LogFilter{From: &from, To: &to}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if series.BucketSeconds != step {
		t.Fatalf("expected %ds buckets, got %d", step, series.BucketSeconds)
	}
	if !series.From.Equal(base) || !series.To.Equal(to) {
		t.Fatalf("unexpected window: %v .. %v", series.From, series.To)
	}
	if len(series.Buckets) != 4 {
		t.Fatalf("expected 4 buckets, got %d: %+v", len(series.Buckets), series.Buckets)
	}
	for i, want := range []struct{ requests, failures int64 }{{2, 1}, {0, 0}, {0, 0}, {1, 0}} {
		got := series.Buckets[i]
		if got.Requests != want.requests || got.Failures != want.failures {
			t.Fatalf("bucket %d: want (%d,%d), got (%d,%d)", i, want.requests, want.failures, got.Requests, got.Failures)
		}
		if !got.Start.Equal(time.Unix(base.Unix()+int64(i)*step, 0)) {
			t.Fatalf("bucket %d start = %v, want %v", i, got.Start, time.Unix(base.Unix()+int64(i)*step, 0))
		}
	}
	first, last := series.Buckets[0], series.Buckets[3]
	if first.PromptTokens != 200 || first.OutputTokens != 40 || first.CacheReadTokens != 120 || first.ReasoningTokens != 10 {
		t.Fatalf("bucket 0 token sums wrong: %+v", first)
	}
	if last.PromptTokens != 100 || last.OutputTokens != 20 {
		t.Fatalf("bucket 3 token sums wrong: %+v", last)
	}

	all, err := store.TimeSeries(context.Background(), LogFilter{}, 240)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, bucket := range all.Buckets {
		total += bucket.Requests
	}
	if total != 3 {
		t.Fatalf("expected every request accounted for, got %d over %d buckets", total, len(all.Buckets))
	}
	if all.From.After(logs[0].StartedAt) {
		t.Fatalf("all-range window should start at or before the first row: %v", all.From)
	}
}
