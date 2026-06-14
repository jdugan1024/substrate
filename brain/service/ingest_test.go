package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func msgs(n int) []IngestMessage {
	out := make([]IngestMessage, n)
	for i := 0; i < n; i++ {
		role := "human"
		if i%2 == 1 {
			role = "assistant"
		}
		out[i] = IngestMessage{Role: role, Text: "message body here", MsgID: string(rune('a' + i))}
	}
	return out
}

func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("empty should be 0, got %d", got)
	}
	if got := estimateTokens("abcd"); got != 1 {
		t.Fatalf("4 chars should be ~1 token, got %d", got)
	}
}

func TestPackChunks_HoldsPartialTailWhenNotEnded(t *testing.T) {
	// budget tiny so each message is its own chunk; last one is held back.
	chunks, remainder := packChunks(msgs(3), 5, false)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 full chunks, got %d", len(chunks))
	}
	if len(remainder) != 1 {
		t.Fatalf("expected 1 held message, got %d", len(remainder))
	}
}

func TestPackChunks_FlushesTailWhenEnded(t *testing.T) {
	chunks, remainder := packChunks(msgs(3), 5, true)
	if len(remainder) != 0 {
		t.Fatalf("expected no remainder when ended, got %d", len(remainder))
	}
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != 3 {
		t.Fatalf("expected all 3 messages chunked, got %d", total)
	}
}

func TestPackChunks_Empty(t *testing.T) {
	chunks, remainder := packChunks(nil, 5, false)
	if len(chunks) != 0 || len(remainder) != 0 {
		t.Fatal("empty input should yield nothing")
	}
}

func TestShouldSummarize(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * time.Minute)
	recent := now.Add(-1 * time.Minute)

	if shouldSummarize(0, &recent, now, false, 6, 5*time.Minute) {
		t.Error("no new messages and not ended → false")
	}
	if !shouldSummarize(0, &recent, now, true, 6, 5*time.Minute) {
		t.Error("session ended → true")
	}
	if !shouldSummarize(3, nil, now, false, 6, 5*time.Minute) {
		t.Error("never summarized with new messages → true")
	}
	if !shouldSummarize(6, &recent, now, false, 6, 5*time.Minute) {
		t.Error("enough new messages → true")
	}
	if !shouldSummarize(1, &old, now, false, 6, 5*time.Minute) {
		t.Error("stale summary → true")
	}
	if shouldSummarize(1, &recent, now, false, 6, 5*time.Minute) {
		t.Error("few new + recent summary → false")
	}
}

func TestRenderTranscript(t *testing.T) {
	got := renderTranscript([]IngestMessage{
		{Role: "human", Text: "hi"},
		{Role: "assistant", Text: "hello"},
	})
	want := "human: hi\nassistant: hello"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestGenerateConversationSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{
					"content": `{"summary":"Discussed Go testing.","topics":["go","testing"],"decisions":["use TDD"],"preferences":[],"open_threads":["add CI"]}`,
				}},
			},
		})
	}))
	defer srv.Close()

	cs, err := generateConversationSummary(context.Background(), http.DefaultClient, srv.URL, "k", "human: how do I test?\nassistant: use TDD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cs.Summary != "Discussed Go testing." {
		t.Errorf("summary: got %q", cs.Summary)
	}
	if len(cs.Topics) != 2 || cs.Topics[0] != "go" {
		t.Errorf("topics: got %v", cs.Topics)
	}
	if len(cs.OpenThreads) != 1 || cs.OpenThreads[0] != "add CI" {
		t.Errorf("open_threads: got %v", cs.OpenThreads)
	}
}
