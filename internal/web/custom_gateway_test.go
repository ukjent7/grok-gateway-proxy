package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
)

func decodeGatewayResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func configuredGateways(t *testing.T, app *App) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d", recorder.Code)
	}
	return decodeGatewayResponse(t, recorder)["gateways"].(map[string]any)
}

func TestCreateAndDeleteCustomGatewayThroughAPI(t *testing.T) {
	app, _ := newTestApp(t)

	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/gateways",
		strings.NewReader(`{"prefix":"/mygate","name":"My Gate","base_url":"https://api.example.com/v1"}`)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	created := decodeGatewayResponse(t, recorder)["gateway"].(map[string]any)
	if created["id"] != "mygate" || created["prefix"] != "/mygate" {
		t.Fatalf("created gateway identity = %v", created)
	}
	// The console needs to tell built-in from user-created without hard-coding
	// the built-in list, so the flag is computed server-side.
	if created["custom"] != true {
		t.Fatalf("created gateway should be flagged custom: %v", created)
	}

	gateways := configuredGateways(t, app)
	if _, ok := gateways["mygate"]; !ok {
		t.Fatalf("the new gateway is missing from /api/config: %v", gateways)
	}
	if gateways["mygate"].(map[string]any)["custom"] != true {
		t.Error("custom flag missing on the config listing")
	}
	if gateways["ds"].(map[string]any)["custom"] != false {
		t.Error("a built-in gateway was reported as custom")
	}

	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8787/api/gateways/mygate", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, ok := configuredGateways(t, app)["mygate"]; ok {
		t.Fatal("the gateway survived the delete")
	}
}

func TestBuiltInGatewayCannotBeDeletedThroughAPI(t *testing.T) {
	app, _ := newTestApp(t)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8787/api/gateways/ds", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("deleting a built-in gateway = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	if _, ok := configuredGateways(t, app)["ds"]; !ok {
		t.Fatal("the built-in gateway disappeared")
	}
}

func TestCreateGatewayRejectsUnknownAndMalformed(t *testing.T) {
	app, _ := newTestApp(t)
	tests := []struct {
		name string
		body string
		code int
	}{
		{name: "uppercase prefix", body: `{"prefix":"/MyGate","name":"x"}`, code: http.StatusBadRequest},
		{name: "reserved prefix", body: `{"prefix":"/api","name":"x"}`, code: http.StatusBadRequest},
		{name: "built-in prefix", body: `{"prefix":"/ds","name":"x"}`, code: http.StatusConflict},
		{name: "removed legacy prefix", body: `{"prefix":"/oc","name":"x"}`, code: http.StatusBadRequest},
		{name: "insecure base url", body: `{"prefix":"/ok","name":"x","base_url":"http://insecure.test"}`, code: http.StatusBadRequest},
		{name: "not json", body: `prefix=ok`, code: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/gateways", strings.NewReader(test.body)))
			if recorder.Code != test.code {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.code, recorder.Body.String())
			}
		})
	}
	// None of the rejected attempts may leave a gateway behind.
	for _, id := range []string{"ok", "api", "MyGate", "oc"} {
		if _, exists := app.config.Snapshot()[id]; exists {
			t.Errorf("rejected create left gateway %q configured", id)
		}
	}
}

// Two gateways whose names normalise to the same model key would emit two
// `[model.x-model]` sections in the client config, and the client keeps only
// one of them. The create must fail and leave nothing behind.
func TestCreateGatewayRejectsCollidingModelKey(t *testing.T) {
	app, _ := newTestApp(t)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/gateways",
		strings.NewReader(`{"prefix":"/clone","name":"DeepSeek"}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("colliding name = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if _, exists := app.config.Snapshot()["clone"]; exists {
		t.Fatal("the colliding gateway was left configured after the rejection")
	}
}

// The regression this guards: gateway routing used to be decided against the
// compiled-in table, so a runtime-created prefix fell through to the UI handler
// and 404'd behind its own address.
func TestCustomPrefixIsRoutedToProxyNotUI(t *testing.T) {
	app, _ := newTestApp(t)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/gateways",
		strings.NewReader(`{"prefix":"/mygate","name":"My Gate"}`)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mygate/responses",
		strings.NewReader(`{"model":"m","input":"hi"}`)))
	// No base URL was set, so the proxy answers 503. A 404 here means the
	// request never reached it.
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d from the proxy: %s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no base URL configured") {
		t.Fatalf("unexpected proxy answer: %s", recorder.Body.String())
	}
}

func TestRouteIsGatewayUsesLiveConfig(t *testing.T) {
	app, _ := newTestApp(t)
	if app.routeIsGateway("/mygate/responses") {
		t.Fatal("an unconfigured prefix should not route to the proxy")
	}
	app.config.Gateways["mygate"] = config.GatewayConfig{ID: "mygate", Prefix: "/mygate", Protocol: config.ProtocolResponses}
	if !app.routeIsGateway("/mygate/responses") {
		t.Fatal("a configured custom prefix should route to the proxy")
	}
	// Component-boundary matching, not string prefixing: /mygateway is a
	// different route entirely.
	if app.routeIsGateway("/mygateway/responses") {
		t.Fatal("prefix matching must respect path components")
	}
	// A bare App has no config to read; it must still recognise the built-ins.
	if !(&App{}).routeIsGateway("/std/responses") {
		t.Fatal("a bare App should fall back to the built-in prefixes")
	}
}

// The client model key is derived from the display name, so renaming a gateway
// can collide exactly as well as creating one. Without this the uniqueness
// invariant would only hold on the path that checked it.
func TestRenamingGatewayOntoAnotherModelKeyIsRejected(t *testing.T) {
	app, _ := newTestApp(t)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/gateways",
		strings.NewReader(`{"prefix":"/team","name":"Team Gateway"}`)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/gateways/team",
		strings.NewReader(`{"name":"DeepSeek"}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("renaming onto a used model key = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if got := app.config.Snapshot()["team"].Name; got != "Team Gateway" {
		t.Fatalf("the rejected rename changed the name to %q", got)
	}

	// A name that does not collide still goes through.
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/gateways/team",
		strings.NewReader(`{"name":"Team Second"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("legal rename = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := app.config.Snapshot()["team"].Name; got != "Team Second" {
		t.Fatalf("name = %q, want Team Second", got)
	}
}
