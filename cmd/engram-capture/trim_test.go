package main

import (
	"strings"
	"testing"
	"time"
)

func TestBuildIngestBatch_TrimsLongMessagesAndSetsEnded(t *testing.T) {
	ts := time.Date(2026, 6, 13, 18, 55, 22, 0, time.UTC)
	tr := Transcript{
		Tool: "claude-code", SessionID: "s1", Title: "title", Project: "/repo",
		ModTime: time.Now().Add(-20 * time.Minute),
		Messages: []Message{
			{Role: "human", Text: "short", Ts: ts, MsgID: "m1"},
			{Role: "assistant", Text: strings.Repeat("x", 120), Ts: ts, MsgID: "m2"},
		},
	}

	batch := BuildIngestBatch(tr, TrimConfig{MaxMessageBytes: 40}, time.Now(), 10*time.Minute)

	if !batch.SessionEnded {
		t.Fatalf("expected session ended by age")
	}
	if batch.Messages[0].Role != "human" || batch.Messages[0].Text != "short" {
		t.Fatalf("first message changed: %#v", batch.Messages[0])
	}
	if batch.Messages[1].Text != "[large assistant message omitted: 120 bytes]" {
		t.Fatalf("trimmed text = %q", batch.Messages[1].Text)
	}
	if batch.Messages[0].Ts != "2026-06-13T18:55:22Z" {
		t.Fatalf("timestamp = %q", batch.Messages[0].Ts)
	}
}
