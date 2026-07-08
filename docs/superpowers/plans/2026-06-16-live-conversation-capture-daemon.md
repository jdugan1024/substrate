# Live Conversation Capture Daemon Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `engram-capture`, a per-machine daemon that reads local LLM transcript files, normalizes them, trims noisy spans, and posts full transcripts to Engram's existing `POST /ingest` endpoint.

**Architecture:** The daemon is a thin local shipper. Parsers convert tool-specific stores into a common `Transcript`, the runner trims and converts that transcript to the `/ingest` wire shape, a local state file suppresses no-op POSTs, and the server remains authoritative for dedup, chunking, summarization, and embedding. The first working slice supports Claude Code JSONL end-to-end and creates explicit parser seams plus discovery commands for Codex, Zed, and Pi before implementing those formats.

**Tech Stack:** Go 1.25, standard `net/http`, `encoding/json`, `filepath.WalkDir`, `github.com/fsnotify/fsnotify` for watch mode, table-driven Go tests with fixtures.

---

## Scope

This plan implements a production-usable v1 for Claude Code plus the daemon foundation needed for every other source. It does not guess unverified local stores for Codex, Zed, or Pi from inside yolobox; this environment exposes `/home/yolo/.codex/history.jsonl` and SQLite state files, but not the user's host Zed or Pi stores. Those tools get concrete discovery tasks that produce fixtures before parser implementation.

Claude Desktop remains out of scope for live capture. The existing `conversation_imports` batch path handles Claude Desktop exports separately from `captured_sessions`.

## File Structure

| Path | Responsibility |
|------|----------------|
| `cmd/engram-capture/types.go` | Shared daemon structs: `Message`, `Transcript`, `IngestBatch`, parser interface, dry-run stats |
| `cmd/engram-capture/config.go` | Env/flag config, default paths, validation |
| `cmd/engram-capture/claude_code.go` | Claude Code JSONL parser and scanner |
| `cmd/engram-capture/trim.go` | Normalize roles/content and replace noisy spans with placeholders |
| `cmd/engram-capture/state.go` | JSON state store keyed by `tool/session_id`, content hash, message count |
| `cmd/engram-capture/client.go` | HTTP client for `POST /ingest`, 401 stop behavior, retryable status classification |
| `cmd/engram-capture/runner.go` | One-shot scan/backfill/dry-run orchestration |
| `cmd/engram-capture/watch.go` | `fsnotify` watcher with debounce and periodic full-scan fallback |
| `cmd/engram-capture/main.go` | CLI flags and process entry point |
| `cmd/engram-capture/testdata/claude-code/basic.jsonl` | Minimal Claude Code fixture with user, assistant text, tool use, and metadata records |
| `cmd/engram-capture/*_test.go` | Unit tests for each pure component |
| `go.mod`, `go.sum` | Add `github.com/fsnotify/fsnotify` |

## Implementation Tasks

### Task 1: Create Daemon Types And Fixture

**Files:**
- Create: `cmd/engram-capture/types.go`
- Create: `cmd/engram-capture/testdata/claude-code/basic.jsonl`
- Create: `cmd/engram-capture/types_test.go`

- [ ] **Step 1: Create the fixture**

Create `cmd/engram-capture/testdata/claude-code/basic.jsonl`:

```jsonl
{"type":"last-prompt","leafUuid":"ignore-me","sessionId":"abc-123"}
{"parentUuid":null,"type":"user","message":{"role":"user","content":"Please inspect the repo."},"uuid":"u1","timestamp":"2026-06-13T18:55:22.395Z","cwd":"/home/jdugan/engram","sessionId":"abc-123"}
{"parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I will inspect it now."}]},"uuid":"a1","timestamp":"2026-06-13T18:55:25.951Z","cwd":"/home/jdugan/engram","sessionId":"abc-123"}
{"parentUuid":"a1","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"rg --files"}}]},"uuid":"a2","timestamp":"2026-06-13T18:55:26.103Z","cwd":"/home/jdugan/engram","sessionId":"abc-123"}
{"parentUuid":"a2","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"main.go\nschema.sql\nbrain/service/ingest.go"}]},"uuid":"u2","timestamp":"2026-06-13T18:55:26.126Z","cwd":"/home/jdugan/engram","sessionId":"abc-123"}
{"parentUuid":"u2","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The ingest service is in brain/service/ingest.go."}]},"uuid":"a3","timestamp":"2026-06-13T18:55:29.296Z","cwd":"/home/jdugan/engram","sessionId":"abc-123"}
```

