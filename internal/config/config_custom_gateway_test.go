package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddGatewayCreatesCustomResponsesGateway(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := LoadConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// The leading slash is optional on input but never part of the id.
	created, err := cfg.AddGateway(NewGateway{Prefix: "mygate", Name: "My Gate", BaseURL: "https://api.example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "mygate" || created.Prefix != "/mygate" {
		t.Fatalf("identity = %q / %q, want mygate and /mygate", created.ID, created.Prefix)
	}
	// Protocol is not a free field: a custom gateway exists to reuse the
	// standard Responses adapter.
	if created.Protocol != ProtocolResponses {
		t.Fatalf("protocol = %q, want %q", created.Protocol, ProtocolResponses)
	}
	if !created.Enabled || !created.UseProxy {
		t.Fatalf("new gateway should start enabled and behind the global proxy: %+v", created)
	}
	if created.UserAgentOverride != DefaultUserAgentOverride {
		t.Fatalf("user agent = %q, want the shipped default", created.UserAgentOverride)
	}

	reloaded, err := LoadConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Gateways["mygate"]; !ok {
		t.Fatalf("custom gateway did not survive a reload: %v", reloaded.Gateways)
	}
}

func TestAddGatewayRejectsUnusablePrefixes(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		// contains is the substring the error must mention; "" only requires
		// that the call failed.
		contains string
	}{
		{name: "empty", prefix: ""},
		{name: "uppercase", prefix: "/MyGate"},
		{name: "leading dash", prefix: "-mygate"},
		{name: "nested path", prefix: "/a/b", contains: "single segment"},
		{name: "query smuggling", prefix: "/a?x=1"},
		{name: "too long", prefix: "/" + strings.Repeat("a", 33)},
		{name: "reserved console route", prefix: "/api", contains: "reserved"},
		{name: "built-in gateway", prefix: "/ds", contains: "built-in"},
		{name: "removed legacy gateway", prefix: "/oc", contains: "removed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = cfg.AddGateway(NewGateway{Prefix: test.prefix, Name: "x"})
			if err == nil {
				t.Fatalf("prefix %q was accepted", test.prefix)
			}
			if test.contains != "" && !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %q, want it to mention %q", err, test.contains)
			}
			if len(cfg.Gateways) != len(DefaultGateways) {
				t.Fatalf("a rejected create left gateways behind: %v", cfg.Gateways)
			}
		})
	}
}

