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
)

type Protocol string

const (
	ProtocolResponses Protocol = "responses"
	ProtocolChat      Protocol = "chat_completions"
)

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
	ListenAddr string                   `json:"listen_addr"`
	APIToken   string                   `json:"api_token,omitempty"`
	ConfigPath string                   `json:"-"`
	Gateways   map[string]GatewayConfig `json:"gateways"`
	mu         sync.RWMutex
}

func DefaultConfig(path string) *Config {
	return &Config{
		ListenAddr: "127.0.0.1:8787",
		ConfigPath: path,
		Gateways: map[string]GatewayConfig{
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
		},
	}
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig(path)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	var disk struct {
		ListenAddr string                   `json:"listen_addr"`
		APIToken   string                   `json:"api_token"`
		Gateways   map[string]GatewayConfig `json:"gateways"`
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
	for id, gateway := range disk.Gateways {
		if defaultGateway, ok := cfg.Gateways[id]; ok {
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
	if disk.LegacyUserAgentOverrideEnabled {
		value := strings.TrimSpace(disk.LegacyUserAgentOverride)
		if value == "" {
			value = "grok-gateway-proxy/dev"
		}
		for id, gateway := range cfg.Gateways {
			gateway.UserAgentOverrideEnabled = true
			gateway.UserAgentOverride = value
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
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		ListenAddr string                   `json:"listen_addr"`
		APIToken   string                   `json:"api_token,omitempty"`
		Gateways   map[string]GatewayConfig `json:"gateways"`
	}{c.ListenAddr, c.APIToken, c.Gateways}, "", "  ")
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

func (c *Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("listen_addr is required")
	}
	for id, gateway := range c.Gateways {
		expected, ok := DefaultConfig("").Gateways[id]
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

func (c *Config) UpdateGateways(gateways map[string]GatewayConfig) error {
	currentGateways := c.Snapshot()
	updated := make(map[string]GatewayConfig, len(currentGateways))
	for id, current := range currentGateways {
		candidate, ok := gateways[id]
		if !ok {
			candidate = current
		}
		candidate.ID = id
		candidate.Prefix = current.Prefix
		candidate.Protocol = current.Protocol
		if candidate.Name == "" {
			candidate.Name = current.Name
		}
		updated[id] = candidate
	}
	c.mu.Lock()
	old := c.Gateways
	c.Gateways = updated
	validationErr := c.Validate()
	c.mu.Unlock()
	if validationErr != nil {
		c.mu.Lock()
		c.Gateways = old
		c.mu.Unlock()
		return validationErr
	}
	if err := c.Save(); err != nil {
		c.mu.Lock()
		c.Gateways = old
		c.mu.Unlock()
		return err
	}
	return nil
}