- [ ] **Step 2: Write the core types**

Create `cmd/engram-capture/types.go`:

```go
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
```

- [ ] **Step 3: Add a type conversion test**

Create `cmd/engram-capture/types_test.go`:

```go
package main

import (
	"testing"
	"time"
)

func TestMessageFieldsAreStableForWireConversion(t *testing.T) {
	ts := time.Date(2026, 6, 13, 18, 55, 22, 0, time.UTC)
	msg := Message{Role: "human", Text: "hello", Ts: ts, MsgID: "m1"}

	if msg.Role != "human" || msg.Text != "hello" || msg.MsgID != "m1" {
		t.Fatalf("message fields changed unexpectedly: %#v", msg)
	}
	if got := msg.Ts.Format(time.RFC3339Nano); got != "2026-06-13T18:55:22Z" {
		t.Fatalf("timestamp format mismatch: %s", got)
	}
}
```

- [ ] **Step 4: Run the initial test**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: package builds and the type test passes.

- [ ] **Step 5: Commit**

```bash
git add cmd/engram-capture
git commit -m "feat: scaffold capture daemon types"
```

### Task 2: Implement Claude Code Parser

**Files:**
- Create: `cmd/engram-capture/claude_code.go`
- Create: `cmd/engram-capture/claude_code_test.go`

- [ ] **Step 1: Write failing parser tests**

Create `cmd/engram-capture/claude_code_test.go`:

```go
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
```

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: FAIL because `ClaudeCodeParser` is undefined.

- [ ] **Step 3: Implement the parser**

Create `cmd/engram-capture/claude_code.go`:

```go
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ClaudeCodeParser struct{}

func (ClaudeCodeParser) Tool() string { return ToolClaudeCode }

func (ClaudeCodeParser) Discover(ctx context.Context, roots []string) ([]string, error) {
	var paths []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if d.IsDir() {
				return nil
			}
			if filepath.Ext(path) == ".jsonl" {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (p ClaudeCodeParser) ParseFile(ctx context.Context, path string) (Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return Transcript{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return Transcript{}, err
	}

	tr := Transcript{Tool: p.Tool(), Path: path, ModTime: st.ModTime()}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return Transcript{}, ctx.Err()
		default:
		}

		var rec claudeCodeRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if tr.SessionID == "" {
			tr.SessionID = rec.SessionID
		}
		if tr.Project == "" {
			tr.Project = rec.CWD
		}

		msg, ok := rec.message()
		if !ok {
			continue
		}
		if tr.Title == "" && msg.Role == "human" {
			tr.Title = firstLine(msg.Text)
		}
		tr.Messages = append(tr.Messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return Transcript{}, err
	}
	if tr.SessionID == "" {
		tr.SessionID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if tr.Title == "" {
		tr.Title = tr.SessionID
	}
	return tr, nil
}

type claudeCodeRecord struct {
	Type      string          `json:"type"`
	UUID      string          `json:"uuid"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"sessionId"`
	CWD       string          `json:"cwd"`
	Message   json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeContentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
}

func (r claudeCodeRecord) message() (Message, bool) {
	if r.Type != "user" && r.Type != "assistant" {
		return Message{}, false
	}
	var cm claudeMessage
	if err := json.Unmarshal(r.Message, &cm); err != nil {
		return Message{}, false
	}
	role := normalizeRole(cm.Role)
	if role == "" {
		return Message{}, false
	}
	text := renderClaudeContent(cm.Content)
	if strings.TrimSpace(text) == "" {
		return Message{}, false
	}
	ts, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
	return Message{Role: role, Text: text, Ts: ts, MsgID: r.UUID}, true
}

func normalizeRole(role string) string {
	switch role {
	case "user", "human":
		return "human"
	case "assistant":
		return "assistant"
	default:
		return ""
	}
}

func renderClaudeContent(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}

	var blocks []claudeContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if txt := strings.TrimSpace(block.Text); txt != "" {
				parts = append(parts, txt)
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name == "" {
				name = "unknown"
			}
			parts = append(parts, fmt.Sprintf("[tool: %s]", name))
		case "tool_result":
			parts = append(parts, fmt.Sprintf("[tool result omitted: %d bytes]", len(block.Content)))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func firstLine(s string) string {
	line := strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(line) > 80 {
		return line[:80]
	}
	return line
}
```

