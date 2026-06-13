# Live Conversation Capture — Server Side Implementation Plan (Part 1 of 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Personal Access Token (PAT) auth and a `POST /ingest` endpoint to engram that accepts a normalized conversation transcript, stores it as append-only raw chunk entries plus one upserted per-session distilled summary entry, with idempotent re-runs.

**Architecture:** A new `api_tokens` table + a PAT branch in the existing `authMiddleware` lets a headless client authenticate with a long-lived bearer token. A new `captured_sessions` table tracks, per `(user, tool, session_id)`, how many messages have been folded into raw chunks and which entry holds the distilled summary. A new `IngestService` (sibling to `EntryService`) does the work: it dedups against the chunk count, packs new messages into token-budgeted `conversation.chunk` entries, and (throttled) regenerates a single `conversation.summary` entry. All pure logic (chunk packing, token estimate, throttle decision, transcript rendering, summary JSON parsing) is unit-tested; DB-backed paths are covered by a final smoke test, matching the existing codebase's test style.

**Tech Stack:** Go 1.25, module `open-brain-go`, `github.com/jackc/pgx/v5`, `github.com/pgvector/pgvector-go`, `crypto/sha256`, `crypto/rand`, `net/http`, OpenRouter (embeddings + chat completions).

**Refinements from the design spec** (deliberate, for implementability):
- The capture daemon (Part 2) sends the **full trimmed transcript** of a session each sweep, not just new messages. The server stays authoritative for dedup and the summary always has the whole conversation available. Bandwidth cost is modest for trimmed text; delta-shipping can be added later.
- Dedup uses an integer **`chunked_msg_count`** (messages already folded into emitted chunks) instead of the spec's `high_water_msg_id`. Under the full-transcript model the transcript is append-only, so an index is stable and avoids "msg id not found" edge cases.

---

## File Map

| File | Change |
|------|--------|
| `schema.sql` | Modify — add `api_tokens` and `captured_sessions` tables |
| `api_tokens.go` | Create (package `main`) — token generate/hash helpers, web JSON handlers for create/list/revoke |
| `api_tokens_test.go` | Create (package `main`) — tests for generate/hash helpers |
| `main.go` | Modify — PAT branch in `authMiddleware`; register `/ingest` and token routes |
| `web/tokens.html` | Create — minimal token-management page |
| `brain/repository/api_token.go` | Create — `InsertAPIToken`, `GetUserIDByTokenHash`, `TouchAPIToken`, `ListAPITokens`, `RevokeAPIToken` |
| `brain/repository/captured_session.go` | Create — `GetCapturedSession`, `UpsertCapturedSession` |
| `brain/repository/entry.go` | Modify — add `UpdateEntryContent` |
| `brain/service/ingest.go` | Create — `IngestService`, batch types, pure helpers, summary generation |
| `brain/service/ingest_test.go` | Create — unit tests for pure helpers + summary generation |
| `ingest_handler.go` | Create (package `main`) — `POST /ingest` HTTP handler |

---

## Task 1: Schema — api_tokens and captured_sessions tables

**Files:**
- Modify: `schema.sql`

- [ ] **Step 1: Add the api_tokens table**

In `schema.sql`, immediately after the `mcp_users` table block (after the closing `);` of `CREATE TABLE IF NOT EXISTS mcp_users` and its comment, before the Thoughts section), insert:

```sql
-- ---------------------------------------------------------------------------
-- API Tokens
-- Long-lived personal access tokens for headless clients (e.g. the capture
-- daemon). Looked up by SHA-256 hash at auth time, before any user context
-- exists, so this table has NO row-level security (like mcp_users). Handlers
-- that create/list/revoke tokens filter by user_id explicitly.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_tokens (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens (user_id);
```

- [ ] **Step 2: Add the captured_sessions table**

In `schema.sql`, after the entries table's RLS policy and trigger block (after the `entries_updated_at` trigger, at the end of the file), append:

```sql
-- ---------------------------------------------------------------------------
-- Captured Sessions
-- Tracks live-captured LLM conversations per (user, tool, session_id).
-- chunked_msg_count = how many transcript messages have been folded into
-- emitted raw chunk entries (dedup high-water mark). summary_entry_id points
-- at the single upserted conversation.summary entry for the session.
-- Separate from conversation_imports (batch Claude Desktop export path).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS captured_sessions (
    user_id            UUID        NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
    tool               TEXT        NOT NULL,
    session_id         TEXT        NOT NULL,
    summary_entry_id   UUID        REFERENCES entries(id) ON DELETE SET NULL,
    chunked_msg_count  INT         NOT NULL DEFAULT 0,
    message_count      INT         NOT NULL DEFAULT 0,
    session_started_at TIMESTAMPTZ,
    session_ended_at   TIMESTAMPTZ,
    last_summarized_at TIMESTAMPTZ,
    last_ingested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, tool, session_id)
);

ALTER TABLE captured_sessions ENABLE ROW LEVEL SECURITY;

CREATE POLICY captured_sessions_user_isolation ON captured_sessions
    FOR ALL
    USING (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    );
```

- [ ] **Step 3: Commit**

```bash
git add schema.sql
git commit -m "feat: add api_tokens and captured_sessions tables"
```

---

## Task 2: Token generate/hash helpers (TDD)

**Files:**
- Create: `api_tokens_test.go`
- Create: `api_tokens.go`

