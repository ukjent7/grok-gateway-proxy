package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestConfig(t *testing.T) *Config {
	t.Helper()
	return DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
}

// Save must be atomic enough to never leave a partially written config, and
// must not leave temp files behind for the next save to trip over.
func TestSaveRoundTripsAndLeavesNoTempFiles(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.ListenAddr = "127.0.0.1:9999"
	cfg.UpstreamTimeout = 45 * time.Second
	cfg.LogRetention = 72 * time.Hour
	cfg.BodyCaptureLimitKB = 256

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadConfig(cfg.ConfigPath, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if reloaded.ListenAddr != "127.0.0.1:9999" {
		t.Fatalf("ListenAddr = %q, want 127.0.0.1:9999", reloaded.ListenAddr)
	}
	if reloaded.GetUpstreamTimeout() != 45*time.Second {
		t.Fatalf("UpstreamTimeout = %v, want 45s", reloaded.GetUpstreamTimeout())
	}
	if reloaded.GetLogRetention() != 72*time.Hour {
		t.Fatalf("LogRetention = %v, want 72h", reloaded.GetLogRetention())
	}
	if reloaded.GetBodyCaptureLimitKB() != 256 {
		t.Fatalf("BodyCaptureLimitKB = %d, want 256", reloaded.GetBodyCaptureLimitKB())
	}

	entries, err := os.ReadDir(filepath.Dir(cfg.ConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "config.json" {
			t.Fatalf("Save left a stray file behind: %s", entry.Name())
		}
	}
}

func TestSaveRejectsInvalidConfig(t *testing.T) {
	cfg := newTestConfig(t)
	if err := cfg.Save(); err != nil {
		t.Fatalf("initial Save: %v", err)
	}
	before, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	cfg.ListenAddr = " "
	if err := cfg.Save(); err == nil {
		t.Fatal("expected Save to reject an empty listen_addr")
	}
	after, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a rejected Save modified the config file")
	}
}

// Snapshot hands out copies: a caller mutating what it received must not be
// able to reach back into the live configuration.
func TestSnapshotIsIndependentCopy(t *testing.T) {
	cfg := newTestConfig(t)
	gateway := cfg.Gateways["ds"]
	gateway.ForwardHeaders = []string{"X-One"}
	cfg.Gateways["ds"] = gateway

	snapshot := cfg.Snapshot()
	snapshot["ds"].ForwardHeaders[0] = "MUTATED"
	delete(snapshot, "st")

	if got := cfg.Snapshot()["ds"].ForwardHeaders[0]; got != "X-One" {
		t.Fatalf("mutating a snapshot leaked into the config: %q", got)
	}
	if _, ok := cfg.Snapshot()["st"]; !ok {
		t.Fatal("deleting from a snapshot leaked into the config")
	}
}

func TestPatchGatewayAppliesPartialUpdate(t *testing.T) {
	cfg := newTestConfig(t)
	name := "Renamed"
	enabled := false
	headers := []string{"X-Trace"}

	updated, err := cfg.PatchGateway("ds", GatewayPatch{
		Name:           &name,
		Enabled:        &enabled,
		ForwardHeaders: &headers,
	})
	if err != nil {
		t.Fatalf("PatchGateway: %v", err)
	}
	if updated.Name != "Renamed" || updated.Enabled {
		t.Fatalf("unexpected patched gateway: %+v", updated)
	}
	if len(updated.ForwardHeaders) != 1 || updated.ForwardHeaders[0] != "X-Trace" {
		t.Fatalf("ForwardHeaders = %v, want [X-Trace]", updated.ForwardHeaders)
	}
	// Keys not mentioned in the patch keep their previous value.
	if updated.BaseURL != cfg.Gateways["ds"].BaseURL {
		t.Fatalf("BaseURL changed without being patched: %q", updated.BaseURL)
	}
	if cfg.Gateways["ds"].Name != "Renamed" {
		t.Fatal("PatchGateway did not persist to the live config")
	}
}

// Identity fields are immutable: a patch must not be able to move a gateway
// to a different prefix or protocol.
func TestPatchGatewayPinsImmutableIdentity(t *testing.T) {
	cfg := newTestConfig(t)
	before := cfg.Gateways["st"]

	if _, err := cfg.PatchGateway("st", GatewayPatch{}); err != nil {
		t.Fatalf("PatchGateway: %v", err)
	}
	after := cfg.Gateways["st"]
	if after.ID != "st" || after.Prefix != before.Prefix || after.Protocol != before.Protocol {
		t.Fatalf("identity changed: %+v -> %+v", before, after)
	}
}

// An empty name means "leave it alone" rather than "blank it out".
func TestPatchGatewayKeepsNameWhenPatchedEmpty(t *testing.T) {
	cfg := newTestConfig(t)
	empty := ""
	updated, err := cfg.PatchGateway("ds", GatewayPatch{Name: &empty})
	if err != nil {
		t.Fatalf("PatchGateway: %v", err)
	}
	if updated.Name != DefaultGateways["ds"].Name {
		t.Fatalf("Name = %q, want the previous %q", updated.Name, DefaultGateways["ds"].Name)
	}
}

func TestPatchGatewayUnknownID(t *testing.T) {
	cfg := newTestConfig(t)
	if _, err := cfg.PatchGateway("nope", GatewayPatch{}); err == nil {
		t.Fatal("expected PatchGateway to reject an unknown gateway id")
	}
}

// A rejected patch must roll the whole gateway map back, not leave a
// partially applied one behind.
func TestPatchGatewayRejectsInvalidCandidateAndRollsBack(t *testing.T) {
	cfg := newTestConfig(t)
	before := cfg.Gateways["ds"]

	enabled := true
	if _, err := cfg.PatchGateway("ds", GatewayPatch{
		UserAgentOverrideEnabled: &enabled,
		UserAgentOverride:        new(string), // empty override
	}); err == nil {
		t.Fatal("expected PatchGateway to reject an enabled but empty user agent override")
	}
	after := cfg.Gateways["ds"]
	if after.UserAgentOverrideEnabled != before.UserAgentOverrideEnabled ||
		after.Name != before.Name {
		t.Fatalf("a rejected patch left partial state: %+v -> %+v", before, after)
	}
}

// The legacy per-gateway switch is still accepted by older clients.
func TestPatchGatewayAcceptsLegacyUseSystemProxy(t *testing.T) {
	cfg := newTestConfig(t)
	disabled := false
	updated, err := cfg.PatchGateway("ds", GatewayPatch{LegacyUseSystemProxy: &disabled})
	if err != nil {
		t.Fatalf("PatchGateway: %v", err)
	}
	if updated.UseProxy {
		t.Fatal("expected the legacy use_system_proxy patch to disable the proxy")
	}
}

func TestSetProxyURL(t *testing.T) {
	cfg := newTestConfig(t)
	if err := cfg.SetProxyURL("  http://127.0.0.1:7890  "); err != nil {
		t.Fatalf("SetProxyURL: %v", err)
	}
	if got := cfg.ProxyURL(); got != "http://127.0.0.1:7890" {
		t.Fatalf("ProxyURL() = %q, want http://127.0.0.1:7890", got)
	}

	// Clearing is allowed.
	if err := cfg.SetProxyURL(""); err != nil {
		t.Fatalf("SetProxyURL(\"\"): %v", err)
	}
	if cfg.ProxyURL() != "" {
		t.Fatalf("ProxyURL() = %q, want empty", cfg.ProxyURL())
	}

	// An invalid value must be rejected without clobbering the current one.
	if err := cfg.SetProxyURL("http://127.0.0.1:7890"); err != nil {
		t.Fatalf("SetProxyURL: %v", err)
	}
	if err := cfg.SetProxyURL("not-a-url"); err == nil {
		t.Fatal("expected SetProxyURL to reject a non-URL value")
	}
	if got := cfg.ProxyURL(); got != "http://127.0.0.1:7890" {
		t.Fatalf("a rejected SetProxyURL clobbered the value: %q", got)
	}
}

func TestValidateProxyURL(t *testing.T) {
	for _, raw := range []string{"", "   ", "http://host:8080", "https://host"} {
		if err := ValidateProxyURL(raw); err != nil {
			t.Fatalf("ValidateProxyURL(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"host:8080", "ftp://host", "://missing-host"} {
		if err := ValidateProxyURL(raw); err == nil {
			t.Fatalf("ValidateProxyURL(%q) = nil, want an error", raw)
		}
	}
}

// Zero and negative values are what a hand-edited or truncated config file
// produces, so the getters must substitute the defaults rather than hand back
// a nonsensical duration or limit.
func TestGettersSubstituteDefaultsForUnsetValues(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.UpstreamTimeout = 0
	if got := cfg.GetUpstreamTimeout(); got != DefaultUpstreamTimeout {
		t.Fatalf("GetUpstreamTimeout() = %v, want %v", got, DefaultUpstreamTimeout)
	}
	cfg.BodyCaptureLimitKB = -1
	if got := cfg.GetBodyCaptureLimitKB(); got != DefaultBodyCaptureLimitKB {
		t.Fatalf("GetBodyCaptureLimitKB() = %d, want %d", got, DefaultBodyCaptureLimitKB)
	}
	// Zero is a meaningful value here ("capture everything"), not "unset".
	cfg.BodyCaptureLimitKB = 0
	if got := cfg.GetBodyCaptureLimitKB(); got != 0 {
		t.Fatalf("GetBodyCaptureLimitKB() = %d, want 0", got)
	}
}

// Gateways written by older builds predate the global proxy switch and must
// keep working without a migration step.
func TestGatewayConfigUnmarshalJSONMigratesLegacyProxySwitch(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantUse  bool
		wantSkip bool
	}{
		{name: "legacy use_system_proxy honoured", body: `{"use_system_proxy":false}`, wantUse: false},
		{name: "legacy use_system_proxy true", body: `{"use_system_proxy":true}`, wantUse: true},
		{name: "new field wins over legacy", body: `{"use_proxy":false,"use_system_proxy":true}`, wantUse: false},
		{name: "absent defaults to enabled", body: `{}`, wantUse: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gateway GatewayConfig
			if err := json.Unmarshal([]byte(test.body), &gateway); err != nil {
				t.Fatal(err)
			}
			if gateway.UseProxy != test.wantUse {
				t.Fatalf("UseProxy = %v, want %v", gateway.UseProxy, test.wantUse)
			}
		})
	}
}

// The saved file is the source of truth on the next boot, so the on-disk
// shape must stay stable: renaming a field silently discards user settings.
func TestSavedConfigKeepsExpectedFieldNames(t *testing.T) {
	cfg := newTestConfig(t)
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"listen_addr", "log_retention_days", "body_capture_limit_kb", "gateways"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("saved config is missing %q: %s", key, raw)
		}
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Fatal("saved config should end with a newline")
	}
}

