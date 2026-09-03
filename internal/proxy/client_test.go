package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"grok-gateway-proxy/internal/config"
)

func TestClientTransportsSplitByRequestMode(t *testing.T) {
	p := NewProxy(config.DefaultConfig(t.TempDir()+"/config.json"), nil, nil)

	for name, client := range map[string]*http.Client{
		"Client":             p.Client,
		"DirectClient":       p.DirectClient,
		"StreamClient":       p.StreamClient,
		"StreamDirectClient": p.StreamDirectClient,
	} {
		if client == nil {
			t.Fatalf("%s is nil", name)
		}
	}

	syncTransport, ok := p.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("Client transport is not *http.Transport")
	}
	if syncTransport.ResponseHeaderTimeout != 0 {
		t.Fatalf("non-streaming client must have no header timeout, got %v", syncTransport.ResponseHeaderTimeout)
	}
	streamTransport, ok := p.StreamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("StreamClient transport is not *http.Transport")
	}
	if streamTransport.ResponseHeaderTimeout != streamResponseHeaderTimeout {
		t.Fatalf("streaming client header timeout = %v, want %v", streamTransport.ResponseHeaderTimeout, streamResponseHeaderTimeout)
	}
	if p.DirectClient.Transport.(*http.Transport).ResponseHeaderTimeout != 0 {
		t.Fatal("direct non-streaming client must have no header timeout")
	}
	if p.StreamDirectClient.Transport.(*http.Transport).ResponseHeaderTimeout != streamResponseHeaderTimeout {
		t.Fatal("direct streaming client must carry the streaming header timeout")
	}
}

func TestClientForSelectsByModeAndProxySetting(t *testing.T) {
	p := &Proxy{
		Client:             &http.Client{},
		DirectClient:       &http.Client{},
		StreamClient:       &http.Client{},
		StreamDirectClient: &http.Client{},
	}
	gateway := config.GatewayConfig{UseProxy: true}
	if p.ClientFor(gateway, false) != p.Client {
		t.Fatal("non-streaming proxied request must use the sync proxy client")
	}
	if p.ClientFor(gateway, true) != p.StreamClient {
		t.Fatal("streaming proxied request must use the stream proxy client")
	}
	gateway.UseProxy = false
	if p.ClientFor(gateway, false) != p.DirectClient {
		t.Fatal("non-streaming direct request must use the sync direct client")
	}
	if p.ClientFor(gateway, true) != p.StreamDirectClient {
		t.Fatal("streaming direct request must use the stream direct client")
	}
}

func TestClientForFallsBackWhenStreamClientsAbsent(t *testing.T) {
	p := &Proxy{Client: &http.Client{}, DirectClient: &http.Client{}}
	gateway := config.GatewayConfig{UseProxy: true}
	if p.ClientFor(gateway, true) != p.Client {
		t.Fatal("missing stream client should fall back to the sync proxy client")
	}
	gateway.UseProxy = false
	if p.ClientFor(gateway, true) != p.DirectClient {
		t.Fatal("missing stream direct client should fall back to the sync direct client")
	}
}

func TestSetProxyURLReplacesAllClients(t *testing.T) {
	p := NewProxy(config.DefaultConfig(t.TempDir()+"/config.json"), nil, nil)
	old := [4]*http.Client{p.Client, p.DirectClient, p.StreamClient, p.StreamDirectClient}

	p.SetProxyURL("http://127.0.0.1:7890")

	next := [4]*http.Client{p.Client, p.DirectClient, p.StreamClient, p.StreamDirectClient}
	for i, client := range next {
		if client == nil {
			t.Fatalf("client %d is nil after SetProxyURL", i)
		}
		if client == old[i] {
			t.Fatalf("client %d was not replaced", i)
		}
		transport := client.Transport.(*http.Transport)

		shouldProxy := i == 0 || i == 2
		if !shouldProxy {
			if transport.Proxy != nil {
				t.Fatalf("direct client %d must not route through a proxy", i)
			}
			continue
		}
		if transport.Proxy == nil {
			t.Fatalf("proxied client %d has no proxy function", i)
		}
		proxyURL, err := transport.Proxy(httptest.NewRequest(http.MethodGet, "https://example.test/", nil))
		if err != nil {
			t.Fatal(err)
		}
		if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:7890" {
			t.Fatalf("proxied client %d did not pick up the new proxy URL", i)
		}
	}
}

func TestStreamResponseHeaderTimeoutIsBounded(t *testing.T) {
	if streamResponseHeaderTimeout <= 0 || streamResponseHeaderTimeout > 5*time.Minute {
		t.Fatalf("stream header timeout %v outside the sane range", streamResponseHeaderTimeout)
	}
}