- [ ] **Step 1: Write the failing test**

Create `api_tokens_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestGenerateAPIToken_Format(t *testing.T) {
	plaintext, hash := generateAPIToken()
	if !strings.HasPrefix(plaintext, tokenPrefix) {
		t.Fatalf("plaintext missing prefix %q: %q", tokenPrefix, plaintext)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if hash == plaintext {
		t.Fatal("hash must differ from plaintext")
	}
}

func TestGenerateAPIToken_Unique(t *testing.T) {
	p1, h1 := generateAPIToken()
	p2, h2 := generateAPIToken()
	if p1 == p2 || h1 == h2 {
		t.Fatal("expected distinct tokens and hashes")
	}
}

func TestHashAPIToken_MatchesGenerated(t *testing.T) {
	plaintext, hash := generateAPIToken()
	if got := hashAPIToken(plaintext); got != hash {
		t.Fatalf("hashAPIToken(plaintext)=%q, want %q", got, hash)
	}
}

func TestHashAPIToken_Deterministic(t *testing.T) {
	if hashAPIToken("engram_pat_abc") != hashAPIToken("engram_pat_abc") {
		t.Fatal("hash should be deterministic")
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test . -run TestGenerateAPIToken -v`
Expected: compile error — `tokenPrefix`, `generateAPIToken`, `hashAPIToken` undefined.

- [ ] **Step 3: Create api_tokens.go with the helpers**

Create `api_tokens.go`:

```go
// ABOUTME: Personal access token (PAT) helpers and web management handlers.
// ABOUTME: Tokens authenticate headless clients (e.g. the capture daemon) to /ingest.

package main

import (
	"crypto/sha256"
	"encoding/hex"
)

// tokenPrefix marks a bearer token as a PAT so authMiddleware can route it to
// the PAT lookup instead of OIDC verification.
const tokenPrefix = "engram_pat_"

// generateAPIToken returns a new opaque token (to show the user once) and its
// SHA-256 hex hash (to store).
func generateAPIToken() (plaintext, hash string) {
	plaintext = tokenPrefix + randomHex(32) // randomHex is defined in web_auth.go
	return plaintext, hashAPIToken(plaintext)
}

// hashAPIToken returns the SHA-256 hex hash of a token's plaintext.
func hashAPIToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run to confirm pass**

Run: `go test . -run "TestGenerateAPIToken|TestHashAPIToken" -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add api_tokens.go api_tokens_test.go
git commit -m "feat: add PAT generate/hash helpers"
```

---

## Task 3: api_tokens repository

**Files:**
- Create: `brain/repository/api_token.go`

- [ ] **Step 1: Create the repository file**

Create `brain/repository/api_token.go`:

```go
// ABOUTME: Repository functions for the api_tokens table (personal access tokens).
// ABOUTME: api_tokens has no RLS, so user-scoped queries filter by user_id explicitly.

package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// APIToken is a row from api_tokens (never exposes the plaintext token).
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// InsertAPIToken records a new token hash for a user and returns its id.
func InsertAPIToken(ctx context.Context, pool *pgxpool.Pool, userID, name, tokenHash string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO api_tokens (user_id, name, token_hash)
		VALUES ($1::uuid, $2, $3)
		RETURNING id::text
	`, userID, name, tokenHash).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert api token: %w", err)
	}
	return id, nil
}

// GetUserIDByTokenHash returns the owning user's id for a live (non-revoked)
// token hash. Returns ("", nil) if no matching live token exists.
func GetUserIDByTokenHash(ctx context.Context, pool *pgxpool.Pool, tokenHash string) (string, error) {
	var userID string
	err := pool.QueryRow(ctx, `
		SELECT user_id::text FROM api_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash).Scan(&userID)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get user by token hash: %w", err)
	}
	return userID, nil
}

// TouchAPIToken updates last_used_at to now for a token hash (best-effort).
func TouchAPIToken(ctx context.Context, pool *pgxpool.Pool, tokenHash string) error {
	_, err := pool.Exec(ctx, `
		UPDATE api_tokens SET last_used_at = now() WHERE token_hash = $1
	`, tokenHash)
	if err != nil {
		return fmt.Errorf("touch api token: %w", err)
	}
	return nil
}