// ValidateConfig is the gate for hand-edited config files, so every way a
// gateway entry can disagree with its known identity must be rejected.
func TestValidateConfigRejectsDivergentGateway(t *testing.T) {
	base := DefaultGateways
	tests := []struct {
		name    string
		mutate  func(gateway GatewayConfig) GatewayConfig
		wantErr string
	}{
		{
			name:    "mismatched id",
			mutate:  func(g GatewayConfig) GatewayConfig { g.ID = "other"; return g },
			wantErr: "mismatched id",
		},
		{
			name:    "wrong prefix",
			mutate:  func(g GatewayConfig) GatewayConfig { g.Prefix = "/elsewhere"; return g },
			wantErr: "must use prefix",
		},
		{
			name:    "wrong protocol",
			mutate:  func(g GatewayConfig) GatewayConfig { g.Protocol = ProtocolChat; return g },
			wantErr: "must use protocol",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateways := map[string]GatewayConfig{"ds": test.mutate(base["ds"])}
			err := ValidateConfig("127.0.0.1:8787", gateways)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateConfigRejectsUnknownGatewayAndEmptyListenAddr(t *testing.T) {
	gateways := map[string]GatewayConfig{"mystery": {ID: "mystery"}}
	if err := ValidateConfig("127.0.0.1:8787", gateways); err == nil {
		t.Fatal("expected an unsupported gateway to be rejected")
	}
	if err := ValidateConfig("  ", BuildDefaultGateways()); err == nil {
		t.Fatal("expected an empty listen_addr to be rejected")
	}
}

// A gateway cannot be saved with the override switched on but empty: the
// proxy would send a blank User-Agent upstream.
func TestValidateConfigRejectsEmptyUserAgentOverride(t *testing.T) {
	gateways := BuildDefaultGateways()
	gateway := gateways["ds"]
	gateway.UserAgentOverrideEnabled = true
	gateway.UserAgentOverride = "  "
	gateways["ds"] = gateway
	if err := ValidateConfig("127.0.0.1:8787", gateways); err == nil {
		t.Fatal("expected an enabled-but-empty user agent override to be rejected")
	}
}

// Save surfaces I/O failures instead of silently discarding the config.
func TestSaveReportsWriteFailure(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	// A path whose parent is an existing file cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = filepath.Join(blocker, "config.json")
	if err := cfg.Save(); err == nil {
		t.Fatal("expected Save to fail when the target directory cannot be created")
	}
}

func TestGatewayConfigUnmarshalJSONRejectsBadLegacyValue(t *testing.T) {
	var gateway GatewayConfig
	if err := json.Unmarshal([]byte(`{"use_system_proxy":"yes"}`), &gateway); err == nil {
		t.Fatal("expected a non-boolean use_system_proxy to be rejected")
	}
}
