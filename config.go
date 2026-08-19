package main

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

const defaultUpstreamTimeout = 5 * time.Minute

type RetryConfig struct {
	Retries    int           `json:"retries"`
	RetryDelay time.Duration `json:"retry_delay"`
	Backoff    bool          `json:"backoff"`
	MaxBackoff time.Duration `json:"max_backoff"`
}

// DefaultRetryConfig is used when the config file has no retry section.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{Retries: 2, RetryDelay: 500 * time.Millisecond, Backoff: true, MaxBackoff: 8 * time.Second}
}

// retryFor reports whether a response status is one worth retrying. It is
// available for callers (e.g. a future client-side retry loop) but the proxy
// itself does not currently auto-retry upstream calls.
func (r RetryConfig) retryFor(status int) bool {
	if r.Retries <= 0 {
		return false
	}
	switch status {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

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
}

type Config struct {
	ListenAddr      string                   `json:"listen_addr"`
	APIToken        string                   `json:"api_token,omitempty"`
	UpstreamTimeout time.Duration            `json:"-"`
	Retry           RetryConfig              `json:"retry"`
	LogRetention    time.Duration            `json:"-"`
	ConfigPath      string                   `json:"-"`
	Gateways        map[string]GatewayConfig `json:"gateways"`
	mu              sync.RWMutex
}

// defaultGateways holds the fixed identity (prefix/protocol/name/base URL) for
// each supported gateway. Built once and reused for validation and defaults
// instead of allocating a fresh Config on every validation.
var defaultGateways = buildDefaultGateways()

func buildDefaultGateways() map[string]GatewayConfig {
	return map[string]GatewayConfig{
		"oc": {
			ID:                "oc",
			Prefix:            "/oc",
			Name:              "OpenCode Zen",
			BaseURL:           "https://opencode.ai/zen/go/v1",
			Protocol:          ProtocolResponses,
			Enabled:           true,
			UserAgentOverride: "grok-gateway-proxy/dev",
		},
		"st": {
			ID:                "st",
			Prefix:            "/st",
			Name:              "SenseNova",
			BaseURL:           "https://token.sensenova.cn/v1",
			Protocol:          ProtocolChat,
			Enabled:           true,
			UserAgentOverride: "grok-gateway-proxy/dev",
		},
		"ve": {
			ID:                "ve",
			Prefix:            "/ve",
			Name:              "Vercel AI Gateway",
			BaseURL:           "https://ai-gateway.vercel.sh/v1",
			Protocol:          ProtocolResponses,
			Enabled:           true,
			UserAgentOverride: "grok-gateway-proxy/dev",
		},
	}
}

func DefaultConfig(path string) *Config {
	return &Config{
		ListenAddr:      "127.0.0.1:8787",
		UpstreamTimeout: defaultUpstreamTimeout,
		Retry:           DefaultRetryConfig(),
		LogRetention:    30 * 24 * time.Hour,
		ConfigPath:      path,
		Gateways:        buildDefaultGateways(),
	}
}

func LoadConfig(path string, logRetentionDays int) (*Config, error) {
	cfg := DefaultConfig(path)
	cfg.LogRetention = time.Duration(logRetentionDays) * 24 * time.Hour

	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	var disk struct {
		ListenAddr       string                   `json:"listen_addr"`
		APIToken         string                   `json:"api_token"`
		UpstreamTimeoutS int                      `json:"upstream_timeout_seconds"`
		Retry            *RetryConfig             `json:"retry"`
		LogRetentionDays *int                     `json:"log_retention_days"`
		Gateways         map[string]GatewayConfig `json:"gateways"`
		// These fields are read only to migrate the previous global setting.
		LegacyUserAgentOverrideEnabled bool   `json:"user_agent_override_enabled"`
		LegacyUserAgentOverride        string `json:"user_agent_override"`
	}
	if err := json.Unmarshal(b, &disk); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if disk.ListenAddr != "" {
		cfg.ListenAddr = disk.ListenAddr
	}
	cfg.APIToken = disk.APIToken
	if disk.UpstreamTimeoutS > 0 {
		cfg.UpstreamTimeout = time.Duration(disk.UpstreamTimeoutS) * time.Second
	}
	if disk.LogRetentionDays != nil {
		if *disk.LogRetentionDays < 0 {
			return nil, errors.New("invalid config: log_retention_days must be >= 0")
		}
		cfg.LogRetention = time.Duration(*disk.LogRetentionDays) * 24 * time.Hour
	}
	if disk.Retry != nil {
		if disk.Retry.Retries < 0 {
			return nil, fmt.Errorf("invalid config: retry.retries must be >= 0")
		}
		if disk.Retry.RetryDelay < 0 || disk.Retry.MaxBackoff < 0 {
			return nil, errors.New("invalid config: retry delays must be non-negative")
		}
		if disk.Retry.MaxBackoff > 0 && disk.Retry.RetryDelay > disk.Retry.MaxBackoff {
			return nil, errors.New("invalid config: retry.retry_delay cannot exceed retry.max_backoff")
		}
		cfg.Retry = *disk.Retry
	}
	for id, gateway := range disk.Gateways {
		if defaultGateway, ok := defaultGateways[id]; ok {
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

func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.saveLocked()
}

// saveLocked persists the config to disk. The caller must hold at least a read
// lock on c.mu.
func (c *Config) saveLocked() error {
	if err := c.validateLocked(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		ListenAddr             string                   `json:"listen_addr"`
		APIToken               string                   `json:"api_token,omitempty"`
		UpstreamTimeoutSeconds int                      `json:"upstream_timeout_seconds,omitempty"`
		Retry                  *RetryConfig             `json:"retry,omitempty"`
		LogRetentionDays       int                      `json:"log_retention_days"`
		Gateways               map[string]GatewayConfig `json:"gateways"`
	}{
		ListenAddr:             c.ListenAddr,
		APIToken:               c.APIToken,
		UpstreamTimeoutSeconds: int(c.UpstreamTimeout / time.Second),
		Retry:                  &c.Retry,
		LogRetentionDays:       int(c.LogRetention / (24 * time.Hour)),
		Gateways:               c.Gateways,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.ConfigPath), 0o700); err != nil {
		return err
	}
	tmp := c.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.ConfigPath)
}

func validateConfig(listenAddr string, gateways map[string]GatewayConfig) error {
	if strings.TrimSpace(listenAddr) == "" {
		return errors.New("listen_addr is required")
	}
	for id, gateway := range gateways {
		expected, ok := defaultGateways[id]
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
		u, err := url.Parse(gateway.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("gateway %q base_url must be an HTTPS URL", id)
		}
	}
	return nil
}

func (c *Config) validateLocked() error {
	return validateConfig(c.ListenAddr, c.Gateways)
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

// UpdateGateways replaces gateways from the provided map (any gateway not
// present keeps its current value) and persists the result. The whole
// read-modify-validate-write cycle runs under a single write lock so
// concurrent updates cannot race or clobber each other.
func (c *Config) UpdateGateways(gateways map[string]GatewayConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	updated := make(map[string]GatewayConfig, len(c.Gateways))
	for id, current := range c.Gateways {
		candidate, ok := gateways[id]
		if !ok {
			candidate = current
		}
		updated[id] = normalizeGateway(id, current, candidate)
	}
	return c.commitLocked(updated)
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
}

// PatchGateway applies a partial update to a single gateway and persists the
// result. Unknown ids and invalid candidate values are rejected.
func (c *Config) PatchGateway(id string, patch GatewayPatch) (GatewayConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, ok := c.Gateways[id]
	if !ok {
		return GatewayConfig{}, fmt.Errorf("unknown gateway %q", id)
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

// UpdateGateway updates a single gateway (by id) and persists the result.
// Unknown ids are rejected.
func (c *Config) UpdateGateway(id string, gateway GatewayConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, ok := c.Gateways[id]
	if !ok {
		return fmt.Errorf("unknown gateway %q", id)
	}
	updated := make(map[string]GatewayConfig, len(c.Gateways))
	for k, v := range c.Gateways {
		updated[k] = v
	}
	updated[id] = normalizeGateway(id, current, gateway)
	return c.commitLocked(updated)
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
	if err := validateConfig(c.ListenAddr, updated); err != nil {
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

// SetListenAddr atomically updates the listen address and persists it.
func (c *Config) SetListenAddr(addr string) error {
	if err := validateConfig(addr, nil); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.ListenAddr
	c.ListenAddr = addr
	if err := c.saveLocked(); err != nil {
		c.ListenAddr = old
		return err
	}
	return nil
}
