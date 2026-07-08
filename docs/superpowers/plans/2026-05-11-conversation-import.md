# Conversation Import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go CLI (`cmd/import-conversations`) that reads a Claude conversation export, generates missing summaries via OpenRouter, and stores each conversation as a searchable Engram entry, with idempotent re-run support via a `conversation_imports` tracking table.

**Architecture:** One `conversation_imports` table tracks UUID → entry ID → content hash so re-runs skip unchanged conversations and re-import changed ones. The CLI reuses `brain.App`, `service.EntryService`, and the existing repository pattern. Pure logic (hash, content formatting, summary generation) is extracted into `importer.go` for testability; `main.go` handles only flag parsing and app wiring.

**Tech Stack:** Go 1.25, module `open-brain-go`, `github.com/jackc/pgx/v5`, `crypto/sha256`, `encoding/json`, `net/http`, `flag`

---

## File Map

| File | Change |
|------|--------|
| `schema.sql` | Modify — add `conversation_imports` table after the `entries` block |
| `brain/repository/conversation_import.go` | Create — `GetConversationImport`, `InsertConversationImport`, `DeleteEntryByID` |
| `cmd/import-conversations/importer.go` | Create — types, `computeContentHash`, `buildContentText`, `generateSummary`, `loadConversations`, `runImport` |
| `cmd/import-conversations/importer_test.go` | Create — unit tests for `computeContentHash`, `buildContentText`, `generateSummary` |
| `cmd/import-conversations/main.go` | Create — flag parsing, `brain.App` init, entry point |

---

## Task 1: Schema — add conversation_imports table

**Files:**
- Modify: `schema.sql`

- [ ] **Step 1: Add the table definition**

Find the line `ALTER TABLE entries ENABLE ROW LEVEL SECURITY;` in `schema.sql`. Insert the following block immediately after the entries index/trigger block and before the RLS section (after the `-- Row Level Security` comment for entries, but keep it near the entries table for locality):

```sql
-- ---------------------------------------------------------------------------
-- Conversation Imports
-- Tracks Claude conversation export UUIDs that have been imported as entries.
-- content_hash is SHA256 of message UUIDs in order — re-runs skip unchanged
-- conversations and re-import changed ones.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS conversation_imports (
    conversation_uuid        TEXT        PRIMARY KEY,
    entry_id                 UUID        NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    conversation_updated_at  TIMESTAMPTZ,
    content_hash             TEXT        NOT NULL,
    imported_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- [ ] **Step 2: Commit**

```bash
git add schema.sql
git commit -m "feat: add conversation_imports tracking table"
```

---

## Task 2: Repository — conversation_import.go

**Files:**
- Create: `brain/repository/conversation_import.go`

- [ ] **Step 1: Create the file**

```go
// ABOUTME: Repository functions for the conversation_imports tracking table.
// ABOUTME: Used by cmd/import-conversations to implement idempotent re-runs.

package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConversationImportRecord is a row from conversation_imports.
type ConversationImportRecord struct {
	ConversationUUID       string
	EntryID                string
	ConversationUpdatedAt  *time.Time
	ContentHash            string
	ImportedAt             time.Time
}

// InsertConversationImportParams holds fields needed to write a tracking row.
type InsertConversationImportParams struct {
	ConversationUUID      string
	EntryID               string
	ConversationUpdatedAt *time.Time
	ContentHash           string
}

// GetConversationImport looks up a tracking record by conversation UUID.
// Returns nil (no error) if not found.
func GetConversationImport(ctx context.Context, pool *pgxpool.Pool, uuid string) (*ConversationImportRecord, error) {
	var r ConversationImportRecord
	err := pool.QueryRow(ctx, `
		SELECT conversation_uuid, entry_id::text, conversation_updated_at, content_hash, imported_at
		FROM conversation_imports
		WHERE conversation_uuid = $1
	`, uuid).Scan(&r.ConversationUUID, &r.EntryID, &r.ConversationUpdatedAt, &r.ContentHash, &r.ImportedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation import: %w", err)
	}
	return &r, nil
}

