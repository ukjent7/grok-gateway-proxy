package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type Protocol string

const (
	ProtocolResponses        Protocol = "responses"
	ProtocolChat             Protocol = "chat_completions"
	ProtocolOpenAICompatible Protocol = "openai_compatible"
	ProtocolAnthropic        Protocol = "anthropic"
)

const DefaultUpstreamTimeout = 5 * time.Minute
const DefaultBodyCaptureLimitKB = 256

type GatewayConfig struct {
	ID                       string   `json:"id"`
	Prefix                   string   `json:"prefix"`
	Name                     string   `json:"name"`
	BaseURL                  string   `json:"base_url"`
	Protocol                 Protocol `json:"protocol"`
	Enabled                  bool     `json:"enabled"`
	ForwardHeaders           []string `json:"forward_headers,omitempty"`
	SessionAffinity          string   `json:"session_affinity,omitempty"`
	UserAgentOverrideEnabled bool     `json:"user_agent_override_enabled"`
	UserAgentOverride        string   `json:"user_agent_override,omitempty"`
	UseProxy                 bool     `json:"use_proxy"`
}

const (
	SessionAffinityOpenAI     = "openai"
	SessionAffinityOpenRouter = "openrouter"
	SessionAffinityOpenCode   = "opencode"
	SessionAffinityOff        = "off"
)

var SessionAffinityModes = []string{SessionAffinityOpenAI, SessionAffinityOpenRouter, SessionAffinityOpenCode, SessionAffinityOff}

func (g GatewayConfig) EffectiveSessionAffinity() string {
	if slices.Contains(SessionAffinityModes, g.SessionAffinity) {
		return g.SessionAffinity
	}
	return SessionAffinityOpenAI
}

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
	listenAddr         string
	proxyURL           string                   `json:"-"`
	upstreamTimeout    time.Duration            `json:"-"`
	logRetention       time.Duration            `json:"-"`
	bodyCaptureLimitKB int                      `json:"-"`
	configPath         string                   `json:"-"`
	Gateways           map[string]GatewayConfig `json:"gateways"`
	persist            bool
	mu                 sync.RWMutex
}

func (c *Config) ShouldPersist() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.persist
}

var DefaultUserAgentOverride = "grok-gateway-proxy/dev"

func SetProductVersion(version string) {
	version = strings.TrimSpace(version)
	if version == "" {
		return
	}
	DefaultUserAgentOverride = "grok-gateway-proxy/" + version

	DefaultGateways = BuildDefaultGateways()
}

var customGatewayIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var headerNamePattern = regexp.MustCompile(`^[!#$%&'*+\-.^_|~0-9A-Za-z]+$`)

func CustomGatewayIDPattern() string { return customGatewayIDPattern.String() }

func ReservedPrefixes() []string {
	out := make([]string, 0, len(reservedPrefixes))
	for _, prefix := range reservedPrefixes {
		out = append(out, strings.TrimPrefix(prefix, "/"))
	}
	return out
}

var reservedPrefixes = []string{"/api", "/static", "/healthz", "/ui"}
var legacyGatewayIDs = map[string]bool{"oc": true, "ve": true}
var DefaultGateways = BuildDefaultGateways()

func IsBuiltinGateway(id string) bool {
	_, ok := DefaultGateways[id]
	return ok
}

func BuildDefaultGateways() map[string]GatewayConfig {
	return map[string]GatewayConfig{
		"ds": {
			ID:                "ds",
			Prefix:            "/ds",
			Name:              "DeepSeek",
			BaseURL:           "",
			Protocol:          ProtocolResponses,
			Enabled:           true,
			SessionAffinity:   SessionAffinityOpenAI,
			UserAgentOverride: DefaultUserAgentOverride,
			UseProxy:          true,
		},
		"st": {
			ID:                "st",
			Prefix:            "/st",
			Name:              "SenseNova",
			BaseURL:           "https://token.sensenova.cn/v1",
			Protocol:          ProtocolChat,
			Enabled:           true,
			SessionAffinity:   SessionAffinityOpenAI,
			UserAgentOverride: DefaultUserAgentOverride,
			UseProxy:          true,
		},
		"std": {
			ID:                "std",
			Prefix:            "/std",
			Name:              "标准 Responses",
			BaseURL:           "",
			Protocol:          ProtocolResponses,
			Enabled:           true,
			SessionAffinity:   SessionAffinityOpenAI,
			UserAgentOverride: DefaultUserAgentOverride,
			UseProxy:          true,
		},
		"oaic": {
			ID:                "oaic",
			Prefix:            "/oaic",
			Name:              "OpenAI Compatible",
			BaseURL:           "",
			Protocol:          ProtocolOpenAICompatible,
			Enabled:           true,
			SessionAffinity:   SessionAffinityOpenAI,
			UserAgentOverride: DefaultUserAgentOverride,
			UseProxy:          true,
		},
		"anth": {
			ID:                "anth",
			Prefix:            "/anth",
			Name:              "Anthropic",
			BaseURL:           "",
			Protocol:          ProtocolAnthropic,
			Enabled:           true,
			SessionAffinity:   SessionAffinityOpenAI,
			UserAgentOverride: DefaultUserAgentOverride,
			UseProxy:          true,
		},
	}
}

