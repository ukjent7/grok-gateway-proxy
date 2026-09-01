package web

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

type contractCase struct {
	module string
	root   string
	shape  string
}

var contractCases = []contractCase{
	{"static/js/drawer.js", "log", "log"},
	{"static/js/drawer.js", "usage", "logUsage"},
	{"static/js/utils.js", "log", "log"},
	{"static/js/utils.js", "gw", "gateway"},
	{"static/js/utils.js", "l", "logSummary"},
	{"static/js/logs.js", "data", "logsPage"},
	{"static/js/logs.js", "l", "logSummary"},
	{"static/js/logs.js", "usage", "logSummaryUsage"},
	{"static/js/logs.js", "u", "logSummaryUsage"},
	{"static/js/overview.js", "data", "logsPage"},
	{"static/js/overview.js", "l", "logSummary"},
	{"static/js/overview.js", "m", "metrics"},
	{"static/js/overview.js", "metrics", "metrics"},
	{"static/js/overview.js", "health", "health"},
	{"static/js/pulse.js", "pulseData", "pulse"},
	{"static/js/pulse.js", "p", "pulseRow"},
	{"static/js/pulse.js", "gw", "gateway"},
	{"static/js/gateways.js", "gw", "gateway"},
	{"static/js/gateways.js", "g", "gateway"},
	{"static/js/gateways.js", "u", "healthEntry"},
	{"static/js/gateways.js", "health", "health"},
	{"static/js/gateways.js", "res", "proxyPatch"},
	{"static/js/health.js", "health", "health"},
	{"static/js/health.js", "u", "healthEntry"},
	{"static/js/health.js", "gw", "gateway"},
	{"static/js/cmdk.js", "gw", "gateway"},
	{"static/js/setup.js", "gw", "gateway"},
	{"static/js/setup.js", "item", "setupSnippet"},
	{"static/js/state.js", "cfg", "config"},
	{"static/js/state.js", "gw", "gateway"},
}

// Every field the console names has to be present, so the fixtures are the
// widest form of each payload: a gateway with an override set, a log with the
// optional members filled, a probe that has run. omitempty is only a hazard for
// a contract test if the fixture leaves the field out on purpose.
func apiShapes(t *testing.T) map[string]map[string]any {
	t.Helper()
	app, st := newTestApp(t)
	const gatewayID = "contract-gw"
	app.config.Gateways[gatewayID] = config.GatewayConfig{
		ID: gatewayID, Prefix: "/" + gatewayID, Name: "Contract Gateway",
		BaseURL: "https://upstream.invalid/v1", Protocol: config.ProtocolResponses,
		Enabled: true, UserAgentOverrideEnabled: true,
		UserAgentOverride: "grok-console-contract/1",
		ForwardHeaders:    []string{"Authorization", "X-Custom"},
		SessionAffinity:   config.SessionAffinityOpenAI,
	}
	if err := st.Insert(context.Background(), contractLog(gatewayID)); err != nil {
		t.Fatal(err)
	}
	// /healthz reports only what the background sweep has measured, so the
	// cached table is seeded rather than probed: this is about the shape of one
	// entry, and healthEntry() is shared with the on-demand probe endpoint.
	app.upstreams = map[string]upstreamHealth{
		gatewayID: {Reachable: false, Status: 502, Err: "connection refused", CheckedAt: time.Now().UTC()},
	}

	get := func(target string) map[string]any {
		return decodeObject(t, getJSON(t, app, "http://127.0.0.1:8787"+target))
	}
	patchProxy := func() map[string]any {
		recorder := serveAPI(http.MethodPatch, "http://127.0.0.1:8787/api/proxy",
			`{"proxy_url":"http://127.0.0.1:7890"}`, app)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PATCH /api/proxy: %d %s", recorder.Code, recorder.Body.String())
		}
		return decodeObject(t, recorder.Body.Bytes())
	}

	logsPage := get("/api/logs?limit=10")
	configRoot := get("/api/config")
	healthRoot := get("/healthz")
	pulseRoot := get("/api/pulse?limit=40")
	full := get("/api/logs/" + contractLogID)
	summary := objectAt(t, logsPage, "items")
	return map[string]map[string]any{
		"log":             full,
		"logUsage":        objectAt(t, full, "usage"),
		"logSummary":      summary,
		"logSummaryUsage": objectAt(t, summary, "usage"),
		"logsPage":        logsPage,
		"metrics":         get("/api/metrics"),
		"pulse":           pulseRoot,
		"pulseRow":        objectAt(t, pulseRoot, "gateways", gatewayID),
		"gateway":         objectAt(t, configRoot, "gateways", gatewayID),
		"config":          configRoot,
		"health":          healthRoot,
		"healthEntry":     objectAt(t, healthRoot, "upstreams", gatewayID),
		"proxyPatch":      patchProxy(),
		"setupSnippet":    objectAt(t, get("/api/setup"), gatewayID),
	}
}