// InsertConversationImport records a successful conversation import.
// conversation_imports has no RLS so this can use the pool directly.
func InsertConversationImport(ctx context.Context, pool *pgxpool.Pool, p InsertConversationImportParams) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO conversation_imports (conversation_uuid, entry_id, conversation_updated_at, content_hash)
		VALUES ($1, $2::uuid, $3, $4)
	`, p.ConversationUUID, p.EntryID, p.ConversationUpdatedAt, p.ContentHash)
	if err != nil {
		return fmt.Errorf("insert conversation import: %w", err)
	}
	return nil
}

// DeleteEntryByID hard-deletes an entry row. Must be called inside a
// WithUserTx transaction so RLS permits the deletion. ON DELETE CASCADE
// automatically removes the corresponding conversation_imports row.
func DeleteEntryByID(ctx context.Context, tx pgx.Tx, entryID string) error {
	_, err := tx.Exec(ctx, "DELETE FROM entries WHERE id = $1::uuid", entryID)
	if err != nil {
		return fmt.Errorf("delete entry %s: %w", entryID, err)
	}
	return nil
}
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./brain/...
```

Expected: no output (clean compile).

- [ ] **Step 3: Commit**

```bash
git add brain/repository/conversation_import.go
git commit -m "feat: add conversation_imports repository functions"
```

---

## Task 3: Types and pure functions (TDD)

**Files:**
- Create: `cmd/import-conversations/importer_test.go`
- Create: `cmd/import-conversations/importer.go`

- [ ] **Step 1: Write failing tests for computeContentHash and buildContentText**

Create `cmd/import-conversations/importer_test.go`:

```go
package main

import (
	"testing"
)

func TestComputeContentHash_EmptyMessages(t *testing.T) {
	h := computeContentHash([]string{})
	if h == "" {
		t.Fatal("expected non-empty hash for empty slice")
	}
}

func TestComputeContentHash_Deterministic(t *testing.T) {
	uuids := []string{"aaa", "bbb", "ccc"}
	h1 := computeContentHash(uuids)
	h2 := computeContentHash(uuids)
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %q != %q", h1, h2)
	}
}

func TestComputeContentHash_OrderSensitive(t *testing.T) {
	h1 := computeContentHash([]string{"aaa", "bbb"})
	h2 := computeContentHash([]string{"bbb", "aaa"})
	if h1 == h2 {
		t.Fatal("hash should differ when UUID order differs")
	}
}

func TestComputeContentHash_NewMessageChangesHash(t *testing.T) {
	h1 := computeContentHash([]string{"aaa", "bbb"})
	h2 := computeContentHash([]string{"aaa", "bbb", "ccc"})
	if h1 == h2 {
		t.Fatal("adding a message UUID should change the hash")
	}
}

func TestBuildContentText(t *testing.T) {
	got := buildContentText("My Conversation", "2025-01-15", "A summary about Go.")
	want := "Conversation: My Conversation\nDate: 2025-01-15\nSummary: A summary about Go."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildContentText_FallbackToNameOnly(t *testing.T) {
	got := buildContentText("My Conversation", "2025-01-15", "")
	want := "Conversation: My Conversation\nDate: 2025-01-15"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
go test ./cmd/import-conversations/
```

Expected: `cannot find package` or compile error — file doesn't exist yet.

- [ ] **Step 3: Create importer.go with types and pure functions**

Create `cmd/import-conversations/importer.go`:

```go
// ABOUTME: Core import logic: conversation types, hash computation, content formatting.
// ABOUTME: generateSummary and runImport also live here.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"open-brain-go/brain"
	"open-brain-go/brain/repository"
	"open-brain-go/brain/service"
)

type message struct {
	UUID   string `json:"uuid"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