func DefaultConfig(path string) *Config {
	return &Config{
		listenAddr:         "127.0.0.1:8787",
		upstreamTimeout:    DefaultUpstreamTimeout,
		logRetention:       7 * 24 * time.Hour,
		bodyCaptureLimitKB: DefaultBodyCaptureLimitKB,
		configPath:         path,
		Gateways:           BuildDefaultGateways(),
	}
}

func LoadConfig(path string, logRetentionDays *int) (*Config, error) {
	cfg := DefaultConfig(path)

	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg.persist = true
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

	var rawGateways struct {
		Gateways map[string]json.RawMessage `json:"gateways"`
	}
	if err := json.Unmarshal(b, &rawGateways); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if disk.ListenAddr != "" {
		cfg.listenAddr = disk.ListenAddr
	}
	cfg.proxyURL = strings.TrimSpace(disk.ProxyURL)
	if disk.UpstreamTimeoutS > 0 {
		cfg.upstreamTimeout = time.Duration(disk.UpstreamTimeoutS) * time.Second
	}
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
			var fields map[string]json.RawMessage
			if raw, ok := rawGateways.Gateways[id]; ok {
				if err := json.Unmarshal(raw, &fields); err != nil {
					fields = nil
				}
			}
			gateway.Prefix = defaultGateway.Prefix
			gateway.Protocol = defaultGateway.Protocol
			if gateway.Name == "" {
				gateway.Name = defaultGateway.Name
			}
			if _, present := fields["base_url"]; !present && gateway.BaseURL == "" {
				gateway.BaseURL = defaultGateway.BaseURL
			}
			if gateway.UserAgentOverride == "" || gateway.UserAgentOverride == "grok-gateway-proxy/dev" {
				gateway.UserAgentOverride = defaultGateway.UserAgentOverride
			}
			gateway.ID = id
			cfg.Gateways[id] = gateway
			continue
		}
		gateway.ID = id
		gateway.Prefix = "/" + id
		gateway.Protocol = ProtocolResponses
		if gateway.Name == "" {
			gateway.Name = id
		}
		if gateway.UserAgentOverride == "" || gateway.UserAgentOverride == "grok-gateway-proxy/dev" {
			gateway.UserAgentOverride = DefaultUserAgentOverride
		}
		if validateCustomGateway(id, gateway) != nil {
			cfg.persist = true
			continue
		}
		cfg.Gateways[id] = gateway
	}
	for id, gateway := range cfg.Gateways {
		if strings.TrimSpace(gateway.SessionAffinity) == "" {
			gateway.SessionAffinity = SessionAffinityOpenAI
			cfg.Gateways[id] = gateway
		}
	}
	for id := range DefaultGateways {
		if _, ok := disk.Gateways[id]; !ok {
			cfg.persist = true
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) setLogRetention(days int) error {
	if days < 0 {
		return errors.New("log_retention_days must be >= 0")
	}
	c.logRetention = time.Duration(days) * 24 * time.Hour
	return nil
}

func (c *Config) setBodyCaptureLimit(kb int) error {
	if kb < 0 {
		return errors.New("body_capture_limit_kb must be >= 0")
	}
	c.bodyCaptureLimitKB = kb
	return nil
}

func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

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
		ListenAddr:             c.listenAddr,
		ProxyURL:               c.proxyURL,
		UpstreamTimeoutSeconds: int(c.upstreamTimeout / time.Second),
		LogRetentionDays:       int(c.logRetention / (24 * time.Hour)),
		BodyCaptureLimitKB:     c.bodyCaptureLimitKB,
		Gateways:               c.Gateways,
	}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(c.configPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, c.configPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func ValidateConfig(listenAddr string, gateways map[string]GatewayConfig) error {
	if strings.TrimSpace(listenAddr) == "" {
		return errors.New("listen_addr is required")
	}
	owners := make(map[string]string, len(gateways))
	for id, gateway := range gateways {
		if IsBuiltinGateway(id) {
			if err := validateBuiltinGateway(id, gateway); err != nil {
				return err
			}
		} else if err := validateCustomGateway(id, gateway); err != nil {
			return err
		}
		if gateway.UserAgentOverrideEnabled && strings.TrimSpace(gateway.UserAgentOverride) == "" {
			return fmt.Errorf("gateway %q user_agent_override is required when the override is enabled", id)
		}

		for _, name := range gateway.ForwardHeaders {
			if !headerNamePattern.MatchString(strings.TrimSpace(name)) {
				return fmt.Errorf("gateway %q forward_headers entry %q is not a valid HTTP header name", id, name)
			}
		}
		if affinity := strings.TrimSpace(gateway.SessionAffinity); affinity != "" && !slices.Contains(SessionAffinityModes, affinity) {
			return fmt.Errorf("gateway %q session_affinity %q must be one of %s", id, affinity, strings.Join(SessionAffinityModes, ", "))
		}

		if raw := strings.TrimSpace(gateway.BaseURL); raw != "" {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("gateway %q base_url must be an HTTPS URL", id)
			}
		}
		for _, reserved := range reservedPrefixes {
			if gateway.Prefix == reserved || strings.HasPrefix(gateway.Prefix, reserved+"/") {
				return fmt.Errorf("gateway %q prefix %s is reserved by the console", id, gateway.Prefix)
			}
		}
		if other, dup := owners[gateway.Prefix]; dup {
			return fmt.Errorf("gateways %q and %q share prefix %s", other, id, gateway.Prefix)
		}
		owners[gateway.Prefix] = id
	}
	return nil
}

