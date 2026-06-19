package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCodexParser_ParseFixture(t *testing.T) {
	parser := CodexParser{}
	path := filepath.Join("testdata", "codex", "sessions", "2026", "06", "18", "rollout-2026-06-18T10-00-00-sess-codex-1.jsonl")

	tr, err := parser.ParseFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if tr.Tool != ToolCodex {
		t.Fatalf("tool = %q", tr.Tool)
	}
	if tr.SessionID != "sess-codex-1" {
		t.Fatalf("session id = %q", tr.SessionID)
	}
	if tr.ParentSessionID != "parent-thread-9" {
		t.Fatalf("parent session id = %q", tr.ParentSessionID)
	}
	if tr.Project != "/home/jdugan/proj" {
		t.Fatalf("project = %q", tr.Project)
	}
	if tr.Title != "Implement the Codex parser." {
		t.Fatalf("title = %q", tr.Title)
	}

	want := []Message{
		{Role: "human", Text: "Implement the Codex parser.", MsgID: "0"},
		{Role: "assistant", Text: "On it. Reading the files.", MsgID: "1"},
		{Role: "assistant", Text: "[tool: shell]", MsgID: "2"},
		{Role: "assistant", Text: "[tool result omitted: 16 bytes]", MsgID: "3"},
		{Role: "assistant", Text: "Done.", MsgID: "4"},
	}
	if len(tr.Messages) != len(want) {
		t.Fatalf("message count = %d, want %d: %#v", len(tr.Messages), len(want), tr.Messages)
	}
	for i := range want {
		if tr.Messages[i].Role != want[i].Role || tr.Messages[i].Text != want[i].Text || tr.Messages[i].MsgID != want[i].MsgID {
			t.Fatalf("message %d = %#v, want %#v", i, tr.Messages[i], want[i])
		}
	}
}

func TestCodexParser_DiscoverFindsRollouts(t *testing.T) {
	parser := CodexParser{}
	root := filepath.Join("testdata", "codex")

	paths, err := parser.Discover(context.Background(), []string{root})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "rollout-2026-06-18T10-00-00-sess-codex-1.jsonl" {
		t.Fatalf("paths = %#v", paths)
	}
}
