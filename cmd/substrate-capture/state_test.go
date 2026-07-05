package main

import (
	"path/filepath"
	"testing"
)

func TestStateStoreDetectsNoOpAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	batch := IngestBatch{Tool: "claude-code", SessionID: "s1", Messages: []IngestMessage{
		{Role: "human", Text: "hello", MsgID: "m1"},
	}}
	if store.ShouldSkip(batch) {
		t.Fatalf("fresh batch should not skip")
	}
	store.MarkPosted(batch)
	if !store.ShouldSkip(batch) {
		t.Fatalf("unchanged batch should skip")
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.ShouldSkip(batch) {
		t.Fatalf("reloaded unchanged batch should skip")
	}
}

func TestStateStoreDoesNotSkipWhenSourceMetadataChanges(t *testing.T) {
	store, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	batch := IngestBatch{Tool: "claude-code", SessionID: "s1", Project: "/repo", Machine: "laptop", Username: "jdugan", Messages: []IngestMessage{
		{Role: "human", Text: "hello", MsgID: "m1"},
	}}
	store.MarkPosted(batch)

	changed := batch
	changed.Machine = "desktop"
	if store.ShouldSkip(changed) {
		t.Fatalf("metadata-only changes must not be skipped")
	}
}
