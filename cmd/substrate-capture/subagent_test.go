package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestClaudeCodeParser_TopLevelHasNoParent(t *testing.T) {
	parser := ClaudeCodeParser{}
	path := filepath.Join("testdata", "claude-code", "basic.jsonl")

	tr, err := parser.ParseFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if tr.ParentSessionID != "" {
		t.Fatalf("top-level transcript should have no parent, got %q", tr.ParentSessionID)
	}
}

func TestClaudeCodeParser_SubagentBecomesLinkedSession(t *testing.T) {
	parser := ClaudeCodeParser{}
	path := filepath.Join("testdata", "subagent", "agent-deadbeef12345678.jsonl")

	tr, err := parser.ParseFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	// A subagent file becomes its own session keyed on the agentId, linked to
	// the parent session it ran under.
	if tr.SessionID != "deadbeef12345678" {
		t.Fatalf("subagent session id = %q, want agentId", tr.SessionID)
	}
	if tr.ParentSessionID != "parent-xyz-999" {
		t.Fatalf("parent session id = %q, want parent sessionId", tr.ParentSessionID)
	}
	// Title comes from the sibling .meta.json description.
	if tr.Title != "Implement streaming parser" {
		t.Fatalf("title = %q, want meta description", tr.Title)
	}
	if len(tr.Messages) != 2 {
		t.Fatalf("messages = %d, want 2: %#v", len(tr.Messages), tr.Messages)
	}
	if tr.Messages[0].Role != "human" || tr.Messages[1].Role != "assistant" {
		t.Fatalf("roles wrong: %#v", tr.Messages)
	}
}

func TestBuildIngestBatch_CarriesParentSessionID(t *testing.T) {
	tr := Transcript{
		Tool: "claude-code", SessionID: "deadbeef12345678", ParentSessionID: "parent-xyz-999",
		Title: "t", Project: "/repo",
		Messages: []Message{{Role: "human", Text: "hi", MsgID: "m1"}},
	}
	batch := BuildIngestBatch(tr, TrimConfig{MaxMessageBytes: 1024}, tr.ModTime, 0, "machine", "user")
	if batch.ParentSessionID != "parent-xyz-999" {
		t.Fatalf("batch parent session id = %q", batch.ParentSessionID)
	}
}