// ListAPITokens returns a user's live (non-revoked) tokens, newest first.
func ListAPITokens(ctx context.Context, pool *pgxpool.Pool, userID string) ([]APIToken, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, name, created_at, last_used_at
		FROM api_tokens
		WHERE user_id = $1::uuid AND revoked_at IS NULL
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	var tokens []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan api token: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// RevokeAPIToken marks a user's token revoked. Filters by user_id so a user
// cannot revoke another user's token.
func RevokeAPIToken(ctx context.Context, pool *pgxpool.Pool, userID, id string) error {
	_, err := pool.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		WHERE id = $1::uuid AND user_id = $2::uuid AND revoked_at IS NULL
	`, id, userID)
	if err != nil {
		return fmt.Errorf("revoke api token: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./brain/...`
Expected: no output (clean compile).

- [ ] **Step 3: Commit**

```bash
git add brain/repository/api_token.go
git commit -m "feat: add api_tokens repository functions"
```

---

## Task 4: PAT branch in authMiddleware

**Files:**
- Modify: `main.go` (the `authMiddleware` function, around lines 124-151)

- [ ] **Step 1: Add the PAT branch**

In `main.go`, find this block inside `authMiddleware`:

```go
		rawToken := strings.TrimPrefix(auth, "Bearer ")

		subject, err := a.OIDC.Verify(r.Context(), rawToken)
```

Replace it with:

```go
		rawToken := strings.TrimPrefix(auth, "Bearer ")

		// Personal access token path: headless clients (e.g. the capture daemon)
		// send a token with the engram_pat_ prefix. Resolve it directly without OIDC.
		if strings.HasPrefix(rawToken, tokenPrefix) {
			tokenHash := hashAPIToken(rawToken)
			userID, err := repository.GetUserIDByTokenHash(r.Context(), a.Pool, tokenHash)
			if err != nil || userID == "" {
				http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
				return
			}
			_ = repository.TouchAPIToken(r.Context(), a.Pool, tokenHash)
			ctx := context.WithValue(r.Context(), brain.CtxUserID, userID)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		subject, err := a.OIDC.Verify(r.Context(), rawToken)
```

- [ ] **Step 2: Add the repository import**

In `main.go`, the import block already imports `"open-brain-go/brain"` and `"open-brain-go/brain/service"`. Add `"open-brain-go/brain/repository"` to that group:

```go
	"open-brain-go/brain"
	"open-brain-go/brain/repository"
	"open-brain-go/brain/service"
```

- [ ] **Step 3: Verify it compiles**

Run: `go build .`
Expected: no output (clean compile).

- [ ] **Step 4: Run existing tests to confirm nothing broke**

Run: `go test .`
Expected: PASS (existing web_auth + api_tokens tests).

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "feat: accept personal access tokens in authMiddleware"
```

---

## Task 5: Token management web handlers + page

**Files:**
- Modify: `api_tokens.go` — add handlers
- Create: `web/tokens.html`
- Modify: `web.go` — register routes + embed tokens.html

- [ ] **Step 1: Add handlers to api_tokens.go**

Append to `api_tokens.go`. Add the imports `encoding/json`, `log`, `net/http`, and `open-brain-go/brain/repository` to the file's import block (currently only `crypto/sha256`, `encoding/hex`):

```go
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"

	"open-brain-go/brain"
	"open-brain-go/brain/repository"
)
```

Then append the handlers:

```go
type createTokenRequest struct {
	Name string `json:"name"`
}

type createTokenResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"` // plaintext — shown exactly once
}

