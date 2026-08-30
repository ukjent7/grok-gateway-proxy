package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Protocol string

const (
	ProtocolResponses Protocol = "responses"
	ProtocolChat      Protocol = "chat_completions"
)

const DefaultUpstreamTimeout = 5 * time.Minute

// DefaultBodyCaptureLimitKB bounds the per-column body capture size for audit
// logging. Bodies larger than this are still forwarded to the client in full;
// only the stored copy is truncated. 256KB covers most LLM payloads while
// preventing a single large response from consuming disproportionate space.
const DefaultBodyCaptureLimitKB = 256

type GatewayConfig struct {
	ID                       string   `json:"id"`
	Prefix                   string   `json:"prefix"`
	Name                     string   `json:"name"`
	BaseURL                  string   `json:"base_url"`
	Protocol                 Protocol `json:"protocol"`
	Enabled                  bool     `json:"enabled"`
	ForwardHeaders           []string `json:"forward_headers,omitempty"`
	UserAgentOverrideEnabled bool     `json:"user_agent_override_enabled"`
	UserAgentOverride        string   `json:"user_agent_override,omitempty"`
	UseProxy                 bool     `json:"use_proxy"`
}

// UnmarshalJSON migrates the previous per-gateway proxy switch while keeping
// newly created gateways enabled by default.
func (g *GatewayConfig) UnmarshalJSON(data []byte) error {
	type gatewayConfig GatewayConfig
	var decoded gatewayConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, present := fields["use_proxy"]; !present {
		if legacy, ok := fields["use_system_proxy"]; ok {
			if err := json.Unmarshal(legacy, &decoded.UseProxy); err != nil {
				return err
			}
		} else {
			decoded.UseProxy = true
		}
	}
	*g = GatewayConfig(decoded)
	return nil
}

type Config struct {
	ListenAddr         string                   `json:"listen_addr"`
	proxyURL           string                   `json:"-"`
	UpstreamTimeout    time.Duration            `json:"-"`
	LogRetention       time.Duration            `json:"-"`
	BodyCaptureLimitKB int                      `json:"-"`
	ConfigPath         string                   `json:"-"`
	Gateways           map[string]GatewayConfig `json:"gateways"`
	mu                 sync.RWMutex
}

// DefaultGateways holds the fixed identity (prefix/protocol/name/base URL) for
// each supported gateway. Built once and reused for validation and defaults
// instead of allocating a fresh Config on every validation.
var DefaultGateways = BuildDefaultGateways()

func BuildDefaultGateways() map[string]GatewayConfig {
	return map[string]GatewayConfig{
		"ds": {
			ID:                "ds",
			Prefix:            "/ds",
			Name:              "DeepSeek",
			BaseURL:           "",
			Protocol:          ProtocolResponses,
			Enabled:           true,
			UserAgentOverride: "grok-gateway-proxy/dev",
			UseProxy:          true,
		},
		"st": {
			ID:                "st",
			Prefix:            "/st",
			Name:              "SenseNova",
			BaseURL:           "https://token.sensenova.cn/v1",
			Protocol:          ProtocolChat,
			Enabled:           true,
			UserAgentOverride: "grok-gateway-proxy/dev",
			UseProxy:          true,
		},
		"std": {
			ID:                "std",
			Prefix:            "/std",
			Name:              "标准 Responses",
			BaseURL:           "",
			Protocol:          ProtocolResponses,
			Enabled:           true,
			UserAgentOverride: "grok-gateway-proxy/dev",
			UseProxy:          true,
		},
	}
}

func DefaultConfig(path string) *Config {
	return &Config{
		ListenAddr:         "127.0.0.1:8787",
		UpstreamTimeout:    DefaultUpstreamTimeout,
		LogRetention:       7 * 24 * time.Hour,
		BodyCaptureLimitKB: DefaultBodyCaptureLimitKB,
		ConfigPath:         path,
		Gateways:           BuildDefaultGateways(),
	}
}

