package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestModelKeyIsASCIISlug(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "DeepSeek", id: "ds", want: "deepseek-model"},
		{name: "标准 Responses", id: "std", want: "responses-model"},
		{name: "  Spaced   Out  ", id: "std", want: "spaced-out-model"},

		{name: "中文网关", id: "ds", want: "ds-model"},
		{name: "", id: "ds", want: "ds-model"},
	}
	for _, test := range tests {
		if got := ModelKey(test.name, test.id); got != test.want {
			t.Errorf("ModelKey(%q, %q) = %q, want %q", test.name, test.id, got, test.want)
		}
	}
}

func TestAddGatewayRejectsCollidingModelKeyWithoutWriting(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddGateway(NewGateway{Prefix: "/team", Name: "Team Gateway"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	onDisk, err := LoadConfig(cfg.ConfigPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk.Gateways["team"]; !ok {
		t.Fatal("the accepted gateway did not reach the file")
	}

	if _, err := cfg.AddGateway(NewGateway{Prefix: "/clone", Name: "Team  Gateway"}); err == nil {
		t.Fatal("expected the colliding name to be rejected")
	} else if !strings.Contains(err.Error(), "client model key") {
		t.Fatalf("unexpected rejection: %v", err)
	}

	onDisk, err = LoadConfig(cfg.ConfigPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk.Gateways["clone"]; ok {
		t.Fatal("the rejected gateway was written to the file")
	}
	if _, ok := cfg.Gateways["clone"]; ok {
		t.Fatal("the rejected gateway reached the live config")
	}
}