const contractLogID = "contract-log-1"

// contractLog fills every column the console reads a field for, including the
// three that are omitempty on the wire: error, response_truncated and
// usage.cache_source.
func contractLog(gatewayID string) store.RequestLog {
	return store.RequestLog{
		ID: contractLogID, StartedAt: time.Now().UTC(),
		GatewayID: gatewayID, GatewayName: "Contract Gateway", Prefix: "/" + gatewayID,
		IngressProtocol: config.ProtocolResponses, UpstreamProtocol: config.ProtocolResponses,
		Model: "grok-4", RequestPath: "/" + gatewayID + "/responses",
		RequestURL:  "http://127.0.0.1:8787/" + gatewayID + "/responses?trace=1",
		UpstreamURL: "https://upstream.invalid/v1/responses", Method: http.MethodPost,
		StatusCode: 200, ClientResponseStatusCode: 200, UpstreamResponseStatusCode: 200,
		Success: true, Stream: true, DurationMS: 1234,
		Error:                   "upstream retried once",
		ResponseTruncated:       true,
		RequestHeaders:          `{"content-type":["application/json"]}`,
		RequestBody:             []byte(`{"model":"grok-4","client":true}`),
		UpstreamHeaders:         `{"authorization":["Bearer redacted"]}`,
		UpstreamBody:            []byte(`{"model":"grok-4","upstream":true}`),
		UpstreamResponseHeaders: `{"content-type":["text/event-stream"]}`,
		UpstreamResponseBody:    []byte(`{"type":"response.completed"}`),
		ResponseHeaders:         `{"content-type":["text/event-stream"]}`,
		ResponseBody:            []byte(`{"type":"response.output_text.delta"}`),
		Usage: store.UsageMetrics{InputTokens: 10, CacheReadTokens: 5, CacheWriteTokens: 1,
			PromptTokens: 20, OutputTokens: 7, ReasoningTokens: 3, CacheSupported: true,
			UsagePresent: true, CacheSource: "usage-source"},
	}
}

func TestConsoleOnlyReadsFieldsTheAPIReturns(t *testing.T) {
	shapes := apiShapes(t)
	checked := 0
	for _, c := range contractCases {
		fields := jsFieldReads(t, c.module, c.root)
		if len(fields) == 0 {
			// A case that reads nothing is a stale case: it would pass forever
			// while the console moved on, which is the failure mode this whole
			// file exists to prevent.
			t.Errorf("%s no longer reads %s.<field>: the case is stale, drop or repoint it", c.module, c.root)
			continue
		}
		shape, ok := shapes[c.shape]
		if !ok {
			t.Fatalf("no API shape named %q", c.shape)
		}
		for _, field := range fields {
			if _, present := shape[field]; !present {
				t.Errorf("%s reads %s.%s, which %s does not return (it returns %s)",
					c.module, c.root, field, c.shape, strings.Join(sortedKeys(shape), " "))
			}
		}
		checked += len(fields)
	}
	if checked == 0 {
		t.Fatal("no contract was checked")
	}
}

