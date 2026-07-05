package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	cfg := DefaultConfig()

	var claudeRoots string
	var codexRoots string
	flag.StringVar(&cfg.BaseURL, "url", cfg.BaseURL, "Substrate base URL")
	flag.StringVar(&cfg.StatePath, "state", cfg.StatePath, "local state JSON path")
	flag.StringVar(&claudeRoots, "claude-root", strings.Join(cfg.ClaudeRoots, string(os.PathListSeparator)), "Claude Code transcript root(s), path-list separated")
	flag.StringVar(&codexRoots, "codex-root", strings.Join(cfg.CodexRoots, string(os.PathListSeparator)), "Codex transcript root(s), path-list separated")
	flag.StringVar(&cfg.Machine, "machine", cfg.Machine, "source machine name")
	flag.StringVar(&cfg.Username, "username", cfg.Username, "source username")
	flag.DurationVar(&cfg.SweepInterval, "sweep-interval", cfg.SweepInterval, "periodic scan interval")
	flag.DurationVar(&cfg.Debounce, "debounce", cfg.Debounce, "watch debounce interval")
	flag.DurationVar(&cfg.EndedAfter, "ended-after", cfg.EndedAfter, "mark sessions ended after file idle duration")
	flag.IntVar(&cfg.Trim.MaxMessageBytes, "max-message-bytes", cfg.Trim.MaxMessageBytes, "replace messages larger than this byte count")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "scan and report without posting")
	flag.BoolVar(&cfg.Backfill, "backfill", false, "scan once and exit")
	flag.BoolVar(&cfg.Watch, "watch", false, "watch for changes after initial scan")
	flag.Parse()

	cfg.ClaudeRoots = filepathList(claudeRoots)
	cfg.CodexRoots = filepathList(codexRoots)
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	runner := Runner{
		Parsers: []Parser{ClaudeCodeParser{}, CodexParser{}},
		Roots: map[string][]string{
			ToolClaudeCode: cfg.ClaudeRoots,
			ToolCodex:      cfg.CodexRoots,
		},
		State:      state,
		Poster:     NewIngestClient(cfg.BaseURL, cfg.PAT, &http.Client{Timeout: 30 * time.Second}),
		Trim:       cfg.Trim,
		Machine:    cfg.Machine,
		Username:   cfg.Username,
		EndedAfter: cfg.EndedAfter,
		Now:        time.Now,
		DryRun:     cfg.DryRun,
	}

	ctx := context.Background()
	stats, err := runner.ScanOnce(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("scan: sessions=%d messages=%d would_post=%d skipped_noop=%d parse_failures=%d estimated_bytes=%d\n",
		stats.Sessions, stats.Messages, stats.WouldPost, stats.SkippedNoOp, stats.ParseFailures, stats.EstimatedBytes)

	if cfg.Watch && !cfg.Backfill {
		if err := Watch(ctx, cfg, runner); err != nil {
			log.Fatal(err)
		}
	}
}

func filepathList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, string(os.PathListSeparator)) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