func validateBuiltinGateway(id string, gateway GatewayConfig) error {
	expected := DefaultGateways[id]
	if gateway.ID != id {
		return fmt.Errorf("gateway %q has mismatched id", id)
	}
	if gateway.Prefix != expected.Prefix {
		return fmt.Errorf("gateway %q must use prefix %s", id, expected.Prefix)
	}
	if gateway.Protocol != expected.Protocol {
		return fmt.Errorf("gateway %q must use protocol %q", id, expected.Protocol)
	}
	return nil
}

func validateCustomGateway(id string, gateway GatewayConfig) error {
	if !customGatewayIDPattern.MatchString(id) {
		return fmt.Errorf("gateway id %q must match %s", id, customGatewayIDPattern.String())
	}
	if legacyGatewayIDs[id] {
		return fmt.Errorf("gateway id %q was used by a gateway this build removed", id)
	}
	if gateway.ID != id {
		return fmt.Errorf("gateway %q has mismatched id", id)
	}
	if gateway.Prefix != "/"+id {
		return fmt.Errorf("gateway %q must use prefix /%s", id, id)
	}
	if gateway.Protocol != ProtocolResponses {
		return fmt.Errorf("gateway %q must use protocol %q: custom gateways reuse the standard Responses adapter", id, ProtocolResponses)
	}
	if strings.TrimSpace(gateway.Name) == "" {
		return fmt.Errorf("gateway %q requires a name", id)
	}
	return nil
}

func ModelKey(name, id string) string {
	for _, candidate := range []string{name, id, "model"} {
		if key := slug(candidate); key != "" {
			return key + "-model"
		}
	}
	return "model-model"
}

func slug(s string) string {
	var out strings.Builder
	pendingHyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if pendingHyphen && out.Len() > 0 {
				out.WriteByte('-')
			}
			pendingHyphen = false
			out.WriteRune(r)
		default:
			if out.Len() > 0 {
				pendingHyphen = true
			}
		}
	}
	return out.String()
}

func ValidateGatewayModelKeys(gateways map[string]GatewayConfig) error {
	owners := make(map[string]string, len(gateways))
	for _, id := range slices.Sorted(maps.Keys(gateways)) {
		key := ModelKey(gateways[id].Name, id)
		if other, dup := owners[key]; dup {
			return fmt.Errorf("gateways %q and %q both map to the client model key %q; rename one", other, id, key)
		}
		owners[key] = id
	}
	return nil
}

