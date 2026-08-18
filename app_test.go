package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticAssetsAreNotMatchedAsGatewayPaths(t *testing.T) {
	app := &App{}

	for _, path := range []string{"/static/style.css", "/static/app.js"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+path, nil)
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, recorder.Code, recorder.Body.String())
		}
		if strings.TrimSpace(recorder.Body.String()) == "" {
			t.Fatalf("%s: expected embedded asset body", path)
		}
	}
}

func TestHasPathPrefixUsesPathBoundary(t *testing.T) {
	for _, test := range []struct {
		path   string
		prefix string
		want   bool
	}{
		{path: "/st", prefix: "/st", want: true},
		{path: "/st/chat/completions", prefix: "/st", want: true},
		{path: "/static/app.js", prefix: "/st", want: false},
		{path: "/status", prefix: "/st", want: false},
	} {
		if got := hasPathPrefix(test.path, test.prefix); got != test.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", test.path, test.prefix, got, test.want)
		}
	}
}
