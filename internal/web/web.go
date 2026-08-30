package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/proxy"
	"grok-gateway-proxy/internal/store"
)

//go:embed static
var staticFiles embed.FS

func (a *App) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		proxy.WriteError(w, http.StatusMethodNotAllowed, fmt.Errorf("only GET is supported for the UI"))
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/ui" || r.URL.Path == "/ui/" {
		data, err := staticFiles.ReadFile("static/index.html")
		if err != nil {
			proxy.WriteError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
		return
	}
	root, err := fs.Sub(staticFiles, "static")
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	http.StripPrefix("/static/", http.FileServer(http.FS(root))).ServeHTTP(w, r)
}

// parseFilter builds the log filter from query parameters. An unparsable
// from/to is an error rather than a silently dropped bound: the caller believes
// it is looking at a time window, and answering with every row would corrupt
// counts and totals.
func parseFilter(r *http.Request) (store.LogFilter, error) {
	query := r.URL.Query()
	filter := store.LogFilter{
		GatewayID: query.Get("gateway"),
		Model:     query.Get("model"),
		Status:    query.Get("status"),
		Limit:     50,
	}
	if value, err := strconv.Atoi(query.Get("limit")); err == nil && value > 0 {
		filter.Limit = min(value, 500)
	}
	if value, err := strconv.Atoi(query.Get("offset")); err == nil && value >= 0 {
		filter.Offset = value
	}
	if value := query.Get("from"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return store.LogFilter{}, fmt.Errorf("invalid from timestamp: %w", err)
		}
		filter.From = &parsed
	}
	if value := query.Get("to"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return store.LogFilter{}, fmt.Errorf("invalid to timestamp: %w", err)
		}
		filter.To = &parsed
	}
	return filter, nil
}

// setupSnippets renders per-gateway client config snippets. The base URL uses
// the proxy's actual listen address so the snippets stay correct when the
// proxy is not on the default 127.0.0.1:8787.
func setupSnippets(listenAddr string, gateways map[string]config.GatewayConfig) map[string]string {
	base := normalizeListenAddr(listenAddr)
	result := make(map[string]string, len(gateways))
	for id, gateway := range gateways {
		modelName := strings.ReplaceAll(gateway.Name, " ", "-")
		modelName = strings.ToLower(modelName)
		result[id] = fmt.Sprintf(`[model.%s-model]
model = "..."
base_url = "%s%s"
api_backend = "%s"
`, modelName, base, gateway.Prefix, gateway.Protocol)
	}
	return result
}

// normalizeListenAddr turns a listen address into a usable base URL, adding a
// default host when the address is hostless. A wildcard host (0.0.0.0 / ::)
// is rewritten to the loopback: the snippet is meant to be opened by a browser
// on this machine, and no browser can connect to 0.0.0.0.
func normalizeListenAddr(listenAddr string) string {
	addr := strings.TrimSpace(listenAddr)
	if addr == "" {
		return "http://127.0.0.1:8787"
	}
	if !strings.Contains(addr, "://") {
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		if strings.HasPrefix(addr, "/") {
			addr = "http://localhost" + addr
		} else if !strings.Contains(addr, "://") {
			addr = "http://" + addr
		}
	}
	// A wildcard listen address is reachable on every interface, but only a
	// concrete host works as a base URL.
	addr = strings.Replace(addr, "://0.0.0.0", "://127.0.0.1", 1)
	addr = strings.Replace(addr, "://[::]", "://127.0.0.1", 1)
	addr = strings.Replace(addr, "://::", "://127.0.0.1", 1)
	return strings.TrimRight(addr, "/")
}