- [ ] **Step 4: Run parser tests**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/engram-capture/claude_code.go cmd/engram-capture/claude_code_test.go cmd/engram-capture/testdata/claude-code/basic.jsonl
git commit -m "feat: parse Claude Code transcripts"
```

### Task 3: Add Trimming And Wire Conversion

**Files:**
- Create: `cmd/engram-capture/trim.go`
- Create: `cmd/engram-capture/trim_test.go`

- [ ] **Step 1: Write trimming tests**

Create `cmd/engram-capture/trim_test.go`:

```go
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
```

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: FAIL because `BuildIngestBatch` and `TrimConfig` are undefined.

- [ ] **Step 3: Implement trimming**

Create `cmd/engram-capture/trim.go`:

```go
package main

import (
	"fmt"
	"strings"
	"time"
)

type TrimConfig struct {
	MaxMessageBytes int
}

func DefaultTrimConfig() TrimConfig {
	return TrimConfig{MaxMessageBytes: 64 * 1024}
}

func BuildIngestBatch(tr Transcript, cfg TrimConfig, now time.Time, endedAfter time.Duration) IngestBatch {
	msgs := make([]IngestMessage, 0, len(tr.Messages))
	for _, msg := range tr.Messages {
		text := trimMessage(msg, cfg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		msgs = append(msgs, IngestMessage{
			Role:  msg.Role,
			Text:  text,
			Ts:    msg.Ts.Format(time.RFC3339Nano),
			MsgID: msg.MsgID,
		})
	}
	return IngestBatch{
		Tool:         tr.Tool,
		SessionID:    tr.SessionID,
		Title:        tr.Title,
		Project:      tr.Project,
		Messages:     msgs,
		SessionEnded: !tr.ModTime.IsZero() && now.Sub(tr.ModTime) >= endedAfter,
	}
}

func trimMessage(msg Message, cfg TrimConfig) string {
	text := strings.TrimSpace(msg.Text)
	if cfg.MaxMessageBytes <= 0 || len(text) <= cfg.MaxMessageBytes {
		return text
	}
	return fmt.Sprintf("[large %s message omitted: %d bytes]", msg.Role, len(text))
}
```

- [ ] **Step 4: Run trimming tests**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/engram-capture/trim.go cmd/engram-capture/trim_test.go
git commit -m "feat: convert transcripts to ingest batches"
```

### Task 4: Add Local State Store

**Files:**
- Create: `cmd/engram-capture/state.go`
- Create: `cmd/engram-capture/state_test.go`

- [ ] **Step 1: Write state tests**

Create `cmd/engram-capture/state_test.go`:

```go
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
```

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: FAIL because `LoadState` is undefined.

- [ ] **Step 3: Implement state**

Create `cmd/engram-capture/state.go`:

```go
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
	path     string
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
```

- [ ] **Step 4: Run state tests**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/engram-capture/state.go cmd/engram-capture/state_test.go
git commit -m "feat: track capture daemon post state"
```

### Task 5: Add HTTP Ingest Client

**Files:**
- Create: `cmd/engram-capture/client.go`
- Create: `cmd/engram-capture/client_test.go`

- [ ] **Step 1: Write client tests**

Create `cmd/engram-capture/client_test.go`:

```go
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

	client := NewIngestClient(srv.URL, "engram_pat_test", srv.Client())
	res, err := client.Post(context.Background(), IngestBatch{Tool: "claude-code", SessionID: "s1", Messages: []IngestMessage{{Role: "human", Text: "hello"}}})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotAuth != "Bearer engram_pat_test" {
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
```

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: FAIL because `NewIngestClient` is undefined.

- [ ] **Step 3: Implement client**

Create `cmd/engram-capture/client.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type IngestClient struct {
	baseURL string
	pat     string
	http    *http.Client
}

type fatalAuthError struct{ status int }

func (e fatalAuthError) Error() string { return fmt.Sprintf("ingest auth failed: HTTP %d", e.status) }

func IsFatalAuthError(err error) bool {
	_, ok := err.(fatalAuthError)
	return ok
}

func NewIngestClient(baseURL, pat string, hc *http.Client) *IngestClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &IngestClient{baseURL: strings.TrimRight(baseURL, "/"), pat: pat, http: hc}
}

func (c *IngestClient) Post(ctx context.Context, batch IngestBatch) (IngestResult, error) {
	var result IngestResult
	body, err := json.Marshal(batch)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ingest", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, resp.Body)
		return result, fatalAuthError{status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return result, fmt.Errorf("ingest failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, err
	}
	return result, nil
}
```

- [ ] **Step 4: Run client tests**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/engram-capture/client.go cmd/engram-capture/client_test.go
git commit -m "feat: post capture batches to ingest"
```

### Task 6: Add Config And Runner

**Files:**
- Create: `cmd/engram-capture/config.go`
- Create: `cmd/engram-capture/runner.go`
- Create: `cmd/engram-capture/runner_test.go`

- [ ] **Step 1: Write runner tests**

Create `cmd/engram-capture/runner_test.go`:

```go
package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type recordingPoster struct{ batches []IngestBatch }

func (p *recordingPoster) Post(ctx context.Context, batch IngestBatch) (IngestResult, error) {
	p.batches = append(p.batches, batch)
	return IngestResult{MessageCount: len(batch.Messages)}, nil
}

func TestRunnerDryRunDoesNotPost(t *testing.T) {
	parser := ClaudeCodeParser{}
	state, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	poster := &recordingPoster{}
	runner := Runner{
		Parsers:    []Parser{parser},
		Roots:      map[string][]string{ToolClaudeCode: []string{filepath.Join("testdata", "claude-code")}},
		State:      state,
		Poster:     poster,
		Trim:       TrimConfig{MaxMessageBytes: 1024},
		EndedAfter: time.Hour,
		Now:        func() time.Time { return time.Date(2026, 6, 13, 19, 0, 0, 0, time.UTC) },
		DryRun:     true,
	}

	stats, err := runner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if stats.Sessions != 1 || stats.Messages != 5 || stats.WouldPost != 1 {
		t.Fatalf("stats = %#v", stats)
	}
	if len(poster.batches) != 0 {
		t.Fatalf("dry run posted %d batches", len(poster.batches))
	}
}

func TestRunnerSkipsNoOpAfterPost(t *testing.T) {
	state, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	poster := &recordingPoster{}
	runner := Runner{
		Parsers:    []Parser{ClaudeCodeParser{}},
		Roots:      map[string][]string{ToolClaudeCode: []string{filepath.Join("testdata", "claude-code")}},
		State:      state,
		Poster:     poster,
		Trim:       TrimConfig{MaxMessageBytes: 1024},
		EndedAfter: time.Hour,
		Now:        time.Now,
	}

	if _, err := runner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if _, err := runner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(poster.batches) != 1 {
		t.Fatalf("posted batches = %d", len(poster.batches))
	}
}
```

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: FAIL because `Runner` is undefined.

- [ ] **Step 3: Implement config and runner**

Create `cmd/engram-capture/config.go`:

```go
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	BaseURL       string
	PAT           string
	StatePath     string
	ClaudeRoots   []string
	SweepInterval time.Duration
	Debounce      time.Duration
	EndedAfter    time.Duration
	Trim          TrimConfig
	DryRun        bool
	Backfill      bool
	Watch         bool
}

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		BaseURL:       envDefault("ENGRAM_URL", "https://engram.x1024.net"),
		PAT:           os.Getenv("ENGRAM_PAT"),
		StatePath:     filepath.Join(home, ".local", "state", "engram-capture", "state.json"),
		ClaudeRoots:   []string{filepath.Join(home, ".claude", "projects")},
		SweepInterval: 30 * time.Second,
		Debounce:      2 * time.Second,
		EndedAfter:    10 * time.Minute,
		Trim:          DefaultTrimConfig(),
	}
}

func (c Config) Validate() error {
	if !c.DryRun && strings.TrimSpace(c.PAT) == "" {
		return errors.New("ENGRAM_PAT is required unless --dry-run is set")
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("server URL is required")
	}
	if len(c.ClaudeRoots) == 0 {
		return errors.New("at least one Claude Code root is required")
	}
	return nil
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

Create `cmd/engram-capture/runner.go`:

```go
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
			batch := BuildIngestBatch(tr, r.Trim, now(), r.EndedAfter)
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
```

- [ ] **Step 4: Run runner tests**

Run:

```bash
go test ./cmd/engram-capture/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/engram-capture/config.go cmd/engram-capture/runner.go cmd/engram-capture/runner_test.go
git commit -m "feat: orchestrate capture scans"
```

### Task 7: Add CLI Entry Point

**Files:**
- Create: `cmd/engram-capture/main.go`

- [ ] **Step 1: Implement `main.go`**

Create `cmd/engram-capture/main.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	cfg := DefaultConfig()

	var claudeRoots string
	flag.StringVar(&cfg.BaseURL, "url", cfg.BaseURL, "Engram base URL")
	flag.StringVar(&cfg.StatePath, "state", cfg.StatePath, "local state JSON path")
	flag.StringVar(&claudeRoots, "claude-root", strings.Join(cfg.ClaudeRoots, string(os.PathListSeparator)), "Claude Code transcript root(s), path-list separated")
	flag.DurationVar(&cfg.SweepInterval, "sweep-interval", cfg.SweepInterval, "periodic scan interval")
	flag.DurationVar(&cfg.Debounce, "debounce", cfg.Debounce, "watch debounce interval")
	flag.DurationVar(&cfg.EndedAfter, "ended-after", cfg.EndedAfter, "mark sessions ended after file idle duration")
	flag.IntVar(&cfg.Trim.MaxMessageBytes, "max-message-bytes", cfg.Trim.MaxMessageBytes, "replace messages larger than this byte count")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "scan and report without posting")
	flag.BoolVar(&cfg.Backfill, "backfill", false, "scan once and exit")
	flag.BoolVar(&cfg.Watch, "watch", false, "watch for changes after initial scan")
	flag.Parse()

	cfg.ClaudeRoots = filepathList(claudeRoots)
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	runner := Runner{
		Parsers:    []Parser{ClaudeCodeParser{}},
		Roots:      map[string][]string{ToolClaudeCode: cfg.ClaudeRoots},
		State:      state,
		Poster:     NewIngestClient(cfg.BaseURL, cfg.PAT, &http.Client{Timeout: 30 * time.Second}),
		Trim:       cfg.Trim,
		EndedAfter: cfg.EndedAfter,
		Now:        time.Now,
		DryRun:     cfg.DryRun,
	}

	ctx := context.Background()
	stats, err := runner.ScanOnce(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("scan: sessions=%d messages=%d would_post=%d skipped_noop=%d parse_failures=%d estimated_bytes=%d\n",
		stats.Sessions, stats.Messages, stats.WouldPost, stats.SkippedNoOp, stats.ParseFailures, stats.EstimatedBytes)

	if cfg.Watch && !cfg.Backfill {
		if err := Watch(ctx, cfg, runner); err != nil {
			log.Fatal(err)
		}
	}
}

func filepathList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, string(os.PathListSeparator)) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
```

- [ ] **Step 2: Run build and observe the expected missing watcher**

Run:

```bash
go build ./cmd/engram-capture/
```

Expected: FAIL because `Watch` is undefined. This confirms the CLI is wired to the watcher task.

- [ ] **Step 3: Commit after watcher task instead of now**

Do not commit this task yet; Task 8 completes the buildable unit.

### Task 8: Add Watch Mode

**Files:**
- Create: `cmd/engram-capture/watch.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Add fsnotify dependency**

