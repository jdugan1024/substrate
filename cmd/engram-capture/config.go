package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	BaseURL       string
	PAT           string
	StatePath     string
	ClaudeRoots   []string
	SweepInterval time.Duration
	Debounce      time.Duration
	EndedAfter    time.Duration
	Trim          TrimConfig
	DryRun        bool
	Backfill      bool
	Watch         bool
}

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		BaseURL:       envDefault("ENGRAM_URL", "https://engram.x1024.net"),
		PAT:           os.Getenv("ENGRAM_PAT"),
		StatePath:     filepath.Join(home, ".local", "state", "engram-capture", "state.json"),
		ClaudeRoots:   []string{filepath.Join(home, ".claude", "projects")},
		SweepInterval: 30 * time.Second,
		Debounce:      2 * time.Second,
		EndedAfter:    10 * time.Minute,
		Trim:          DefaultTrimConfig(),
	}
}

func (c Config) Validate() error {
	if !c.DryRun && strings.TrimSpace(c.PAT) == "" {
		return errors.New("ENGRAM_PAT is required unless --dry-run is set")
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("server URL is required")
	}
	if len(c.ClaudeRoots) == 0 {
		return errors.New("at least one Claude Code root is required")
	}
	return nil
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