type conversation struct {
	UUID      string    `json:"uuid"`
	Name      string    `json:"name"`
	Summary   string    `json:"summary"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
	Messages  []message `json:"chat_messages"`
}

// computeContentHash returns a SHA256 hex string over all message UUIDs in order.
func computeContentHash(messageUUIDs []string) string {
	h := sha256.New()
	for _, id := range messageUUIDs {
		h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildContentText formats a conversation for EntryService.Capture.
// If summary is empty, omits the Summary line.
func buildContentText(name, createdAt, summary string) string {
	if summary == "" {
		return fmt.Sprintf("Conversation: %s\nDate: %s", name, createdAt)
	}
	return fmt.Sprintf("Conversation: %s\nDate: %s\nSummary: %s", name, createdAt, summary)
}
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
go test ./cmd/import-conversations/
```

Expected:
```
ok  	open-brain-go/cmd/import-conversations
```

- [ ] **Step 5: Commit**

```bash
git add cmd/import-conversations/importer.go cmd/import-conversations/importer_test.go
git commit -m "feat: add conversation types and pure helper functions"
```

---

## Task 4: Summary generation (TDD)

**Files:**
- Modify: `cmd/import-conversations/importer.go` — add `generateSummary`
- Modify: `cmd/import-conversations/importer_test.go` — add test

- [ ] **Step 1: Write the failing test**

Add to `cmd/import-conversations/importer_test.go`:

```go
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerateSummary_ReturnsSummaryText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "A conversation about Linux."}},
			},
		})
	}))
	defer srv.Close()

	conv := conversation{
		Name: "Linux discussion",
		Messages: []message{
			{Sender: "human", Text: "Does LineageOS remove Google?"},
			{Sender: "assistant", Text: "Yes, it does."},
			{Sender: "human", Text: "What about Pixel phones?"},
		},
	}

	summary, err := generateSummary(context.Background(), http.DefaultClient, srv.URL, "test-key", conv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "A conversation about Linux." {
		t.Fatalf("got %q, want %q", summary, "A conversation about Linux.")
	}
}

func TestGenerateSummary_OnlyIncludesHumanMessages(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "summary"}},
			},
		})
	}))
	defer srv.Close()

	conv := conversation{
		Name: "Test",
		Messages: []message{
			{Sender: "human", Text: "question one"},
			{Sender: "assistant", Text: "answer one"},
			{Sender: "human", Text: "question two"},
		},
	}

	_, _ = generateSummary(context.Background(), http.DefaultClient, srv.URL, "key", conv)

	messages, _ := capturedBody["messages"].([]any)
	userMsg, _ := messages[1].(map[string]any) // index 0 is system, index 1 is user
	content, _ := userMsg["content"].(string)
	if strings.Contains(content, "answer one") {
		t.Error("assistant messages should not appear in the prompt")
	}
	if !strings.Contains(content, "question one") || !strings.Contains(content, "question two") {
		t.Error("human messages should appear in the prompt")
	}
}
```

The test file header should be updated to include the `context` and `strings` imports. Replace the entire import block at the top of `importer_test.go` with:

```go
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)
```

- [ ] **Step 2: Run to confirm failure**

```bash
go test ./cmd/import-conversations/
```

Expected: compile error — `generateSummary` undefined.

- [ ] **Step 3: Add generateSummary to importer.go**

Add this function to `cmd/import-conversations/importer.go` (before the closing of the file):

```go
// generateSummary calls the OpenRouter chat completions endpoint to produce a
// 2-3 sentence summary of a conversation using only human-side messages.
// baseURL is the OpenRouter API base (e.g. "https://openrouter.ai/api/v1") —
// injectable for tests. Falls back to the conversation name on any error.
func generateSummary(ctx context.Context, client *http.Client, baseURL, openRouterKey string, conv conversation) (string, error) {
	var humanTexts []string
	for _, m := range conv.Messages {
		if m.Sender == "human" {
			humanTexts = append(humanTexts, m.Text)
			if len(humanTexts) >= 10 {
				break
			}
		}
	}

	prompt := strings.Join(humanTexts, "\n")
	if len(prompt) > 2000 {
		prompt = prompt[:2000]
	}

	body, _ := json.Marshal(map[string]any{
		"model": "openai/gpt-4o-mini",
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": "Given the following conversation transcript (human messages only), write a 2-3 sentence summary capturing the main topic, key questions asked, and any conclusions reached.",
			},
			{"role": "user", "content": fmt.Sprintf("Conversation: %s\nMessages:\n%s", conv.Name, prompt)},
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+openRouterKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("empty choices in response")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
go test ./cmd/import-conversations/
```

Expected:
```
ok  	open-brain-go/cmd/import-conversations
```

- [ ] **Step 5: Commit**

```bash
git add cmd/import-conversations/importer.go cmd/import-conversations/importer_test.go
git commit -m "feat: add summary generation via OpenRouter"
```

---

## Task 5: Import loop and main.go

**Files:**
- Modify: `cmd/import-conversations/importer.go` — add `loadConversations`, `runImport`
- Create: `cmd/import-conversations/main.go`

- [ ] **Step 1: Add loadConversations to importer.go**

Add to `cmd/import-conversations/importer.go`:

```go
// loadConversations reads and parses a Claude conversations.json export file.
func loadConversations(path string) ([]conversation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var convs []conversation
	if err := json.NewDecoder(f).Decode(&convs); err != nil {
		return nil, fmt.Errorf("decode conversations: %w", err)
	}
	return convs, nil
}
```

- [ ] **Step 2: Add runImport to importer.go**

Add to `cmd/import-conversations/importer.go`:

```go
const openRouterBase = "https://openrouter.ai/api/v1"

type importStats struct {
	skipped  int
	imported int
	updated  int
	failed   []string
}

// runImport processes all conversations in the export. userID must be a valid
// Engram user UUID; it is injected into ctx for RLS-scoped DB operations.
func runImport(ctx context.Context, app *brain.App, es *service.EntryService, userID, inputPath string, dryRun bool) error {
	ctx = context.WithValue(ctx, brain.CtxUserID, userID)

	convs, err := loadConversations(inputPath)
	if err != nil {
		return err
	}
	log.Printf("loaded %d conversations from %s", len(convs), inputPath)

	var stats importStats

	for _, conv := range convs {
		uuids := make([]string, len(conv.Messages))
		for i, m := range conv.Messages {
			uuids[i] = m.UUID
		}
		hash := computeContentHash(uuids)

		existing, err := repository.GetConversationImport(ctx, app.Pool, conv.UUID)
		if err != nil {
			log.Printf("WARN: lookup failed for %s (%s): %v", conv.UUID, conv.Name, err)
			stats.failed = append(stats.failed, conv.UUID)
			continue
		}

		if existing != nil && existing.ContentHash == hash {
			log.Printf("skip %s: unchanged", conv.Name)
			stats.skipped++
			continue
		}

		action := "import"
		if existing != nil {
			action = "update"
		}

		if dryRun {
			log.Printf("dry-run: would %s %q (uuid=%s)", action, conv.Name, conv.UUID)
			if action == "update" {
				stats.updated++
			} else {
				stats.imported++
			}
			continue
		}

		// Re-import: delete old entry first (CASCADE removes tracking row).
		if existing != nil {
			if err := app.WithUserTx(ctx, func(tx pgx.Tx) error {
				return repository.DeleteEntryByID(ctx, tx, existing.EntryID)
			}); err != nil {
				log.Printf("WARN: delete failed for %s: %v", conv.UUID, err)
				stats.failed = append(stats.failed, conv.UUID)
				continue
			}
		}

		// Build or generate summary.
		summary := conv.Summary
		if summary == "" {
			generated, err := generateSummary(ctx, http.DefaultClient, openRouterBase, app.OpenRouterKey, conv)
			if err != nil {
				log.Printf("WARN: summary generation failed for %q: %v — using name only", conv.Name, err)
			} else {
				summary = generated
			}
		}

		text := buildContentText(conv.Name, conv.CreatedAt, summary)

		result, err := es.Capture(ctx, text, "claude-export")
		if err != nil {
			log.Printf("WARN: capture failed for %q: %v", conv.Name, err)
			stats.failed = append(stats.failed, conv.UUID)
			continue
		}

		var updatedAt *time.Time
		if conv.UpdatedAt != "" {
			t, err := time.Parse(time.RFC3339, conv.UpdatedAt)
			if err == nil {
				updatedAt = &t
			}
		}

		if err := repository.InsertConversationImport(ctx, app.Pool, repository.InsertConversationImportParams{
			ConversationUUID:      conv.UUID,
			EntryID:               result.EntryID,
			ConversationUpdatedAt: updatedAt,
			ContentHash:           hash,
		}); err != nil {
			log.Printf("WARN: tracking insert failed for %q: %v", conv.Name, err)
			stats.failed = append(stats.failed, conv.UUID)
			continue
		}

		log.Printf("%s: %q → entry %s (%s)", action, conv.Name, result.EntryID, result.RecordType)
		if action == "update" {
			stats.updated++
		} else {
			stats.imported++
		}
	}

	log.Printf("\n=== Import complete ===")
	log.Printf("  imported: %d", stats.imported)
	log.Printf("  updated:  %d", stats.updated)
	log.Printf("  skipped:  %d", stats.skipped)
	log.Printf("  failed:   %d", len(stats.failed))
	if len(stats.failed) > 0 {
		log.Printf("  failed UUIDs: %s", strings.Join(stats.failed, ", "))
	}
	return nil
}
```

You also need to add `"github.com/jackc/pgx/v5"` and `"time"` to the imports in `importer.go`. The full import block for `importer.go` should be:

```go
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"open-brain-go/brain"
	"open-brain-go/brain/repository"
	"open-brain-go/brain/service"
)
```

- [ ] **Step 3: Create main.go**

Create `cmd/import-conversations/main.go`:

```go
// ABOUTME: CLI entry point for importing Claude conversation exports into Engram.
// ABOUTME: Usage: import-conversations --input conversations.json --user-id <uuid> [--dry-run]

package main

import (
	"context"
	"flag"
	"log"
	"os"

	"open-brain-go/brain"
	"open-brain-go/brain/service"
)

func main() {
	inputPath := flag.String("input", "", "Path to conversations.json (required)")
	userID := flag.String("user-id", "", "Engram user UUID to import as (required)")
	dryRun := flag.Bool("dry-run", false, "Log what would be imported without writing to DB")
	flag.Parse()

	if *inputPath == "" {
		log.Fatal("--input is required")
	}
	if *userID == "" {
		log.Fatal("--user-id is required")
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	openRouterKey := os.Getenv("OPENROUTER_API_KEY")
	if openRouterKey == "" {
		log.Fatal("OPENROUTER_API_KEY is required")
	}

	ctx := context.Background()

	app, err := brain.New(ctx, dbURL, openRouterKey)
	if err != nil {
		log.Fatalf("init app: %v", err)
	}
	defer app.Pool.Close()

	es := service.NewEntryService(app)

	if err := runImport(ctx, app, es, *userID, *inputPath, *dryRun); err != nil {
		log.Fatalf("import failed: %v", err)
	}
}
```

- [ ] **Step 4: Build to verify it compiles**

```bash
go build ./cmd/import-conversations/
```

Expected: produces `./import-conversations` binary with no errors.

- [ ] **Step 5: Run tests to confirm nothing is broken**

```bash
go test ./cmd/import-conversations/
```

Expected:
```
ok  	open-brain-go/cmd/import-conversations
```

- [ ] **Step 6: Commit**

```bash
git add cmd/import-conversations/importer.go cmd/import-conversations/main.go
git commit -m "feat: add import loop and CLI entry point"
```

---

## Task 6: Smoke test

- [ ] **Step 1: Dry-run to verify logic without DB writes**

```bash
sops exec-env secrets/engram.env \
  './import-conversations --input /home/jdugan/2026-05-11/conversations.json --user-id <your-engram-user-uuid> --dry-run'
```

Replace `<your-engram-user-uuid>` with your UUID from the `mcp_users` table (find it with `SELECT id, name FROM mcp_users;`).

Expected: 328 lines like `dry-run: would import "LineageOS and Google services removal" (uuid=...)`, followed by the summary showing `imported: 328, skipped: 0, failed: 0`.

- [ ] **Step 2: Confirm user UUID with a DB query**

If you don't know your UUID:
```bash
sops exec-env secrets/engram.env \
  'psql $DATABASE_URL -c "SELECT id, name FROM mcp_users;"'
```

- [ ] **Step 3: Run the real import**

```bash
sops exec-env secrets/engram.env \
  './import-conversations --input /home/jdugan/2026-05-11/conversations.json --user-id <your-engram-user-uuid>'
```

Expected: progress lines for each conversation, final summary showing ~282 imported immediately + ~46 with generated summaries. Total ~328 imported, 0 failed.

- [ ] **Step 4: Verify in Engram search**

Use the Engram MCP `search_thoughts` tool or the browse page to search for a topic from a known conversation (e.g. "LineageOS"). Confirm the entry appears with topics and people extracted.

- [ ] **Step 5: Re-run to verify idempotency**

```bash
sops exec-env secrets/engram.env \
  './import-conversations --input /home/jdugan/2026-05-11/conversations.json --user-id <your-engram-user-uuid>'
```

Expected final summary: `imported: 0, updated: 0, skipped: 328, failed: 0`.

- [ ] **Step 6: Clean up binary and commit**

```bash
rm ./import-conversations
git add -p  # confirm nothing unintended staged
git commit -m "feat: conversation import complete and smoke-tested" --allow-empty
```