Run:

```bash
go get github.com/fsnotify/fsnotify@latest
```

Expected: `go.mod` and `go.sum` update.

- [ ] **Step 2: Implement watch mode**

Create `cmd/engram-capture/watch.go`:

```go
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

func Watch(ctx context.Context, cfg Config, runner Runner) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	for _, root := range cfg.ClaudeRoots {
		if err := addRecursiveWatches(watcher, root); err != nil {
			log.Printf("watch add failed: path=%s err=%v", root, err)
		}
	}

	scanNow := make(chan struct{}, 1)
	trigger := func() {
		select {
		case scanNow <- struct{}{}:
		default:
		}
	}

	ticker := time.NewTicker(cfg.SweepInterval)
	defer ticker.Stop()

	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-watcher.Events:
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := addRecursiveWatches(watcher, event.Name); err != nil {
						log.Printf("watch add failed: path=%s err=%v", event.Name, err)
					}
				}
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) || event.Has(fsnotify.Rename) {
				debounce = time.After(cfg.Debounce)
			}
		case err := <-watcher.Errors:
			log.Printf("watch error: %v", err)
		case <-debounce:
			debounce = nil
			trigger()
		case <-ticker.C:
			trigger()
		case <-scanNow:
			stats, err := runner.ScanOnce(ctx)
			if err != nil {
				if IsFatalAuthError(err) {
					return err
				}
				log.Printf("scan failed: %v", err)
				continue
			}
			log.Printf("scan complete: sessions=%d messages=%d posted=%d skipped_noop=%d parse_failures=%d",
				stats.Sessions, stats.Messages, stats.WouldPost, stats.SkippedNoOp, stats.ParseFailures)
		}
	}
}

func addRecursiveWatches(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if err := watcher.Add(path); err != nil {
				log.Printf("watch add skipped: path=%s err=%v", path, err)
			}
		}
		return nil
	})
}
```

