package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIngestClientPostsBearerBatch(t *testing.T) {
	var gotAuth string
	var gotBatch IngestBatch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/ingest" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
			t.Fatalf("decode: %v", err)
		}
		json.NewEncoder(w).Encode(IngestResult{ChunksCreated: 1, Summarized: true, MessageCount: 1})
	}))
	defer srv.Close()

	client := NewIngestClient(srv.URL, "substrate_pat_test", srv.Client())
	res, err := client.Post(context.Background(), IngestBatch{Tool: "claude-code", SessionID: "s1", Messages: []IngestMessage{{Role: "human", Text: "hello"}}})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotAuth != "Bearer substrate_pat_test" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBatch.SessionID != "s1" || res.MessageCount != 1 {
		t.Fatalf("batch/result mismatch: %#v %#v", gotBatch, res)
	}
}

func TestIngestClientUnauthorizedIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := NewIngestClient(srv.URL, "bad", srv.Client())
	_, err := client.Post(context.Background(), IngestBatch{Tool: "claude-code", SessionID: "s1"})
	if err == nil || !IsFatalAuthError(err) {
		t.Fatalf("expected fatal auth error, got %v", err)
	}
}
