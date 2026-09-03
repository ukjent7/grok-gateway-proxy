package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"grok-gateway-proxy/internal/store"
)

func TestEventsStreamPushesOnChange(t *testing.T) {
	app, st := newTestApp(t)
	srv := httptest.NewServer(app)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	events := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case events <- scanner.Text():
			default:
			}
		}
		close(events)
	}()

	waitEvent := func(want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case line, ok := <-events:
				if !ok {
					t.Fatal("event stream closed early")
				}
				if strings.HasPrefix(line, want) {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %q event", want)
			}
		}
	}

	waitEvent("event: change")
	if err := st.Insert(ctx, store.RequestLog{ID: "sse-1", GatewayID: "st", Success: true}); err != nil {
		t.Fatal(err)
	}
	waitEvent("event: change")
}
