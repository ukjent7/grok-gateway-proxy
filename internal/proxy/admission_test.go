package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

// The budget is only worth having if the bytes come back: a reservation that
// leaked would throttle the process to a stop one request at a time.
func TestBodyAdmissionChargesAndReturns(t *testing.T) {
	budget := newBodyAdmission(10)
	if err := budget.reserve(context.Background(), 6); err != nil {
		t.Fatal(err)
	}
	if free := heldFree(budget); free != 4 {
		t.Fatalf("free = %d after reserving 6 of 10, want 4", free)
	}
	budget.release(6)
	if free := heldFree(budget); free != 10 {
		t.Fatalf("free = %d after release, want the whole budget back", free)
	}
}

// A request that arrives while the pool is empty is queued, not refused: the
// whole point of waiting is that the bytes are on their way back.
func TestBodyAdmissionWakesWhenBytesComeBack(t *testing.T) {
	budget := newBodyAdmission(10)
	if err := budget.reserve(context.Background(), 10); err != nil {
		t.Fatal(err)
	}

	granted := make(chan error, 1)
	go func() { granted <- budget.reserve(context.Background(), 4) }()

	select {
	case err := <-granted:
		t.Fatalf("reserve returned %v while every byte was still held", err)
	case <-time.After(50 * time.Millisecond):
	}

	budget.release(6)
	select {
	case err := <-granted:
		if err != nil {
			t.Fatalf("reserve after a release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a release did not wake a queued reservation")
	}
	// 8 of the 10 are held: the first caller gave back 6 and the queued one
	// took 4 of those. What must not happen is the release going unclaimed.
	if free := heldFree(budget); free != 2 {
		t.Fatalf("free = %d, want 2 of a 10-byte budget", free)
	}
}

// The wait must end when the client hangs up, or a queue of disconnected
// requests would hold the budget open for callers that are already gone.
func TestBodyAdmissionGivesUpWhenTheCallerLeaves(t *testing.T) {
	budget := newBodyAdmission(4)
	if err := budget.reserve(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- budget.reserve(ctx, 1) }()

	// Cancelled after the goroutine is queued: cancel is what must wake it.
	time.AfterFunc(30*time.Millisecond, cancel)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reserve after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the caller did not end the wait")
	}
}

// A Proxy assembled as a struct literal has no budget, and that must mean "no
// gate here" rather than a nil-pointer panic on the request path.
func TestBodyAdmissionNilAdmitsEverything(t *testing.T) {
	var budget *bodyAdmission
	if err := budget.reserve(context.Background(), maxBodyBytes); err != nil {
		t.Fatalf("nil budget rejected a reservation: %v", err)
	}
	budget.release(maxBodyBytes)
}

// The charge is what makes the bound mean anything: a declared length is
// charged as itself because the read will not exceed it, and an undeclared one
// (chunked, or no Content-Length) is charged the cap because the unknown must
// not be the case that gets in cheapest.
func TestChargeForBody(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared int64
		want     int64
	}{
		{"chunked", -1, maxBodyBytes},
		{"empty body", 0, maxBodyBytes},
		{"ordinary prompt", 40 << 10, 40 << 10},
		{"at the cap", maxBodyBytes, maxBodyBytes},
		// A lie about size cannot buy a cheaper reservation than the cap the
		// read enforces anyway.
		{"over the cap", maxBodyBytes * 3, maxBodyBytes},
	} {
		if got := chargeForBody(test.declared); got != test.want {
			t.Errorf("%s: chargeForBody(%d) = %d, want %d", test.name, test.declared, got, test.want)
		}
	}
}

// An admission failure has to reach the client as a 503 and the audit log as a
// recorded failure, without a byte of the body read and without an upstream
// call — the request never got that far.
func TestServeHTTPRefusesWhenTheBodyBudgetIsGone(t *testing.T) {
	st, err := store.OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	called := false
	// HTTPS because that is all config accepts for a built-in gateway. Nothing
	// dials it: the request must be refused before routing, and the server's
	// only job is to say whether that happened.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()

	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	if _, err := cfg.PatchGateway("ds", config.GatewayPatch{BaseURL: &upstream.URL}); err != nil {
		t.Fatal(err)
	}
	p := NewProxy(cfg, st, slog.Default())
	// Everything reserved: the next caller has nowhere to go, and a cancelled
	// context stands in for the 30-second queue without the test waiting it out.
	if err := p.bodies.reserve(context.Background(), inFlightBodyBudget); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ds/responses", strings.NewReader(`{"model":"x"}`))
	req = req.WithContext(ctx)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", recorder.Code, recorder.Body.String())
	}
	if called {
		t.Fatal("a request that was never admitted still reached the upstream")
	}
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected the refusal to be audited, got %d rows", len(logs))
	}
	if logs[0].StatusCode != http.StatusServiceUnavailable || logs[0].Error == "" {
		t.Fatalf("audit row = status %d error %q, want 503 with the reason", logs[0].StatusCode, logs[0].Error)
	}
	if logs[0].RequestBody != nil {
		t.Fatalf("a refused body was still captured: %q", logs[0].RequestBody)
	}
}

// heldFree reads the remaining budget the way the semaphore itself would: the
// accounting is only meaningful under the lock that guards it.
func heldFree(b *bodyAdmission) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.free
}