func (c *Config) validateLocked() error {
	if err := ValidateProxyURL(c.proxyURL); err != nil {
		return err
	}
	return ValidateConfig(c.listenAddr, c.Gateways)
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

func (c *Config) MatchGateway(path string) (GatewayConfig, string, bool) {
	if c == nil {
		return matchGatewayInMap(DefaultGateways, path)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return matchGatewayInMap(c.Gateways, path)
}

func matchGatewayInMap(gateways map[string]GatewayConfig, path string) (GatewayConfig, string, bool) {
	var (
		best    GatewayConfig
		bestLen = -1
	)
	for _, gateway := range gateways {
		if path != gateway.Prefix && !strings.HasPrefix(path, gateway.Prefix+"/") {
			continue
		}
		if len(gateway.Prefix) > bestLen || (len(gateway.Prefix) == bestLen && gateway.ID < best.ID) {
			best, bestLen = gateway, len(gateway.Prefix)
		}
	}
	if bestLen < 0 {
		return GatewayConfig{}, "", false
	}
	return best, strings.TrimPrefix(path, best.Prefix), true
}

type GatewayPatch struct {
	Name                     *string   `json:"name"`
	BaseURL                  *string   `json:"base_url"`
	Enabled                  *bool     `json:"enabled"`
	ForwardHeaders           *[]string `json:"forward_headers"`
	SessionAffinity          *string   `json:"session_affinity"`
	UserAgentOverrideEnabled *bool     `json:"user_agent_override_enabled"`
	UserAgentOverride        *string   `json:"user_agent_override"`
	UseProxy                 *bool     `json:"use_proxy"`
	LegacyUseSystemProxy     *bool     `json:"use_system_proxy"`
}

var ErrUnknownGateway = errors.New("unknown gateway")
var ErrBuiltinGateway = errors.New("built-in gateway")

type NewGateway struct {
	Prefix  string `json:"prefix"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
}

func (c *Config) AddGateway(req NewGateway) (GatewayConfig, error) {
	id := strings.TrimPrefix(strings.TrimSpace(req.Prefix), "/")
	if !customGatewayIDPattern.MatchString(id) {
		return GatewayConfig{}, fmt.Errorf("gateway prefix %q must be a single segment matching %s", req.Prefix, customGatewayIDPattern.String())
	}
	if legacyGatewayIDs[id] {
		return GatewayConfig{}, fmt.Errorf("gateway prefix %q was used by a gateway this build removed", req.Prefix)
	}
	if IsBuiltinGateway(id) {
		return GatewayConfig{}, fmt.Errorf("%w %q", ErrBuiltinGateway, id)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.Gateways[id]; exists {
		return GatewayConfig{}, fmt.Errorf("gateway %q already exists", id)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = id
	}
	gateway := GatewayConfig{
		ID:                id,
		Prefix:            "/" + id,
		Name:              name,
		BaseURL:           strings.TrimSpace(req.BaseURL),
		Protocol:          ProtocolResponses,
		Enabled:           true,
		SessionAffinity:   SessionAffinityOpenAI,
		UserAgentOverride: DefaultUserAgentOverride,
		UseProxy:          true,
	}
	updated := copyGateways(c.Gateways)
	updated[id] = gateway
	if err := c.commitLocked(updated); err != nil {
		return GatewayConfig{}, err
	}
	return updated[id], nil
}

func (c *Config) DeleteGateway(id string) error {
	if IsBuiltinGateway(id) {
		return fmt.Errorf("%w %q", ErrBuiltinGateway, id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.Gateways[id]; !ok {
		return fmt.Errorf("%w %q", ErrUnknownGateway, id)
	}
	updated := copyGateways(c.Gateways)
	delete(updated, id)
	return c.commitLocked(updated)
}

func copyGateways(src map[string]GatewayConfig) map[string]GatewayConfig {
	result := make(map[string]GatewayConfig, len(src))
	for id, gateway := range src {
		result[id] = gateway
	}
	return result
}

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
	if patch.SessionAffinity != nil {
		gateway.SessionAffinity = strings.TrimSpace(*patch.SessionAffinity)
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

	updated := copyGateways(c.Gateways)
	updated[id] = normalizeGateway(id, current, gateway)
	if err := c.commitLocked(updated); err != nil {
		return GatewayConfig{}, err
	}
	return updated[id], nil
}

func normalizeGateway(id string, current, candidate GatewayConfig) GatewayConfig {
	candidate.ID = id
	candidate.Prefix = current.Prefix
	candidate.Protocol = current.Protocol
	if candidate.Name == "" {
		candidate.Name = current.Name
	}
	return candidate
}

func (c *Config) commitLocked(updated map[string]GatewayConfig) error {
	if err := ValidateConfig(c.listenAddr, updated); err != nil {
		return err
	}
	if err := ValidateGatewayModelKeys(updated); err != nil {
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

func (c *Config) ListenAddr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.listenAddr
}

func (c *Config) SetListenAddr(listenAddr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listenAddr = listenAddr
}

func (c *Config) ProxyURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.proxyURL
}

func (c *Config) ConfigPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.configPath
}

func (c *Config) SetConfigPath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configPath = path
}

func (c *Config) GetUpstreamTimeout() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.upstreamTimeout <= 0 {
		return DefaultUpstreamTimeout
	}
	return c.upstreamTimeout
}

func (c *Config) SetUpstreamTimeout(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upstreamTimeout = d
}

func (c *Config) GetLogRetention() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logRetention
}

func (c *Config) SetLogRetention(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logRetention = d
}

func (c *Config) GetBodyCaptureLimitKB() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bodyCaptureLimitKB
}

func (c *Config) SetBodyCaptureLimitKB(kb int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodyCaptureLimitKB = kb
}