// The console renders a request URL as prefix + path, so a prefix has to arrive
// with exactly one leading slash. Three call sites had to be fixed for `//ds`;
// this pins the value itself, which no key-name check would catch, and checks
// the setup snippets that are built the same way.
func TestGatewayPrefixesHaveOneLeadingSlash(t *testing.T) {
	app, _ := newTestApp(t)
	configRoot := decodeObject(t, getJSON(t, app, "http://127.0.0.1:8787/api/config"))
	gateways := objectAt(t, configRoot, "gateways")
	if len(gateways) == 0 {
		t.Fatal("no gateways configured, so the prefix contract went un-checked")
	}
	for id, view := range gateways {
		gateway, ok := view.(map[string]any)
		if !ok {
			t.Fatalf("gateway %q is not an object: %T", id, view)
		}
		prefix, _ := gateway["prefix"].(string)
		if !strings.HasPrefix(prefix, "/") || strings.HasPrefix(prefix, "//") {
			t.Errorf("gateway %q has prefix %q: the console renders prefix + path", id, prefix)
		}
	}

	baseURLRe := regexp.MustCompile(`base_url = "([^"]*)"`)
	for id, value := range decodeObject(t, getJSON(t, app, "http://127.0.0.1:8787/api/setup")) {
		snippet, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("setup entry %q is not an object: %T", id, value)
		}
		text, _ := snippet["snippet"].(string)
		match := baseURLRe.FindStringSubmatch(text)
		if match == nil {
			t.Fatalf("setup entry %q has no base_url line: %q", id, text)
		}
		afterScheme := strings.TrimPrefix(match[1], "http://")
		if strings.Contains(afterScheme, "//") {
			t.Errorf("setup snippet for %q points at %q: prefix doubled its slash", id, match[1])
		}
	}
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return obj
}

// objectAt walks into a decoded response. A path element naming an array takes
// its first element, which is how the shapes below reach one log row or one
// gateway's pulse rows.
func objectAt(t *testing.T, obj map[string]any, path ...string) map[string]any {
	t.Helper()
	current := obj
	for i, key := range path {
		value, ok := current[key]
		if !ok {
			t.Fatalf("response has no %q at %s (keys: %s)", key, strings.Join(path[:i+1], "."), strings.Join(sortedKeys(current), " "))
		}
		if next, ok := value.(map[string]any); ok {
			current = next
			continue
		}
		list, ok := value.([]any)
		if !ok || len(list) == 0 {
			t.Fatalf("%s is neither an object nor a non-empty array: %T", strings.Join(path[:i+1], "."), value)
		}
		next, ok := list[0].(map[string]any)
		if !ok {
			t.Fatalf("%s[0] is not an object", strings.Join(path[:i+1], "."))
		}
		current = next
	}
	return current
}

func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// importDeclRe matches one ES module import declaration — the multi-line brace
// form the console uses when it pulls in several helpers, or a bare
// side-effect import.
var importDeclRe = regexp.MustCompile(`(?ms)^import\s+.*?\s+from\s*['"][^'"]*['"]\s*;?|^import\s*['"][^'"]*['"]\s*;?`)

// jsFieldReads lists the distinct payload fields one module reads off an
// identifier. RE2 has no lookbehind, so the left boundary is part of the
// pattern. Reads whose name contains a capital are skipped, because `p.dataset`
// and `el.classList` name a DOM property, not an API field. Import declarations
// are removed first: `from './health.js'` otherwise reads as a field named `js`
// on a payload named health, and imports never read anything.
func jsFieldReads(t *testing.T, module, root string) []string {
	t.Helper()
	source, err := staticFiles.ReadFile(module)
	if err != nil {
		t.Fatalf("read the shipped module %s: %v", module, err)
	}
	source = importDeclRe.ReplaceAll(source, nil)
	pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_$])` + regexp.QuoteMeta(root) + `\.([A-Za-z][A-Za-z0-9_]*)`)
	seen := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
		field := match[2]
		if strings.ContainsFunc(field, unicode.IsUpper) {
			continue
		}
		seen[field] = true
	}
	fields := make([]string, 0, len(seen))
	for field := range seen {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}
