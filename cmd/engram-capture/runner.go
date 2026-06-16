package main

import (
	"context"
	"encoding/json"
	"log"
	"time"
)

type Poster interface {
	Post(ctx context.Context, batch IngestBatch) (IngestResult, error)
}

type Runner struct {
	Parsers    []Parser
	Roots      map[string][]string
	State      *StateStore
	Poster     Poster
	Trim       TrimConfig
	Machine    string
	Username   string
	EndedAfter time.Duration
	Now        func() time.Time
	DryRun     bool
}

func (r Runner) ScanOnce(ctx context.Context) (DryRunStats, error) {
	var stats DryRunStats
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	for _, parser := range r.Parsers {
		paths, err := parser.Discover(ctx, r.Roots[parser.Tool()])
		if err != nil {
			return stats, err
		}
		for _, path := range paths {
			tr, err := parser.ParseFile(ctx, path)
			if err != nil {
				stats.ParseFailures++
				log.Printf("parse failed: tool=%s path=%s err=%v", parser.Tool(), path, err)
				continue
			}
			if len(tr.Messages) == 0 {
				continue
			}
			batch := BuildIngestBatch(tr, r.Trim, now(), r.EndedAfter, r.Machine, r.Username)
			stats.Sessions++
			stats.Messages += len(batch.Messages)
			stats.EstimatedBytes += estimateBatchBytes(batch)
			if r.State.ShouldSkip(batch) {
				stats.SkippedNoOp++
				continue
			}
			stats.WouldPost++
			if r.DryRun {
				continue
			}
			if _, err := r.Poster.Post(ctx, batch); err != nil {
				return stats, err
			}
			r.State.MarkPosted(batch)
			if err := r.State.Save(); err != nil {
				return stats, err
			}
		}
	}
	return stats, nil
}

func estimateBatchBytes(batch IngestBatch) int {
	b, err := json.Marshal(batch)
	if err != nil {
		return 0
	}
	return len(b)
}