- [ ] **Step 3: Fix missing import in `main.go`**

Modify `cmd/engram-capture/main.go` to include `path/filepath` only if the implementation uses it. The code above does not use it; no import is needed.

- [ ] **Step 4: Run all daemon tests and build**

Run:

```bash
go test ./cmd/engram-capture/
go build ./cmd/engram-capture/
```

Expected: both pass.

- [ ] **Step 5: Commit CLI and watcher**

```bash
git add go.mod go.sum cmd/engram-capture/main.go cmd/engram-capture/watch.go
git commit -m "feat: add capture daemon cli and watch mode"
```

### Task 9: Add Real Dry-Run Verification

**Files:**
- No source changes expected unless dry-run exposes a bug.

- [ ] **Step 1: Run dry-run against visible Claude Code transcripts**

Run from this yolobox environment:

```bash
go run ./cmd/engram-capture --dry-run --backfill --claude-root /home/yolo/.claude/projects
```

Expected: output like:

```text
scan: sessions=<nonzero> messages=<nonzero> would_post=<nonzero> skipped_noop=0 parse_failures=0 estimated_bytes=<nonzero>
```

- [ ] **Step 2: Run the same dry-run again**

Run:

```bash
go run ./cmd/engram-capture --dry-run --backfill --claude-root /home/yolo/.claude/projects
```

