// ABOUTME: Shared types for the local Engram capture daemon.
// ABOUTME: Kept lightweight so the daemon does not import server DB dependencies.

package main

import (
	"context"
	"time"
)

const (
	ToolClaudeCode = "claude-code"
	ToolCodex      = "codex"
	ToolZed        = "zed"
	ToolPi         = "pi"
)

type Message struct {
	Role  string    `json:"role"`
	Text  string    `json:"text"`
	Ts    time.Time `json:"ts"`
	MsgID string    `json:"msg_id"`
}

type Transcript struct {
	Tool      string
	SessionID string
	Title     string
	Project   string
	Path      string
	ModTime   time.Time
	Messages  []Message
}

type IngestMessage struct {
	Role  string `json:"role"`
	Text  string `json:"text"`
	Ts    string `json:"ts"`
	MsgID string `json:"msg_id"`
}

type IngestBatch struct {
	Tool         string          `json:"tool"`
	SessionID    string          `json:"session_id"`
	Title        string          `json:"title"`
	Project      string          `json:"project"`
	Messages     []IngestMessage `json:"messages"`
	SessionEnded bool            `json:"session_ended"`
}

type IngestResult struct {
	ChunksCreated int  `json:"chunks_created"`
	Summarized    bool `json:"summarized"`
	MessageCount  int  `json:"message_count"`
}

type Parser interface {
	Tool() string
	Discover(ctx context.Context, roots []string) ([]string, error)
	ParseFile(ctx context.Context, path string) (Transcript, error)
}

type DryRunStats struct {
	Sessions       int
	Messages       int
	WouldPost      int
	SkippedNoOp    int
	ParseFailures  int
	EstimatedBytes int
}