// createTokenHandler issues a new PAT for the authenticated user and returns
// the plaintext token once. Runs behind webAuthMiddleware (userID in context).
func createTokenHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(brain.CtxUserID).(string)
		if userID == "" {
			http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
			return
		}
		var req createTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
			return
		}

		plaintext, hash := generateAPIToken()
		id, err := repository.InsertAPIToken(r.Context(), a.Pool, userID, req.Name, hash)
		if err != nil {
			log.Printf("create token error: %v", err)
			http.Error(w, `{"error":"failed to create token"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createTokenResponse{ID: id, Name: req.Name, Token: plaintext})
	}
}

// listTokensHandler returns the user's live tokens (no hashes, no plaintext).
func listTokensHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(brain.CtxUserID).(string)
		tokens, err := repository.ListAPITokens(r.Context(), a.Pool, userID)
		if err != nil {
			log.Printf("list tokens error: %v", err)
			http.Error(w, `{"error":"failed to list tokens"}`, http.StatusInternalServerError)
			return
		}
		if tokens == nil {
			tokens = []repository.APIToken{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tokens": tokens})
	}
}

// revokeTokenHandler revokes one of the user's tokens by id (path value "id").
func revokeTokenHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(brain.CtxUserID).(string)
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, `{"error":"id is required"}`, http.StatusBadRequest)
			return
		}
		if err := repository.RevokeAPIToken(r.Context(), a.Pool, userID, id); err != nil {
			log.Printf("revoke token error: %v", err)
			http.Error(w, `{"error":"failed to revoke token"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
```

- [ ] **Step 2: Create the tokens page**

Create `web/tokens.html`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Engram — Capture Tokens</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 640px; margin: 2rem auto; padding: 0 1rem; }
    input, button { font-size: 1rem; padding: 0.4rem; }
    .token { background: #f4f4f4; padding: 0.6rem; border-radius: 4px; font-family: monospace; word-break: break-all; }
    ul { list-style: none; padding: 0; }
    li { display: flex; justify-content: space-between; align-items: center; padding: 0.5rem 0; border-bottom: 1px solid #eee; }
  </style>
</head>
<body>
  <h1>Capture Tokens</h1>
  <p>Create a personal access token for the capture daemon. The token is shown only once.</p>
  <input id="name" placeholder="token name (e.g. laptop)">
  <button onclick="createToken()">Create</button>
  <div id="new" class="token" style="display:none"></div>
  <h2>Active tokens</h2>
  <ul id="list"></ul>
  <script>
    async function load() {
      const r = await fetch('/tokens', { credentials: 'same-origin' });
      if (!r.ok) { document.getElementById('list').innerHTML = '<li>Not signed in — <a href="/web/login">log in</a></li>'; return; }
      const { tokens } = await r.json();
      document.getElementById('list').innerHTML = tokens.map(t =>
        `<li><span>${t.name}</span><button onclick="revoke('${t.id}')">Revoke</button></li>`).join('') || '<li>None yet</li>';
    }
    async function createToken() {
      const name = document.getElementById('name').value.trim();
      if (!name) return;
      const r = await fetch('/tokens', { method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name }) });
      if (!r.ok) { alert('failed'); return; }
      const { token } = await r.json();
      const el = document.getElementById('new');
      el.style.display = 'block';
      el.textContent = token;
      document.getElementById('name').value = '';
      load();
    }
    async function revoke(id) {
      await fetch('/tokens/' + id, { method: 'DELETE', credentials: 'same-origin' });
      load();
    }
    load();
  </script>
</body>
</html>
```

- [ ] **Step 3: Register routes and embed the page in web.go**

In `web.go`, add an embed directive next to the existing ones (after the `browseUI` embed):

```go
//go:embed web/tokens.html
var tokensUI string
```

Then in `RegisterWebHandlers`, after the existing `GET /entries` line, add:

```go
	mux.HandleFunc("GET /tokens.html", serveTokensUI())
	mux.Handle("POST /tokens", webAuthMiddleware(sessions, http.HandlerFunc(createTokenHandler(a))))
	mux.Handle("GET /tokens", webAuthMiddleware(sessions, http.HandlerFunc(listTokensHandler(a))))
	mux.Handle("DELETE /tokens/{id}", webAuthMiddleware(sessions, http.HandlerFunc(revokeTokenHandler(a))))
```

And add the `serveTokensUI` helper near `serveBrowseUI`:

```go
func serveTokensUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, tokensUI)
	}
}
```

- [ ] **Step 4: Verify it compiles and tests pass**

Run: `go build . && go test .`
Expected: clean build; existing tests PASS.

- [ ] **Step 5: Commit**

```bash
git add api_tokens.go web/tokens.html web.go
git commit -m "feat: add token management web handlers and page"
```

---

## Task 6: captured_sessions repository

**Files:**
- Create: `brain/repository/captured_session.go`

- [ ] **Step 1: Create the file**

Create `brain/repository/captured_session.go`:

```go
// ABOUTME: Repository functions for the captured_sessions live-capture tracking table.
// ABOUTME: RLS-scoped; all functions run inside a WithUserTx transaction.

package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CapturedSession is a row from captured_sessions.
type CapturedSession struct {
	Tool             string
	SessionID        string
	SummaryEntryID   *string
	ChunkedMsgCount  int
	MessageCount     int
	LastSummarizedAt *time.Time
}

// UpsertCapturedSessionParams holds the fields written on each ingest.
type UpsertCapturedSessionParams struct {
	Tool             string
	SessionID        string
	SummaryEntryID   *string
	ChunkedMsgCount  int
	MessageCount     int
	LastSummarizedAt *time.Time
	SessionEnded     bool
}

// GetCapturedSession returns the tracking row for (tool, session_id), or
// (nil, nil) if none exists. Must run inside a WithUserTx (RLS).
func GetCapturedSession(ctx context.Context, tx pgx.Tx, tool, sessionID string) (*CapturedSession, error) {
	var c CapturedSession
	err := tx.QueryRow(ctx, `
		SELECT tool, session_id, summary_entry_id::text, chunked_msg_count, message_count, last_summarized_at
		FROM captured_sessions
		WHERE tool = $1 AND session_id = $2
	`, tool, sessionID).Scan(&c.Tool, &c.SessionID, &c.SummaryEntryID, &c.ChunkedMsgCount, &c.MessageCount, &c.LastSummarizedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get captured session: %w", err)
	}
	return &c, nil
}

// UpsertCapturedSession inserts or updates the tracking row. Sets
// session_started_at on first insert and session_ended_at when SessionEnded.
// Must run inside a WithUserTx (RLS).
func UpsertCapturedSession(ctx context.Context, tx pgx.Tx, p UpsertCapturedSessionParams) error {
	var endedAt *time.Time
	if p.SessionEnded {
		now := time.Now()
		endedAt = &now
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO captured_sessions (
			user_id, tool, session_id, summary_entry_id,
			chunked_msg_count, message_count, last_summarized_at,
			session_started_at, session_ended_at, last_ingested_at
		) VALUES (
			current_setting('app.current_user_id')::uuid, $1, $2, $3::uuid,
			$4, $5, $6,
			now(), $7, now()
		)
		ON CONFLICT (user_id, tool, session_id) DO UPDATE SET
			summary_entry_id   = COALESCE(EXCLUDED.summary_entry_id, captured_sessions.summary_entry_id),
			chunked_msg_count  = EXCLUDED.chunked_msg_count,
			message_count      = EXCLUDED.message_count,
			last_summarized_at = COALESCE(EXCLUDED.last_summarized_at, captured_sessions.last_summarized_at),
			session_ended_at   = COALESCE(EXCLUDED.session_ended_at, captured_sessions.session_ended_at),
			last_ingested_at   = now()
	`, p.Tool, p.SessionID, p.SummaryEntryID,
		p.ChunkedMsgCount, p.MessageCount, p.LastSummarizedAt,
		endedAt)
	if err != nil {
		return fmt.Errorf("upsert captured session: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./brain/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add brain/repository/captured_session.go
git commit -m "feat: add captured_sessions repository functions"
```

---

## Task 7: UpdateEntryContent repository function

**Files:**
- Modify: `brain/repository/entry.go`

- [ ] **Step 1: Add UpdateEntryContent**

Append to `brain/repository/entry.go` (after `InsertEntry`):

```go
// UpdateEntryContentParams holds fields for re-writing an existing entry's
// content (used to upsert a conversation summary as it is regenerated).
type UpdateEntryContentParams struct {
	EntryID     string
	ContentText string
	Payload     json.RawMessage
	Tags        []string
	Entities    json.RawMessage
	Embedding   *pgvector.Vector
}

// UpdateEntryContent rewrites an existing entry's content, payload, tags,
// entities, and embedding inside a transaction. RLS scopes the row to the
// current user. The entries_updated_at trigger refreshes updated_at.
func UpdateEntryContent(ctx context.Context, tx pgx.Tx, p UpdateEntryContentParams) error {
	if p.Tags == nil {
		p.Tags = []string{}
	}
	if p.Entities == nil {
		p.Entities = json.RawMessage("{}")
	}
	_, err := tx.Exec(ctx, `
		UPDATE entries
		SET content_text = $2, payload = $3, tags = $4, entities = $5, embedding = $6
		WHERE id = $1::uuid
	`, p.EntryID, p.ContentText, p.Payload, p.Tags, p.Entities, p.Embedding)
	if err != nil {
		return fmt.Errorf("update entry content: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./brain/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add brain/repository/entry.go
git commit -m "feat: add UpdateEntryContent repository function"
```

---

## Task 8: Ingest pure logic (TDD)

**Files:**
- Create: `brain/service/ingest_test.go`
- Create: `brain/service/ingest.go`

- [ ] **Step 1: Write failing tests for the pure helpers**

Create `brain/service/ingest_test.go`:

```go
package service

import (
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
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./brain/service/ -run "TestEstimateTokens|TestPackChunks|TestShouldSummarize|TestRenderTranscript" -v`
Expected: compile error — types/functions undefined.

- [ ] **Step 3: Create ingest.go with types and pure helpers**

Create `brain/service/ingest.go`:

```go
// ABOUTME: IngestService — stores live-captured conversation transcripts as raw
// ABOUTME: chunk entries plus one upserted per-session distilled summary entry.

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"open-brain-go/brain"
	"open-brain-go/brain/repository"
)

// Tunables (configurable later; constants for v1).
const (
	chunkBudgetTokens = 1500
	summaryMinNewMsgs = 6
	summaryMaxAge     = 5 * time.Minute
	openRouterBase    = "https://openrouter.ai/api/v1"

	recordTypeChunk   = "conversation.chunk"
	recordTypeSummary = "conversation.summary"
)

// IngestMessage is one normalized message in a transcript batch.
type IngestMessage struct {
	Role  string `json:"role"` // "human" | "assistant"
	Text  string `json:"text"`
	Ts    string `json:"ts"`
	MsgID string `json:"msg_id"`
}

// IngestBatch is the full (trimmed) transcript for one session as sent by the
// capture daemon. Messages SHOULD be the complete transcript in order.
type IngestBatch struct {
	Tool         string          `json:"tool"`
	SessionID    string          `json:"session_id"`
	Title        string          `json:"title"`
	Project      string          `json:"project"`
	Messages     []IngestMessage `json:"messages"`
	SessionEnded bool            `json:"session_ended"`
}

// IngestResult summarizes what an ingest produced.
type IngestResult struct {
	ChunksCreated int  `json:"chunks_created"`
	Summarized    bool `json:"summarized"`
	MessageCount  int  `json:"message_count"`
}

// estimateTokens approximates token count as ceil(chars/4).
func estimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}

// packChunks groups messages into chunks whose estimated token totals stay near
// budgetTokens. A trailing partial chunk is returned as remainder (held for a
// later sweep) unless sessionEnded, in which case it is flushed into chunks.
func packChunks(msgs []IngestMessage, budgetTokens int, sessionEnded bool) (chunks [][]IngestMessage, remainder []IngestMessage) {
	var cur []IngestMessage
	curTokens := 0
	for _, m := range msgs {
		t := estimateTokens(m.Text)
		if curTokens+t > budgetTokens && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur = nil
			curTokens = 0
		}
		cur = append(cur, m)
		curTokens += t
	}
	if len(cur) > 0 {
		if sessionEnded {
			chunks = append(chunks, cur)
		} else {
			remainder = cur
		}
	}
	return chunks, remainder
}

// shouldSummarize decides whether to regenerate the per-session summary.
func shouldSummarize(newMsgCount int, lastSummarizedAt *time.Time, now time.Time, sessionEnded bool, minNewMsgs int, maxAge time.Duration) bool {
	if sessionEnded {
		return true
	}
	if newMsgCount == 0 {
		return false
	}
	if lastSummarizedAt == nil {
		return true
	}
	if newMsgCount >= minNewMsgs {
		return true
	}
	return now.Sub(*lastSummarizedAt) >= maxAge
}

// renderTranscript formats messages as "role: text" lines.
func renderTranscript(msgs []IngestMessage) string {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Text)
	}
	return b.String()
}

// ConversationSummary is the structured distillation of a session.
type ConversationSummary struct {
	Summary     string   `json:"summary"`
	Topics      []string `json:"topics"`
	Decisions   []string `json:"decisions"`
	Preferences []string `json:"preferences"`
	OpenThreads []string `json:"open_threads"`
}

// generateConversationSummary calls OpenRouter chat completions to distill a
// transcript into a structured summary. baseURL is injectable for tests.
func generateConversationSummary(ctx context.Context, client *http.Client, baseURL, key, fullText string) (ConversationSummary, error) {
	if len(fullText) > 24000 {
		fullText = fullText[:24000]
	}
	body, _ := json.Marshal(map[string]any{
		"model":           "openai/gpt-4o-mini",
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{
				"role": "system",
				"content": `Summarize this LLM coding/chat session. Return JSON with:
- "summary": 2-4 sentence prose summary of what happened
- "topics": array of 1-5 short topic tags
- "decisions": array of decisions or conclusions reached (empty if none)
- "preferences": array of preferences the user expressed (empty if none)
- "open_threads": array of unresolved questions or TODOs (empty if none)`,
			},
			{"role": "user", "content": fullText},
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return ConversationSummary{}, fmt.Errorf("build summary request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return ConversationSummary{}, fmt.Errorf("summary request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ConversationSummary{}, fmt.Errorf("decode summary response: %w", err)
	}
	if len(result.Choices) == 0 {
		return ConversationSummary{}, fmt.Errorf("empty choices in summary response")
	}

	var cs ConversationSummary
	if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &cs); err != nil {
		return ConversationSummary{}, fmt.Errorf("parse summary json: %w", err)
	}
	return cs, nil
}

// (IngestService and Ingest are added in the next task.)
var _ = pgx.ErrNoRows // keep pgx import until Ingest is added
var _ = repository.Entry{}
var _ = brain.CtxUserID
```

- [ ] **Step 4: Run tests to confirm they pass**

Run: `go test ./brain/service/ -run "TestEstimateTokens|TestPackChunks|TestShouldSummarize|TestRenderTranscript" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add brain/service/ingest.go brain/service/ingest_test.go
git commit -m "feat: add ingest types and pure helpers"
```

---

## Task 9: Summary generation test (TDD)

**Files:**
- Modify: `brain/service/ingest_test.go` — add httptest-backed test

- [ ] **Step 1: Add the failing test**

Add to `brain/service/ingest_test.go`. Update the import block to include `context`, `encoding/json`, `net/http`, `net/http/httptest`:

```go
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)
```

Then add:

```go
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
```

- [ ] **Step 2: Run to confirm it passes**

Run: `go test ./brain/service/ -run TestGenerateConversationSummary -v`
Expected: PASS (the function already exists from Task 8).

- [ ] **Step 3: Commit**

```bash
git add brain/service/ingest_test.go
git commit -m "test: add conversation summary generation test"
```

---

## Task 10: IngestService.Ingest orchestration

**Files:**
- Modify: `brain/service/ingest.go` — add `IngestService` and `Ingest`

- [ ] **Step 1: Replace the placeholder tail of ingest.go**

In `brain/service/ingest.go`, delete these three placeholder lines at the end:

```go
// (IngestService and Ingest are added in the next task.)
var _ = pgx.ErrNoRows // keep pgx import until Ingest is added
var _ = repository.Entry{}
var _ = brain.CtxUserID
```

Replace them with:

```go
// IngestService stores live-captured conversation transcripts.
type IngestService struct {
	app *brain.App
}

// NewIngestService creates an IngestService backed by the given App.
func NewIngestService(app *brain.App) *IngestService {
	return &IngestService{app: app}
}

// Ingest stores a transcript batch: it emits new raw chunk entries and, when
// the throttle allows, upserts the per-session distilled summary entry.
// The batch SHOULD contain the full trimmed transcript for the session.
func (s *IngestService) Ingest(ctx context.Context, batch IngestBatch) (IngestResult, error) {
	if batch.Tool == "" || batch.SessionID == "" {
		return IngestResult{}, fmt.Errorf("tool and session_id are required")
	}

	// 1. Read existing tracking row (RLS tx).
	var existing *repository.CapturedSession
	if err := s.app.WithUserTx(ctx, func(tx pgx.Tx) error {
		c, err := repository.GetCapturedSession(ctx, tx, batch.Tool, batch.SessionID)
		existing = c
		return err
	}); err != nil {
		return IngestResult{}, fmt.Errorf("read captured session: %w", err)
	}

	chunkedCount := 0
	var lastSummarizedAt *time.Time
	var summaryEntryID *string
	if existing != nil {
		chunkedCount = existing.ChunkedMsgCount
		lastSummarizedAt = existing.LastSummarizedAt
		summaryEntryID = existing.SummaryEntryID
	}

	// 2. Determine new (not-yet-chunked) messages and pack them.
	newMsgs := []IngestMessage{}
	if chunkedCount < len(batch.Messages) {
		newMsgs = batch.Messages[chunkedCount:]
	}
	chunks, remainder := packChunks(newMsgs, chunkBudgetTokens, batch.SessionEnded)

	// 3. Embed each chunk (network, outside tx). seq = index of first message.
	type chunkInsert struct {
		text     string
		seq      int
		embed    pgvectorVector
		entities json.RawMessage
	}
	var toInsert []chunkInsert
	seq := chunkedCount
	for _, c := range chunks {
		text := renderTranscript(c)
		emb, err := s.app.GetEmbedding(ctx, text)
		if err != nil {
			return IngestResult{}, fmt.Errorf("embed chunk: %w", err)
		}
		ent, _ := json.Marshal(map[string]any{
			"tool": batch.Tool, "session_id": batch.SessionID, "seq": seq,
		})
		toInsert = append(toInsert, chunkInsert{text: text, seq: seq, embed: emb, entities: ent})
		seq += len(c)
	}
	newChunkedCount := chunkedCount + (len(newMsgs) - len(remainder))

	// 4. Decide on and generate the summary (network, outside tx).
	now := time.Now()
	doSummary := shouldSummarize(len(newMsgs), lastSummarizedAt, now, batch.SessionEnded, summaryMinNewMsgs, summaryMaxAge)
	var summaryText string
	var summaryPayload, summaryEntities json.RawMessage
	var summaryTags []string
	var summaryEmbed pgvectorVector
	if doSummary {
		cs, err := generateConversationSummary(ctx, http.DefaultClient, openRouterBase, s.app.OpenRouterKey, renderTranscript(batch.Messages))
		if err != nil {
			// Best-effort: don't lose raw chunks because the summary failed.
			doSummary = false
		} else {
			summaryText = cs.Summary
			if summaryText == "" {
				summaryText = batch.Title
			}
			summaryEmbed, err = s.app.GetEmbedding(ctx, summaryText)
			if err != nil {
				// Best-effort: never lose raw chunks because the summary embed failed.
				doSummary = false
			} else {
				summaryPayload, _ = json.Marshal(cs)
				summaryEntities, _ = json.Marshal(map[string]any{
					"tool": batch.Tool, "session_id": batch.SessionID, "title": batch.Title,
				})
				summaryTags = cs.Topics
			}
		}
	}

	// 5. Persist everything in one RLS tx.
	newSummaryID := summaryEntryID
	if err := s.app.WithUserTx(ctx, func(tx pgx.Tx) error {
		for _, ci := range toInsert {
			emb := ci.embed
			if _, err := repository.InsertEntry(ctx, tx, repository.InsertEntryParams{
				RecordType:    recordTypeChunk,
				SchemaVersion: "1.0.0",
				Source:        batch.Tool,
				ContentText:   ci.text,
				Payload:       ci.entities, // payload mirrors entities for chunks
				Tags:          []string{},
				Entities:      ci.entities,
				Embedding:     &emb,
			}); err != nil {
				return err
			}
		}

		if doSummary {
			emb := summaryEmbed
			if summaryEntryID != nil {
				if err := repository.UpdateEntryContent(ctx, tx, repository.UpdateEntryContentParams{
					EntryID:     *summaryEntryID,
					ContentText: summaryText,
					Payload:     summaryPayload,
					Tags:        summaryTags,
					Entities:    summaryEntities,
					Embedding:   &emb,
				}); err != nil {
					return err
				}
			} else {
				id, err := repository.InsertEntry(ctx, tx, repository.InsertEntryParams{
					RecordType:    recordTypeSummary,
					SchemaVersion: "1.0.0",
					Source:        batch.Tool,
					ContentText:   summaryText,
					Payload:       summaryPayload,
					Tags:          summaryTags,
					Entities:      summaryEntities,
					Embedding:     &emb,
				})
				if err != nil {
					return err
				}
				newSummaryID = &id
			}
		}

		var summarizedAt *time.Time
		if doSummary {
			summarizedAt = &now
		}
		return repository.UpsertCapturedSession(ctx, tx, repository.UpsertCapturedSessionParams{
			Tool:             batch.Tool,
			SessionID:        batch.SessionID,
			SummaryEntryID:   newSummaryID,
			ChunkedMsgCount:  newChunkedCount,
			MessageCount:     len(batch.Messages),
			LastSummarizedAt: summarizedAt,
			SessionEnded:     batch.SessionEnded,
		})
	}); err != nil {
		return IngestResult{}, fmt.Errorf("persist ingest: %w", err)
	}

	return IngestResult{
		ChunksCreated: len(toInsert),
		Summarized:    doSummary,
		MessageCount:  len(batch.Messages),
	}, nil
}
```

- [ ] **Step 2: Add the pgvector type alias to the imports**

The code above references `pgvectorVector`. At the top of `brain/service/ingest.go`, add the pgvector import and a type alias. Update the import block to add:

```go
	pgvector "github.com/pgvector/pgvector-go"
```

And immediately after the `const (...)` block, add:

```go
// pgvectorVector aliases the embedding vector type for brevity.
type pgvectorVector = pgvector.Vector
```

- [ ] **Step 3: Verify it compiles**

Run: `go build ./brain/...`
Expected: no output. (The previously-unused `pgx`, `repository`, `brain` imports are now all used.)

- [ ] **Step 4: Run all service tests**

Run: `go test ./brain/service/`
Expected: `ok  open-brain-go/brain/service`.

- [ ] **Step 5: Commit**

```bash
git add brain/service/ingest.go
git commit -m "feat: add IngestService.Ingest orchestration"
```

---

## Task 11: /ingest HTTP handler and route

**Files:**
- Create: `ingest_handler.go`
- Modify: `main.go` — construct IngestService and register the route

- [ ] **Step 1: Create the handler**

Create `ingest_handler.go`:

```go
// ABOUTME: HTTP handler for POST /ingest — accepts a conversation transcript batch.
// ABOUTME: Authenticated via authMiddleware (PAT or OIDC); delegates to IngestService.

package main

import (
	"encoding/json"
	"log"
	"net/http"

	"open-brain-go/brain/service"
)

func ingestHandler(ingest *service.IngestService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var batch service.IngestBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		if batch.Tool == "" || batch.SessionID == "" {
			http.Error(w, `{"error":"tool and session_id are required"}`, http.StatusBadRequest)
			return
		}

		result, err := ingest.Ingest(r.Context(), batch)
		if err != nil {
			log.Printf("ingest error (tool=%s session=%s): %v", batch.Tool, batch.SessionID, err)
			http.Error(w, `{"error":"ingest failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}
```

- [ ] **Step 2: Wire it in main.go**

In `main.go`, after `es := service.NewEntryService(app)` (around line 79), add:

```go
	ingestSvc := service.NewIngestService(app)
```

Then in the mux setup, after the `mux.Handle("/mcp/", ...)` lines (around line 103), add:

```go
	mux.Handle("POST /ingest", authMiddleware(app, http.HandlerFunc(ingestHandler(ingestSvc))))
```

- [ ] **Step 3: Verify it compiles and all tests pass**

Run: `go build . && go test ./...`
Expected: clean build; all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add ingest_handler.go main.go
git commit -m "feat: add POST /ingest endpoint"
```

---

## Task 12: End-to-end smoke test

**Files:** none (manual verification against a running stack)

- [ ] **Step 1: Apply the schema to the running database**

```bash
sops exec-env secrets/engram.env 'psql $DATABASE_URL -f schema.sql'
```

Expected: `CREATE TABLE` / `CREATE INDEX` / `CREATE POLICY` lines, no errors.

- [ ] **Step 2: Rebuild and restart the server**

```bash
sops exec-env secrets/engram.env 'docker compose up -d --build'
```

Expected: containers start; `docker compose logs` shows "Open Brain MCP server listening".

- [ ] **Step 3: Create a PAT via the web UI**

Open `https://engram.x1024.net/tokens.html`, log in if needed, create a token named "smoke-test", and copy the plaintext shown once. Export it:

```bash
export ENGRAM_PAT='engram_pat_...'   # paste the value
```

- [ ] **Step 4: POST a transcript to /ingest**

```bash
curl -sS -X POST https://engram.x1024.net/ingest \
  -H "Authorization: Bearer $ENGRAM_PAT" \
  -H "Content-Type: application/json" \
  -d '{
    "tool": "claude-code",
    "session_id": "smoke-1",
    "title": "Smoke test session",
    "messages": [
      {"role":"human","text":"How do I write a table-driven test in Go?","msg_id":"m1"},
      {"role":"assistant","text":"Use a slice of structs with name, input, want fields and loop with t.Run.","msg_id":"m2"},
      {"role":"human","text":"Great, lets use TDD going forward.","msg_id":"m3"}
    ],
    "session_ended": true
  }'
```

Expected JSON like: `{"chunks_created":1,"summarized":true,"message_count":3}`.

- [ ] **Step 5: Verify entries were written**

```bash
sops exec-env secrets/engram.env \
  'psql $DATABASE_URL -c "SELECT record_type, source, left(content_text, 60) FROM entries WHERE record_type LIKE '\''conversation.%'\'' ORDER BY created_at;"'
```

Expected: one `conversation.chunk` row and one `conversation.summary` row, source `claude-code`.

- [ ] **Step 6: Verify idempotency — re-POST the same batch**

Re-run the Step 4 curl. Expected: `{"chunks_created":0,"summarized":true,...}` (no new chunks because `chunked_msg_count` already covers all 3 messages; summary regenerates because `session_ended` is true). Confirm the chunk count in the DB did not grow:

```bash
sops exec-env secrets/engram.env \
  'psql $DATABASE_URL -c "SELECT count(*) FROM entries WHERE record_type = '\''conversation.chunk'\'';"'
```

Expected: still 1.

- [ ] **Step 7: Verify it surfaces in search/browse**

Use the Engram MCP `search` tool or the browse page to search "table-driven test". Confirm the chunk and/or summary entry appears.

- [ ] **Step 8: Revoke the smoke-test token**

In `tokens.html`, revoke "smoke-test". Confirm a subsequent curl with `$ENGRAM_PAT` returns 401.

- [ ] **Step 9: Commit (docs/cleanup only, if any)**

```bash
git add -p
git commit -m "chore: live capture server side smoke-tested" --allow-empty
```

---

## Self-Review Notes

- **Spec coverage:** PAT auth (Tasks 1–5), `captured_sessions` + dedup (Tasks 1, 6, 10), raw append-only chunking (Tasks 8, 10), throttled distilled summary upsert (Tasks 8–10), `/ingest` endpoint (Task 11), error tolerance — chunks written before summary, summary failure non-fatal (Task 10, Step 1). Embeddings via existing OpenRouter path (reuses `app.GetEmbedding`).
- **Out of scope here (Part 2):** the capture daemon, per-tool parsers, trimming/placeholders, `--backfill`/`--dry-run`, and ended-by-age detection. Those live in the client binary plan. Backfill needs no server changes — it reuses `/ingest`.
- **Deviations from spec** (documented in the header): full-transcript batches instead of deltas; `chunked_msg_count` instead of `high_water_msg_id`.
```