func TestAddGatewayRejectsDuplicatePrefixAndRollsBack(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddGateway(NewGateway{Prefix: "/first", Name: "First"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddGateway(NewGateway{Prefix: "/first", Name: "Second"}); err == nil {
		t.Fatal("a second gateway with the same prefix was accepted")
	}
	if got := cfg.Gateways["first"].Name; got != "First" {
		t.Fatalf("the failed create overwrote the existing gateway: %q", got)
	}
}

func TestDeleteGatewayOnlyRemovesCustomGateways(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddGateway(NewGateway{Prefix: "/temporary", Name: "Temporary"}); err != nil {
		t.Fatal(err)
	}

	if err := cfg.DeleteGateway("ds"); !errors.Is(err, ErrBuiltinGateway) {
		t.Fatalf("deleting a built-in gateway returned %v, want ErrBuiltinGateway", err)
	}
	if err := cfg.DeleteGateway("never-existed"); !errors.Is(err, ErrUnknownGateway) {
		t.Fatalf("deleting an unknown gateway returned %v, want ErrUnknownGateway", err)
	}
	if err := cfg.DeleteGateway("temporary"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Gateways["temporary"]; ok {
		t.Fatal("the gateway survived deletion in memory")
	}

	reloaded, err := LoadConfig(cfg.ConfigPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Gateways["temporary"]; ok {
		t.Fatal("the deleted gateway came back after a reload")
	}
	for id := range DefaultGateways {
		if _, ok := reloaded.Gateways[id]; !ok {
			t.Fatalf("deleting a custom gateway dropped built-in %q", id)
		}
	}
}

// A config written by an older build mixes three cases in one map: gateways
// this build ships, gateways the user added, and gateways this build removed.
// Only the second group may survive.
func TestLoadConfigKeepsCustomAndDropsLegacyGateways(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"listen_addr":"127.0.0.1:8787","gateways":{` +
		`"ds":{"id":"ds","name":"DeepSeek"},` +
		`"st":{"id":"st","name":"SenseNova","base_url":"https://x.test/v1"},` +
		`"std":{"id":"std","name":"Std"},` +
		`"mygate":{"id":"mygate","name":"My Gate","base_url":"https://y.test/v1","protocol":"chat_completions","prefix":"/somewhere/else"},` +
		`"oc":{"id":"oc","name":"OpenCode Zen"},"ve":{"id":"ve","name":"Vibe"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	gateway, ok := cfg.Gateways["mygate"]
	if !ok {
		t.Fatalf("the custom gateway was discarded: %v", cfg.Gateways)
	}
	// Whatever the file claimed about protocol and prefix is overridden: the id
	// is the prefix and the protocol is the adapter it reuses.
	if gateway.Protocol != ProtocolResponses || gateway.Prefix != "/mygate" {
		t.Fatalf("custom identity was not pinned: %+v", gateway)
	}
	for _, legacy := range []string{"oc", "ve"} {
		if _, ok := cfg.Gateways[legacy]; ok {
			t.Errorf("legacy gateway %q was revived as a custom gateway", legacy)
		}
	}
	// The dropped entries are still on disk, so a rewrite is owed.
	if !cfg.ShouldPersist() {
		t.Error("a config still listing removed gateways should be rewritten")
	}

	// Round-tripping through Save leaves the custom gateway intact and current.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Gateways["mygate"]; !ok {
		t.Fatal("the custom gateway did not survive a save/reload round trip")
	}
	if reloaded.ShouldPersist() {
		t.Error("a config the build just wrote should not need rewriting")
	}
}

func TestValidateConfigRejectsBrokenCustomGateways(t *testing.T) {
	good := GatewayConfig{ID: "mygate", Prefix: "/mygate", Name: "My Gate", Protocol: ProtocolResponses}
	tests := []struct {
		name    string
		mutate  func(GatewayConfig) GatewayConfig
		wantErr string
	}{
		{name: "chat protocol", mutate: func(g GatewayConfig) GatewayConfig { g.Protocol = ProtocolChat; return g }, wantErr: "standard Responses adapter"},
		{name: "borrowed prefix", mutate: func(g GatewayConfig) GatewayConfig { g.Prefix = "/other"; return g }, wantErr: "must use prefix /mygate"},
		{name: "missing name", mutate: func(g GatewayConfig) GatewayConfig { g.Name = "  "; return g }, wantErr: "requires a name"},
		{name: "mismatched id", mutate: func(g GatewayConfig) GatewayConfig { g.ID = "elsewhere"; return g }, wantErr: "mismatched id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateways := map[string]GatewayConfig{"mygate": test.mutate(good)}
			err := ValidateConfig("127.0.0.1:8787", gateways)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

// Overlapping prefixes are deliberately legal: routing resolves the longest
// match, so neither gateway becomes ambiguous at request time.
func TestValidateConfigAllowsNestedPrefixes(t *testing.T) {
	gateways := map[string]GatewayConfig{
		"gw":    {ID: "gw", Prefix: "/gw", Name: "GW", Protocol: ProtocolResponses},
		"gw_v2": {ID: "gw_v2", Prefix: "/gw_v2", Name: "GW v2", Protocol: ProtocolResponses},
	}
	if err := ValidateConfig("127.0.0.1:8787", gateways); err != nil {
		t.Fatalf("sibling prefixes were rejected: %v", err)
	}
}