Expected: dry-run does not persist post state, so `would_post` remains nonzero. This is intentional because no POST occurred.

- [ ] **Step 3: Run unit tests for the whole repo**

Run:

```bash
go test ./...
```

Expected: PASS.

### Task 10: Codex/Zed/Pi Parser Discovery

**Files:**
- Create: `docs/superpowers/plans/2026-06-16-live-conversation-capture-parser-discovery.md`

- [ ] **Step 1: Record Codex local evidence**

Create `docs/superpowers/plans/2026-06-16-live-conversation-capture-parser-discovery.md` with:

```markdown
# Live Capture Parser Discovery Notes

## Codex

Observed in yolobox:

- `/home/yolo/.codex/history.jsonl` contains prompt history records with `session_id`, `ts`, and `text`.
- `/home/yolo/.codex/state_5.sqlite`, `goals_1.sqlite`, `logs_2.sqlite`, and `memories_1.sqlite` exist.
- `sqlite3` was not installed in the yolobox image during planning, so table schemas were not inspected.

Next host-side command:

```bash
sqlite3 ~/.codex/state_5.sqlite '.tables'
sqlite3 ~/.codex/logs_2.sqlite '.tables'
sqlite3 ~/.codex/state_5.sqlite '.schema'
sqlite3 ~/.codex/logs_2.sqlite '.schema'
```

Acceptance criterion for a Codex parser:

- It must emit both human and assistant messages for a session.
- If only prompt history is available, do not enable the parser by default because it would create misleading one-sided transcripts.

## Zed

No Zed store was visible inside yolobox. Run on the host:

```bash
find ~/.config ~/.local/share -maxdepth 5 \( -iname '*zed*' -o -path '*zed*' \) 2>/dev/null
```

Acceptance criterion for a Zed parser:

- Prefer SQLite or JSON stores over UI cache files.
- Add a fixture copied from a single redacted thread before writing parser code.

## Pi

No Pi store was visible inside yolobox. Run on the host:

```bash
find ~ -maxdepth 6 \( -iname '*pi*' -o -path '*pi*' \) 2>/dev/null
```

Acceptance criterion for a Pi parser:

- Identify a durable local transcript source containing user and assistant text.
- Add a redacted fixture before writing parser code.
```

