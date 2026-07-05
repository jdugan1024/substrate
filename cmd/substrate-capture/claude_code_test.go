package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestClaudeCodeParser_ParseFixture(t *testing.T) {
	parser := ClaudeCodeParser{}
	path := filepath.Join("testdata", "claude-code", "basic.jsonl")

	tr, err := parser.ParseFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}

	if tr.Tool != ToolClaudeCode {
		t.Fatalf("tool = %q", tr.Tool)
	}
	if tr.SessionID != "abc-123" {
		t.Fatalf("session id = %q", tr.SessionID)
	}
	if tr.Project != "/home/jdugan/engram" {
		t.Fatalf("project = %q", tr.Project)
	}
	if tr.Title != "Please inspect the repo." {
		t.Fatalf("title = %q", tr.Title)
	}

	want := []Message{
		{Role: "human", Text: "Please inspect the repo.", MsgID: "u1"},
		{Role: "assistant", Text: "I will inspect it now.", MsgID: "a1"},
		{Role: "assistant", Text: "[tool: Bash]", MsgID: "a2"},
		{Role: "human", Text: "[tool result omitted: 46 bytes]", MsgID: "u2"},
		{Role: "assistant", Text: "The ingest service is in brain/service/ingest.go.", MsgID: "a3"},
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

func TestClaudeCodeParser_DiscoverFindsJSONL(t *testing.T) {
	parser := ClaudeCodeParser{}
	root := filepath.Join("testdata", "claude-code")

	paths, err := parser.Discover(context.Background(), []string{root})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "basic.jsonl" {
		t.Fatalf("paths = %#v", paths)
	}
}