// LoadConfig loads configuration from path. logRetentionDays is the explicit
// retention (in days) supplied via the --log-retention-days flag or the
// GROK_PROXY_LOG_RETENTION_DAYS env var; it is nil when neither was provided.
//
// Precedence (highest to lowest): explicit flag/env > config file value >
// built-in default (7 days). Passing nil means "no explicit value", so the
// file value (or the default) applies.
func LoadConfig(path string, logRetentionDays *int) (*Config, error) {
	cfg := DefaultConfig(path)

	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if logRetentionDays != nil {
			if err := cfg.setLogRetention(*logRetentionDays); err != nil {
				return nil, fmt.Errorf("invalid log retention: %w", err)
			}
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	var disk struct {
		ListenAddr         string                   `json:"listen_addr"`
		ProxyURL           string                   `json:"proxy_url"`
		UpstreamTimeoutS   int                      `json:"upstream_timeout_seconds"`
		LogRetentionDays   *int                     `json:"log_retention_days"`
		BodyCaptureLimitKB *int                     `json:"body_capture_limit_kb"`
		Gateways           map[string]GatewayConfig `json:"gateways"`
	}
	if err := json.Unmarshal(b, &disk); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if disk.ListenAddr != "" {
		cfg.ListenAddr = disk.ListenAddr
	}
	cfg.proxyURL = strings.TrimSpace(disk.ProxyURL)
	if disk.UpstreamTimeoutS > 0 {
		cfg.UpstreamTimeout = time.Duration(disk.UpstreamTimeoutS) * time.Second
	}
	// Precedence: explicit flag/env (logRetentionDays) > file value > default.
	if disk.LogRetentionDays != nil {
		if err := cfg.setLogRetention(*disk.LogRetentionDays); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
	}
	if logRetentionDays != nil {
		if err := cfg.setLogRetention(*logRetentionDays); err != nil {
			return nil, fmt.Errorf("invalid log retention: %w", err)
		}
	}
	if disk.BodyCaptureLimitKB != nil {
		if err := cfg.setBodyCaptureLimit(*disk.BodyCaptureLimitKB); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
	}
	for id, gateway := range disk.Gateways {
		if defaultGateway, ok := DefaultGateways[id]; ok {
			gateway.Prefix = defaultGateway.Prefix
			gateway.Protocol = defaultGateway.Protocol
			if gateway.Name == "" {
				gateway.Name = defaultGateway.Name
			}
			if gateway.BaseURL == "" {
				gateway.BaseURL = defaultGateway.BaseURL
			}
			if gateway.UserAgentOverride == "" {
				gateway.UserAgentOverride = defaultGateway.UserAgentOverride
			}
			gateway.ID = id
			cfg.Gateways[id] = gateway
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// setLogRetention validates and applies a retention duration in days. Zero
// means "keep forever"; negative values are rejected.
func (c *Config) setLogRetention(days int) error {
	if days < 0 {
		return errors.New("log_retention_days must be >= 0")
	}
	c.LogRetention = time.Duration(days) * 24 * time.Hour
	return nil
}

// setBodyCaptureLimit validates and applies the per-column body capture limit
// in KB. Zero means "capture everything" (backward-compatible); negative
// values are rejected.
func (c *Config) setBodyCaptureLimit(kb int) error {
	if kb < 0 {
		return errors.New("body_capture_limit_kb must be >= 0")
	}
	c.BodyCaptureLimitKB = kb
	return nil
}

// Save persists the config to disk. It takes the write lock, not a read one:
// saveLocked serializes the in-memory config, and other goroutines mutate
// those fields under the same write lock, so a read lock would allow a torn
// snapshot (e.g. one gateway entry from before a concurrent edit).
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

// saveLocked persists the config to disk, writing to a uniquely named temp
// file and renaming it into place so readers never observe a partially
// written config. The caller must hold c.mu (write lock).
//
// The temp name is randomized rather than a fixed <path>.tmp: a fixed name is
// shared state, so a concurrent writer (another Save, or an external tool)
// could truncate it mid-write and leave the rename publishing someone else's
// bytes. A unique name also means a crashed save leaves behind garbage that
// the next save simply ignores, instead of a .tmp file the next save reuses.
func (c *Config) saveLocked() error {
	if err := c.validateLocked(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		ListenAddr             string                   `json:"listen_addr"`
		ProxyURL               string                   `json:"proxy_url,omitempty"`
		UpstreamTimeoutSeconds int                      `json:"upstream_timeout_seconds,omitempty"`
		LogRetentionDays       int                      `json:"log_retention_days"`
		BodyCaptureLimitKB     int                      `json:"body_capture_limit_kb"`
		Gateways               map[string]GatewayConfig `json:"gateways"`
	}{
		ListenAddr:             c.ListenAddr,
		ProxyURL:               c.proxyURL,
		UpstreamTimeoutSeconds: int(c.UpstreamTimeout / time.Second),
		LogRetentionDays:       int(c.LogRetention / (24 * time.Hour)),
		BodyCaptureLimitKB:     c.BodyCaptureLimitKB,
		Gateways:               c.Gateways,
	}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.ConfigPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// CreateTemp applies 0o600 to the file it creates.
	tmp, err := os.CreateTemp(dir, filepath.Base(c.ConfigPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, c.ConfigPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func ValidateConfig(listenAddr string, gateways map[string]GatewayConfig) error {
	if strings.TrimSpace(listenAddr) == "" {
		return errors.New("listen_addr is required")
	}
	for id, gateway := range gateways {
		expected, ok := DefaultGateways[id]
		if !ok {
			return fmt.Errorf("gateway %q is not supported", id)
		}
		if gateway.ID != id {
			return fmt.Errorf("gateway %q has mismatched id", id)
		}
		if gateway.Prefix != expected.Prefix {
			return fmt.Errorf("gateway %q must use prefix %s", id, expected.Prefix)
		}
		if gateway.Protocol != expected.Protocol {
			return fmt.Errorf("gateway %q must use protocol %q", id, expected.Protocol)
		}
		if gateway.UserAgentOverrideEnabled && strings.TrimSpace(gateway.UserAgentOverride) == "" {
			return fmt.Errorf("gateway %q user_agent_override is required when the override is enabled", id)
		}
		// Base URL may be empty: the gateway stays unusable until a base URL
		// is configured in the console, but the config itself is valid.
		if strings.TrimSpace(gateway.BaseURL) == "" {
			continue
		}
		u, err := url.Parse(gateway.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("gateway %q base_url must be an HTTPS URL", id)
		}
	}
	return nil
}

func (c *Config) validateLocked() error {
	if err := ValidateProxyURL(c.proxyURL); err != nil {
		return err
	}
	return ValidateConfig(c.ListenAddr, c.Gateways)
}

func ValidateProxyURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("proxy_url must be an HTTP or HTTPS URL")
	}
	return nil
}

func (c *Config) Validate() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.validateLocked()
}

func (c *Config) Snapshot() map[string]GatewayConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]GatewayConfig, len(c.Gateways))
	for id, gateway := range c.Gateways {
		gateway.ForwardHeaders = append([]string(nil), gateway.ForwardHeaders...)
		result[id] = gateway
	}
	return result
}

// GatewayPatch contains the mutable gateway fields that can be updated
// partially. Nil fields are left unchanged.
type GatewayPatch struct {
	Name                     *string   `json:"name"`
	BaseURL                  *string   `json:"base_url"`
	Enabled                  *bool     `json:"enabled"`
	ForwardHeaders           *[]string `json:"forward_headers"`
	UserAgentOverrideEnabled *bool     `json:"user_agent_override_enabled"`
	UserAgentOverride        *string   `json:"user_agent_override"`
	UseProxy                 *bool     `json:"use_proxy"`
	LegacyUseSystemProxy     *bool     `json:"use_system_proxy"`
}

// ErrUnknownGateway reports a gateway id that has no configuration. Callers
// match it with errors.Is to map the failure to a 404 instead of a string
// comparison on the message.
var ErrUnknownGateway = errors.New("unknown gateway")

// PatchGateway applies a partial update to a single gateway and persists the
// result. Unknown ids and invalid candidate values are rejected.
func (c *Config) PatchGateway(id string, patch GatewayPatch) (GatewayConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, ok := c.Gateways[id]
	if !ok {
		return GatewayConfig{}, fmt.Errorf("%w %q", ErrUnknownGateway, id)
	}
	gateway := current
	if patch.Name != nil {
		gateway.Name = *patch.Name
	}
	if patch.BaseURL != nil {
		gateway.BaseURL = *patch.BaseURL
	}
	if patch.Enabled != nil {
		gateway.Enabled = *patch.Enabled
	}
	if patch.ForwardHeaders != nil {
		gateway.ForwardHeaders = append([]string(nil), *patch.ForwardHeaders...)
	}
	if patch.UserAgentOverrideEnabled != nil {
		gateway.UserAgentOverrideEnabled = *patch.UserAgentOverrideEnabled
	}
	if patch.UserAgentOverride != nil {
		gateway.UserAgentOverride = *patch.UserAgentOverride
	}
	if patch.UseProxy != nil {
		gateway.UseProxy = *patch.UseProxy
	} else if patch.LegacyUseSystemProxy != nil {
		gateway.UseProxy = *patch.LegacyUseSystemProxy
	}

	updated := make(map[string]GatewayConfig, len(c.Gateways))
	for k, v := range c.Gateways {
		updated[k] = v
	}
	updated[id] = normalizeGateway(id, current, gateway)
	if err := c.commitLocked(updated); err != nil {
		return GatewayConfig{}, err
	}
	return updated[id], nil
}

// normalizeGateway pins the immutable identity fields (id/prefix/protocol) to
// the known gateway and falls back to the current name when the candidate
// omits it.
func normalizeGateway(id string, current, candidate GatewayConfig) GatewayConfig {
	candidate.ID = id
	candidate.Prefix = current.Prefix
	candidate.Protocol = current.Protocol
	if candidate.Name == "" {
		candidate.Name = current.Name
	}
	return candidate
}

// commitLocked validates and swaps in a candidate gateway map, persisting it.
// The caller must hold c.mu (write lock). On persistence failure the previous
// gateways are restored.
func (c *Config) commitLocked(updated map[string]GatewayConfig) error {
	if err := ValidateConfig(c.ListenAddr, updated); err != nil {
		return err
	}
	old := c.Gateways
	c.Gateways = updated
	if err := c.saveLocked(); err != nil {
		c.Gateways = old
		return err
	}
	return nil
}

// SetProxyURL atomically updates the global HTTP/HTTPS proxy address.
func (c *Config) SetProxyURL(proxyURL string) error {
	proxyURL = strings.TrimSpace(proxyURL)
	if err := ValidateProxyURL(proxyURL); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.proxyURL
	c.proxyURL = proxyURL
	if err := c.saveLocked(); err != nil {
		c.proxyURL = old
		return err
	}
	return nil
}

// ProxyURL returns the current global HTTP/HTTPS proxy URL. Reads must go
// through this accessor (rather than the proxyURL field directly) because the
// value is mutated by SetProxyURL under c.mu while the HTTP server may read it
// concurrently.
func (c *Config) ProxyURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.proxyURL
}

func (c *Config) GetUpstreamTimeout() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.UpstreamTimeout <= 0 {
		return DefaultUpstreamTimeout
	}
	return c.UpstreamTimeout
}

func (c *Config) GetLogRetention() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.LogRetention
}

func (c *Config) GetBodyCaptureLimitKB() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.BodyCaptureLimitKB < 0 {
		return DefaultBodyCaptureLimitKB
	}
	return c.BodyCaptureLimitKB
}