- [ ] **Step 2: Commit discovery notes**

```bash
git add docs/superpowers/plans/2026-06-16-live-conversation-capture-parser-discovery.md
git commit -m "docs: record live capture parser discovery"
```

## Verification Before Completion

Run:

```bash
go test ./cmd/engram-capture/
go test ./...
go run ./cmd/engram-capture --dry-run --backfill --claude-root /home/yolo/.claude/projects
```

Expected:

- Daemon package tests pass.
- Full repo tests pass.
- Dry-run reports nonzero Claude Code sessions/messages in this yolobox environment.

## Execution Notes

- Use a real `ENGRAM_PAT` only after the production server deploy from Task 1 of the handoff is complete.
- For a first real run, use:

```bash
ENGRAM_PAT='engram_pat_...' go run ./cmd/engram-capture --backfill --claude-root ~/.claude/projects
```

- For ongoing capture after backfill:

```bash
ENGRAM_PAT='engram_pat_...' go run ./cmd/engram-capture --watch --claude-root ~/.claude/projects
```

- `--dry-run` never writes local state because no POST was accepted by the server.
- The daemon sends full trimmed transcripts. The local state file only suppresses no-op POSTs; deleting it is safe because server-side `captured_sessions.chunked_msg_count` remains authoritative.

## Self-Review

- **Spec coverage:** Implements daemon shell, Claude Code parser, trimming/placeholders, local cursor state, POST loop, backfill/dry-run, ended-by-age, and watch mode. Additional parsers are gated by discovery notes because their stores were not available in yolobox.
- **Placeholder scan:** No unresolved marker items remain. Unknown parser formats are handled as explicit discovery tasks with commands and acceptance criteria.
- **Type consistency:** `Message`, `Transcript`, `IngestBatch`, `IngestMessage`, `StateStore`, `Runner`, `Poster`, and `ClaudeCodeParser` names are consistent across tasks.
