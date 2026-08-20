package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigRetentionPrecedence(t *testing.T) {
	intPtr := func(v int) *int { return &v }

	tests := []struct {
		name     string
		file     string // "" means no config file
		explicit *int
		wantDays int
		wantErr  bool
	}{
		{
			name:     "no file, no explicit value falls back to default",
			file:     "",
			explicit: nil,
			wantDays: 30,
		},
		{
			name:     "no file, explicit value applies",
			file:     "",
			explicit: intPtr(7),
			wantDays: 7,
		},
		{
			name:     "file value used when nothing is explicit",
			file:     `{"log_retention_days": 90}`,
			explicit: nil,
			wantDays: 90,
		},
		{
			name:     "explicit value outranks file value",
			file:     `{"log_retention_days": 90}`,
			explicit: intPtr(7),
			wantDays: 7,
		},
		{
			name:     "explicit zero means keep forever",
			file:     `{"log_retention_days": 90}`,
			explicit: intPtr(0),
			wantDays: 0,
		},
		{
			name:     "file zero means keep forever",
			file:     `{"log_retention_days": 0}`,
			explicit: nil,
			wantDays: 0,
		},
		{
			name:     "negative explicit value is rejected",
			file:     "",
			explicit: intPtr(-1),
			wantErr:  true,
		},
		{
			name:     "negative file value is rejected",
			file:     `{"log_retention_days": -1}`,
			explicit: nil,
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tc.file != "" {
				path = writeConfigFile(t, tc.file)
			}
			cfg, err := LoadConfig(path, tc.explicit)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			want := time.Duration(tc.wantDays) * 24 * time.Hour
			if cfg.LogRetention != want {
				t.Fatalf("LogRetention = %v, want %v", cfg.LogRetention, want)
			}
		})
	}
}
