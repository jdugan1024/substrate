package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type StateStore struct {
	path    string
	Entries map[string]StateEntry `json:"entries"`
}

type StateEntry struct {
	MessageCount int       `json:"message_count"`
	ContentHash  string    `json:"content_hash"`
	LastPostedAt time.Time `json:"last_posted_at"`
}

func LoadState(path string) (*StateStore, error) {
	store := &StateStore{path: path, Entries: map[string]StateEntry{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(b, store); err != nil {
		return nil, err
	}
	store.path = path
	if store.Entries == nil {
		store.Entries = map[string]StateEntry{}
	}
	return store, nil
}

func (s *StateStore) ShouldSkip(batch IngestBatch) bool {
	entry, ok := s.Entries[stateKey(batch.Tool, batch.SessionID)]
	return ok && entry.MessageCount == len(batch.Messages) && entry.ContentHash == batchHash(batch)
}

func (s *StateStore) MarkPosted(batch IngestBatch) {
	s.Entries[stateKey(batch.Tool, batch.SessionID)] = StateEntry{
		MessageCount: len(batch.Messages),
		ContentHash:  batchHash(batch),
		LastPostedAt: time.Now().UTC(),
	}
}

func (s *StateStore) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

func stateKey(tool, sessionID string) string {
	return tool + "/" + sessionID
}

func batchHash(batch IngestBatch) string {
	h := sha256.New()
	for _, msg := range batch.Messages {
		h.Write([]byte(msg.Role))
		h.Write([]byte{0})
		h.Write([]byte(msg.MsgID))
		h.Write([]byte{0})
		h.Write([]byte(msg.Text))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
