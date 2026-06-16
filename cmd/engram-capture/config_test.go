package main

import "testing"

func TestDefaultConfigUsesSourceMetadataEnvOverrides(t *testing.T) {
	t.Setenv("ENGRAM_CAPTURE_MACHINE", "desktop")
	t.Setenv("ENGRAM_CAPTURE_USERNAME", "jdugan")

	cfg := DefaultConfig()

	if cfg.Machine != "desktop" {
		t.Fatalf("machine = %q", cfg.Machine)
	}
	if cfg.Username != "jdugan" {
		t.Fatalf("username = %q", cfg.Username)
	}
}
