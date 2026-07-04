package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type recordingPoster struct{ batches []IngestBatch }

func (p *recordingPoster) Post(ctx context.Context, batch IngestBatch) (IngestResult, error) {
	p.batches = append(p.batches, batch)
	return IngestResult{MessageCount: len(batch.Messages)}, nil
}

func TestRunnerDryRunDoesNotPost(t *testing.T) {
	parser := ClaudeCodeParser{}
	state, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	poster := &recordingPoster{}
	runner := Runner{
		Parsers:    []Parser{parser},
		Roots:      map[string][]string{ToolClaudeCode: []string{filepath.Join("testdata", "claude-code")}},
		State:      state,
		Poster:     poster,
		Trim:       TrimConfig{MaxMessageBytes: 1024},
		EndedAfter: time.Hour,
		Now:        func() time.Time { return time.Date(2026, 6, 13, 19, 0, 0, 0, time.UTC) },
		DryRun:     true,
	}

	stats, err := runner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if stats.Sessions != 1 || stats.Messages != 5 || stats.WouldPost != 1 {
		t.Fatalf("stats = %#v", stats)
	}
	if len(poster.batches) != 0 {
		t.Fatalf("dry run posted %d batches", len(poster.batches))
	}
}

func TestRunnerSkipsNoOpAfterPost(t *testing.T) {
	state, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	poster := &recordingPoster{}
	runner := Runner{
		Parsers:    []Parser{ClaudeCodeParser{}},
		Roots:      map[string][]string{ToolClaudeCode: []string{filepath.Join("testdata", "claude-code")}},
		State:      state,
		Poster:     poster,
		Trim:       TrimConfig{MaxMessageBytes: 1024},
		EndedAfter: time.Hour,
		Now:        time.Now,
	}

	if _, err := runner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if _, err := runner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(poster.batches) != 1 {
		t.Fatalf("posted batches = %d", len(poster.batches))
	}
}
