package main

import (
	"log/slog"
	"testing"
)

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"127.0.0.2:8787", true},
		{"localhost:8787", true},
		{"LOCALHOST:8787", true},
		{"[::1]:8787", true},
		{":8787", false},
		{"0.0.0.0:8787", false},
		{"[::]:8787", false},
		{"192.168.1.100:8787", false},
		{"example.com:8787", false},
	}

	for _, tt := range tests {
		if got := isLoopbackAddr(tt.addr); got != tt.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	if parseLogLevel("debug") != slog.LevelDebug {
		t.Error("expected debug level")
	}
	if parseLogLevel("warn") != slog.LevelWarn {
		t.Error("expected warn level")
	}
	if parseLogLevel("error") != slog.LevelError {
		t.Error("expected error level")
	}
	if parseLogLevel("unknown") != slog.LevelInfo {
		t.Error("expected default info level")
	}
}
