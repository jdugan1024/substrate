# Link Tracking (note.link) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `note.link` as a new entry record type with capture via `link:` prefix (web), `add_item link` (MCP), and PWA share target (mobile/desktop), with synchronous title/description fetch at capture and a background enrichment worker for retries and full-text extraction.

**Architecture:** New `note.link` schema stored in the canonical `entries` table alongside all existing record types. `FetchLinkMeta` in `brain/fetcher.go` handles synchronous HTML metadata extraction using regex (no new dependencies). An `EnrichmentWorker` goroutine retries failed fetches and extracts full-text for richer embeddings. All three capture surfaces route through `EntryService.CaptureTyped`. The web `link:` prefix is detected by `patternMatch` in `brain/extractor.go`, which routes to a new `note.link` case in `buildDeterministicEnvelope`.

**Tech Stack:** Go stdlib (`net/http`, `regexp`, `strings`); existing pgx v5, pgvector, jsonschema stack; vanilla JS for PWA share param handling.

---

## File Map

| File | Action | Responsibility |
|------|--------|----------------|
| `brain/schemas/note.link@1.0.0.json` | Create | JSON Schema for the new record type |
| `brain/fetcher.go` | Create | `FetchLinkMeta`, `ParseLinkText`, `BuildLinkPayload`, `EnrichmentWorker` |
| `brain/fetcher_test.go` | Create | Unit tests for fetcher helpers |
| `brain/extractor.go` | Modify | Add `link:` rule to `patternMatch`; handle `note.link` in `buildDeterministicEnvelope` |
| `brain/extractor_test.go` | Create | Tests for `link:` pattern matching and envelope building |
| `brain/app.go` | Modify | Add `WithAdminTx` for RLS-bypassing worker transactions (Task 2) |
| `brain/service/entry.go` | Modify | Handle `note.link` in `CaptureTyped` (no LLM, sync fetch) |
| `core/add_item.go` | Modify | Add `"link"` and `"note.link"` to `typeAliases` |
| `main.go` | Modify | Start `EnrichmentWorker` goroutine on server startup |
| `pwa.go` | Modify | Add `share_target` to manifest; add `/share` redirect handler; update service worker |
| `web/index.html` | Modify | Read `share_url`/`share_title` params on load; pre-fill textarea |

---

### Task 1: Schema file

**Files:**
- Create: `brain/schemas/note.link@1.0.0.json`
- Modify: `brain/registry_test.go`

- [ ] **Step 1: Write the failing test**

In `brain/registry_test.go`, add `"note.link"` to the `expected` slice in `TestSchemaRegistry_AllSchemasLoad`, and update the count in `TestSchemaRegistry_KnownRecordTypes`:

```go
// TestSchemaRegistry_AllSchemasLoad — add "note.link" to expected slice
expected := []string{
    "note.thought",
    "note.unstructured",
    "crm.contact",
    "crm.interaction",
    "maintenance.task",
    "jobhunt.application",
    "note.link",          // ← add
}

// TestSchemaRegistry_KnownRecordTypes — bump minimum count
if len(types) < 7 {    // was < 6
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./brain/... -run TestSchemaRegistry -v
```
Expected: FAIL — `SchemaFor("note.link", "1.0.0"): no schema registered...`

- [ ] **Step 3: Create the schema file**

`brain/schemas/note.link@1.0.0.json`:
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "note.link",
  "type": "object",
  "required": ["url"],
  "additionalProperties": false,
  "properties": {
    "url":            { "type": "string" },
    "title":          { "type": "string" },
    "description":    { "type": "string" },
    "notes":          { "type": "string" },
    "fetch_status":   { "type": "string", "enum": ["pending", "fetched", "failed"] },
    "fetch_error":    { "type": "string" },
    "extract_status": { "type": "string", "enum": ["pending", "extracted", "failed"] },
    "extract_error":  { "type": "string" }
  }
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./brain/... -run TestSchemaRegistry -v
```
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add brain/schemas/note.link@1.0.0.json brain/registry_test.go
git commit -m "feat: add note.link schema"
```

---

### Task 2: FetchLinkMeta and link helpers

**Files:**
- Create: `brain/fetcher.go`
- Create: `brain/fetcher_test.go`

- [ ] **Step 1: Write the failing tests**

`brain/fetcher_test.go`:
```go
package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLinkMeta_OGTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head>
			<meta property="og:title" content="OG Title">
			<meta property="og:description" content="OG Description">
		</head><body>hello</body></html>`))
	}))
	defer srv.Close()

	title, desc, err := FetchLinkMeta(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "OG Title" {
		t.Errorf("got title %q, want %q", title, "OG Title")
	}
	if desc != "OG Description" {
		t.Errorf("got desc %q, want %q", desc, "OG Description")
	}
}

func TestFetchLinkMeta_FallbackTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Fallback Title</title></head><body/></html>`))
	}))
	defer srv.Close()

	title, desc, err := FetchLinkMeta(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Fallback Title" {
		t.Errorf("got title %q, want %q", title, "Fallback Title")
	}
	if desc != "" {
		t.Errorf("expected empty desc, got %q", desc)
	}
}

func TestFetchLinkMeta_MetaDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head>
			<title>Page Title</title>
			<meta name="description" content="Meta Desc">
		</head></html>`))
	}))
	defer srv.Close()

	title, desc, err := FetchLinkMeta(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Page Title" {
		t.Errorf("got title %q", title)
	}
	if desc != "Meta Desc" {
		t.Errorf("got desc %q", desc)
	}
}

func TestFetchLinkMeta_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := FetchLinkMeta(context.Background(), srv.URL)
	if err == nil {
		t.Error("expected error for 404 response")
	}
}

func TestParseLinkText_URLOnly(t *testing.T) {
	url, notes := ParseLinkText("https://example.com/article")
	if url != "https://example.com/article" {
		t.Errorf("got url %q", url)
	}
	if notes != "" {
		t.Errorf("expected empty notes, got %q", notes)
	}
}

func TestParseLinkText_URLWithNotes(t *testing.T) {
	url, notes := ParseLinkText("https://example.com/article  great piece on distributed systems")
	if url != "https://example.com/article" {
		t.Errorf("got url %q", url)
	}
	if notes != "great piece on distributed systems" {
		t.Errorf("got notes %q", notes)
	}
}

func TestParseLinkText_LeadingSpace(t *testing.T) {
	url, notes := ParseLinkText("  https://example.com  my note  ")
	if url != "https://example.com" {
		t.Errorf("got url %q", url)
	}
	if notes != "my note" {
		t.Errorf("got notes %q", notes)
	}
}

func TestBuildLinkPayload_FetchedWithBoth(t *testing.T) {
	payload, contentText := BuildLinkPayload("https://example.com", "My Title", "My Desc", "my notes", nil)
	if contentText != "My Title — My Desc (https://example.com)" {
		t.Errorf("got contentText %q", contentText)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["fetch_status"] != "fetched" {
		t.Errorf("expected fetch_status=fetched, got %v", m["fetch_status"])
	}
	if m["extract_status"] != "pending" {
		t.Errorf("expected extract_status=pending, got %v", m["extract_status"])
	}
	if m["notes"] != "my notes" {
		t.Errorf("expected notes='my notes', got %v", m["notes"])
	}
}

func TestBuildLinkPayload_FetchedTitleOnly(t *testing.T) {
	_, contentText := BuildLinkPayload("https://example.com", "My Title", "", "", nil)
	if contentText != "My Title (https://example.com)" {
		t.Errorf("got contentText %q", contentText)
	}
}

func TestBuildLinkPayload_FetchError(t *testing.T) {
	payload, contentText := BuildLinkPayload("https://example.com", "", "", "", fmt.Errorf("timeout"))
	if contentText != "https://example.com" {
		t.Errorf("got contentText %q", contentText)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["fetch_status"] != "pending" {
		t.Errorf("expected fetch_status=pending, got %v", m["fetch_status"])
	}
	if _, ok := m["extract_status"]; ok {
		t.Error("extract_status should not be set when fetch failed")
	}
	if m["fetch_error"] == nil {
		t.Error("fetch_error should be set on fetch failure")
	}
}

func TestBuildLinkPayload_NoNotes(t *testing.T) {
	payload, _ := BuildLinkPayload("https://example.com", "T", "D", "", nil)
	var m map[string]any
	json.Unmarshal(payload, &m)
	if _, ok := m["notes"]; ok {
		t.Error("notes should be omitted when empty")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./brain/... -run "TestFetchLinkMeta|TestParseLinkText|TestBuildLinkPayload" -v
```
Expected: FAIL — `undefined: FetchLinkMeta`

- [ ] **Step 3: Create brain/fetcher.go**

```go
// ABOUTME: Link metadata fetcher — HTML title/description extraction and background enrichment worker.

package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── HTML meta extraction ──────────────────────────────────────────────────────

var (
	reOGTitle     = regexp.MustCompile(`(?i)<meta[^>]+property=["']og:title["'][^>]+content=["']([^"'<>]+)["']`)
	reOGTitleAlt  = regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"'<>]+)["'][^>]+property=["']og:title["']`)
	reOGDesc      = regexp.MustCompile(`(?i)<meta[^>]+property=["']og:description["'][^>]+content=["']([^"'<>]+)["']`)
	reOGDescAlt   = regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"'<>]+)["'][^>]+property=["']og:description["']`)
	reMetaDesc    = regexp.MustCompile(`(?i)<meta[^>]+name=["']description["'][^>]+content=["']([^"'<>]+)["']`)
	reMetaDescAlt = regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"'<>]+)["'][^>]+name=["']description["']`)
	reTitleTag    = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)</title>`)
	reHTMLTag     = regexp.MustCompile(`<[^>]+>`)
	reSpaces      = regexp.MustCompile(`\s+`)
)

// FetchLinkMeta fetches the given URL and extracts title and description from
// HTML meta tags. Priority: og:title > <title>, og:description > <meta name="description">.
// Returns a non-nil error if the HTTP request fails or the response is not 2xx.
func FetchLinkMeta(ctx context.Context, rawURL string) (title, description string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Engram/1.0; +https://engram.x1024.net)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 512 KB is enough to capture <head> without loading large bodies.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", "", fmt.Errorf("read body: %w", err)
	}
	s := string(body)

	title = firstMatch(s, reOGTitle, reOGTitleAlt, reTitleTag)
	description = firstMatch(s, reOGDesc, reOGDescAlt, reMetaDesc, reMetaDescAlt)
	return title, description, nil
}

// firstMatch tries each regex in order, returning the first non-empty capture group.
func firstMatch(s string, patterns ...*regexp.Regexp) string {
	for _, re := range patterns {
		if m := re.FindStringSubmatch(s); len(m) > 1 {
			if v := strings.TrimSpace(m[1]); v != "" {
				return v
			}
		}
	}
	return ""
}

// ParseLinkText splits "url [optional notes]" into its two components.
// The URL is the first whitespace-delimited token; notes is everything after it.
func ParseLinkText(text string) (rawURL, notes string) {
	text = strings.TrimSpace(text)
	parts := strings.SplitN(text, " ", 2)
	rawURL = parts[0]
	if len(parts) > 1 {
		notes = strings.TrimSpace(parts[1])
	}
	return
}

// BuildLinkPayload constructs the JSON payload and content_text for a note.link entry.
//   - On success (fetchErr == nil): fetch_status="fetched", extract_status="pending",
//     content_text = "{title} — {description} ({url})" (falls back to just title or url).
//   - On failure (fetchErr != nil): fetch_status="pending", fetch_error=<reason>,
//     content_text = url (searchable immediately by URL).
func BuildLinkPayload(rawURL, title, description, notes string, fetchErr error) (payload json.RawMessage, contentText string) {
	m := map[string]any{"url": rawURL}

	if fetchErr != nil {
		m["fetch_status"] = "pending"
		m["fetch_error"] = fetchErr.Error()
		contentText = rawURL
	} else {
		m["fetch_status"] = "fetched"
		m["extract_status"] = "pending"
		if title != "" {
			m["title"] = title
		}
		if description != "" {
			m["description"] = description
		}
		switch {
		case title != "" && description != "":
			contentText = fmt.Sprintf("%s — %s (%s)", title, description, rawURL)
		case title != "":
			contentText = fmt.Sprintf("%s (%s)", title, rawURL)
		default:
			contentText = rawURL
		}
	}

	if notes != "" {
		m["notes"] = notes
	}

	b, _ := json.Marshal(m)
	return b, contentText
}

// ── Enrichment worker ─────────────────────────────────────────────────────────

// EnrichmentWorker retries pending link fetches and extracts full-text content
// for richer semantic embeddings. Start it via Run in a goroutine.
type EnrichmentWorker struct {
	app *App
}

// NewEnrichmentWorker creates an EnrichmentWorker backed by the given App.
func NewEnrichmentWorker(app *App) *EnrichmentWorker {
	return &EnrichmentWorker{app: app}
}

// Run starts the enrichment loop. It runs until ctx is cancelled.
// The interval is controlled by ENGRAM_ENRICHMENT_INTERVAL (default 10m).
func (w *EnrichmentWorker) Run(ctx context.Context) {
	interval := 10 * time.Minute
	if v := os.Getenv("ENGRAM_ENRICHMENT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	log.Printf("enrichment worker: starting (interval=%s)", interval)
	for {
		w.fetchPendingLinks(ctx)
		w.extractPendingLinks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

type linkEntry struct {
	id      string
	url     string
	notes   string
	payload map[string]any
}

// fetchPendingLinks retries note.link entries where fetch_status="pending".
func (w *EnrichmentWorker) fetchPendingLinks(ctx context.Context) {
	var entries []linkEntry
	err := w.app.WithAdminTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, payload
			FROM entries
			WHERE record_type = 'note.link'
			  AND payload->>'fetch_status' = 'pending'
			  AND deleted_at IS NULL
			LIMIT 50
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e linkEntry
			var raw []byte
			if err := rows.Scan(&e.id, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &e.payload); err != nil {
				continue
			}
			e.url, _ = e.payload["url"].(string)
			e.notes, _ = e.payload["notes"].(string)
			if e.url != "" {
				entries = append(entries, e)
			}
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("enrichment: query pending links: %v", err)
		return
	}

	for _, e := range entries {
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		title, desc, fetchErr := FetchLinkMeta(fetchCtx, e.url)
		cancel()

		newPayload, contentText := BuildLinkPayload(e.url, title, desc, e.notes, fetchErr)

		// On persistent 4xx errors, mark as failed (no further retries).
		if fetchErr != nil && strings.HasPrefix(fetchErr.Error(), "HTTP 4") {
			var m map[string]any
			json.Unmarshal(newPayload, &m)
			m["fetch_status"] = "failed"
			newPayload, _ = json.Marshal(m)
		}

		emb, embErr := w.app.GetEmbedding(ctx, contentText)
		if embErr != nil {
			log.Printf("enrichment: embedding for %s: %v", e.id, embErr)
			continue
		}

		if err := w.app.WithAdminTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE entries
				SET payload = $2, content_text = $3, embedding = $4, updated_at = NOW()
				WHERE id = $1
			`, e.id, newPayload, contentText, &emb)
			return err
		}); err != nil {
			log.Printf("enrichment: update entry %s: %v", e.id, err)
		} else {
			log.Printf("enrichment: fetched link %s", e.id)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// extractPendingLinks fetches full-text for entries where fetch_status="fetched"
// and extract_status="pending", then regenerates their embeddings.
func (w *EnrichmentWorker) extractPendingLinks(ctx context.Context) {
	var entries []linkEntry
	err := w.app.WithAdminTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, payload->>'url'
			FROM entries
			WHERE record_type = 'note.link'
			  AND payload->>'fetch_status' = 'fetched'
			  AND payload->>'extract_status' = 'pending'
			  AND deleted_at IS NULL
			LIMIT 20
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e linkEntry
			if err := rows.Scan(&e.id, &e.url); err != nil {
				return err
			}
			if e.url != "" {
				entries = append(entries, e)
			}
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("enrichment: query extract pending: %v", err)
		return
	}

	for _, e := range entries {
		extractCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		text, extractErr := fetchFullText(extractCtx, e.url)
		cancel()

		var extractStatus, extractErrStr, embeddingText string
		if extractErr != nil || strings.TrimSpace(text) == "" {
			extractStatus = "failed"
			if extractErr != nil {
				extractErrStr = extractErr.Error()
			} else {
				extractErrStr = "no usable text extracted"
			}
			embeddingText = e.url
		} else {
			extractStatus = "extracted"
			if len(text) > 2000 {
				text = text[:2000]
			}
			embeddingText = text
		}

		emb, embErr := w.app.GetEmbedding(ctx, embeddingText)
		if embErr != nil {
			log.Printf("enrichment: extract embedding for %s: %v", e.id, embErr)
			continue
		}

		statusJSON, _ := json.Marshal(extractStatus)
		errJSON, _ := json.Marshal(extractErrStr)

		if err := w.app.WithAdminTx(ctx, func(tx pgx.Tx) error {
			if extractErrStr != "" {
				_, err := tx.Exec(ctx, `
					UPDATE entries
					SET payload = jsonb_set(jsonb_set(payload, '{extract_status}', $2), '{extract_error}', $3),
					    embedding = $4, updated_at = NOW()
					WHERE id = $1
				`, e.id, statusJSON, errJSON, &emb)
				return err
			}
			_, err := tx.Exec(ctx, `
				UPDATE entries
				SET payload = jsonb_set(payload, '{extract_status}', $2),
				    content_text = $3,
				    embedding = $4, updated_at = NOW()
				WHERE id = $1
			`, e.id, statusJSON, embeddingText, &emb)
			return err
		}); err != nil {
			log.Printf("enrichment: update extract %s: %v", e.id, err)
		} else {
			log.Printf("enrichment: extracted text for link %s (status=%s)", e.id, extractStatus)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// fetchFullText fetches a URL and returns its body with HTML tags stripped.
func fetchFullText(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Engram/1.0; +https://engram.x1024.net)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", err
	}

	text := reHTMLTag.ReplaceAllString(string(body), " ")
	text = reSpaces.ReplaceAllString(text, " ")
	return strings.TrimSpace(text), nil
}
```

- [ ] **Step 4: Add WithAdminTx to brain/app.go**

`EnrichmentWorker` calls `w.app.WithAdminTx` — add the method to `App` in `brain/app.go` now so the package compiles. Insert after `WithUserTx`:

```go
// WithAdminTx begins a transaction that bypasses row-level security.
// Requires the database role to have the BYPASSRLS attribute or be the
// table owner. Used exclusively by the EnrichmentWorker.
func (a *App) WithAdminTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL row_security = off"); err != nil {
		return fmt.Errorf("disable row_security: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
```

- [ ] **Step 5: Build to verify no compile errors**

```
go build ./...
```
Expected: success

- [ ] **Step 6: Run tests to verify they pass**

```
go test ./brain/... -run "TestFetchLinkMeta|TestParseLinkText|TestBuildLinkPayload" -v
```
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add brain/fetcher.go brain/fetcher_test.go brain/app.go
git commit -m "feat: add FetchLinkMeta, ParseLinkText, BuildLinkPayload, EnrichmentWorker, WithAdminTx"
```

---

### Task 3: link: prefix rule in extractor

**Files:**
- Modify: `brain/extractor.go`
- Create: `brain/extractor_test.go`

- [ ] **Step 1: Write the failing tests**

`brain/extractor_test.go`:
```go
package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPatternMatch_LinkPrefix(t *testing.T) {
	cases := []string{
		"link:https://example.com",
		"LINK:https://example.com",
		"Link:https://example.com/article extra notes",
		"link: https://example.com",
	}
	for _, text := range cases {
		rt, conf := patternMatch(text)
		if rt != "note.link" {
			t.Errorf("patternMatch(%q): got record_type %q, want note.link", text, rt)
		}
		if conf != 1.0 {
			t.Errorf("patternMatch(%q): got confidence %v, want 1.0", text, conf)
		}
	}
}

func TestPatternMatch_LinkDoesNotMatchPlainURL(t *testing.T) {
	// A plain URL without the link: prefix should route to note.thought, not note.link
	rt, _ := patternMatch("https://example.com check this out")
	if rt == "note.link" {
		t.Error("plain URL without link: prefix should not match note.link")
	}
}

func TestBuildDeterministicEnvelope_NoteLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>Test Page</title></head><body/></html>`))
	}))
	defer srv.Close()

	a := &App{OpenRouterKey: "test"}
	env, err := a.buildDeterministicEnvelope(context.Background(), "link:"+srv.URL+"  my notes", "note.link")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.RecordType != "note.link" {
		t.Errorf("got record_type %q", env.RecordType)
	}
	if env.Confidence != 1.0 {
		t.Errorf("got confidence %v, want 1.0", env.Confidence)
	}
	if env.ContentText == "" {
		t.Error("content_text should not be empty")
	}

	var m map[string]any
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if m["url"] == nil {
		t.Error("payload missing url field")
	}
	if m["notes"] != "my notes" {
		t.Errorf("expected notes='my notes', got %v", m["notes"])
	}
}

func TestBuildDeterministicEnvelope_NoteLinkNoPrefix(t *testing.T) {
	// When called from CaptureTyped, text may not have the link: prefix
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>Bare URL Page</title></head><body/></html>`))
	}))
	defer srv.Close()

	a := &App{OpenRouterKey: "test"}
	env, err := a.buildDeterministicEnvelope(context.Background(), srv.URL, "note.link")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.RecordType != "note.link" {
		t.Errorf("got record_type %q", env.RecordType)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./brain/... -run "TestPatternMatch_Link|TestBuildDeterministicEnvelope_NoteLink" -v
```
Expected: FAIL — `patternMatch("link:...") got note.thought, want note.link`

- [ ] **Step 3: Update patternMatch in brain/extractor.go**

Add the `link:` check at the very top of `patternMatch`, before the regex variable declarations are used. Also add `"strings"` to the import block.

Replace the current `patternMatch` function opening:

```go
func patternMatch(text string) (recordType string, confidence float64) {
	hasEmail := reEmail.MatchString(text)
```

With:

```go
func patternMatch(text string) (recordType string, confidence float64) {
	// Explicit link: prefix — highest priority, no ambiguity.
	if trimmed := strings.TrimSpace(text); len(trimmed) >= 5 && strings.EqualFold(trimmed[:5], "link:") {
		return "note.link", 1.0
	}

	hasEmail := reEmail.MatchString(text)
```

Add `"strings"` to the import block in `brain/extractor.go`.

- [ ] **Step 4: Update buildDeterministicEnvelope in brain/extractor.go**

Add a `note.link` case at the very top of `buildDeterministicEnvelope`, before the `note.thought` check:

```go
func (a *App) buildDeterministicEnvelope(ctx context.Context, text, recordType string) (*Envelope, error) {
	// note.link: strip optional link: prefix, parse url+notes, sync fetch metadata.
	if recordType == "note.link" {
		raw := strings.TrimSpace(text)
		if len(raw) >= 5 && strings.EqualFold(raw[:5], "link:") {
			raw = strings.TrimSpace(raw[5:])
		}
		rawURL, notes := ParseLinkText(raw)

		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		title, desc, fetchErr := FetchLinkMeta(fetchCtx, rawURL)

		payload, contentText := BuildLinkPayload(rawURL, title, desc, notes, fetchErr)
		return &Envelope{
			RecordType:    "note.link",
			SchemaVersion: "1.0.0",
			Payload:       payload,
			ContentText:   contentText,
			Confidence:    1.0,
		}, nil
	}

	// For note.thought, we can skip LLM entirely — just wrap the content.
	if recordType == "note.thought" {
```

- [ ] **Step 5: Run all brain tests**

```
go test ./brain/... -v
```
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add brain/extractor.go brain/extractor_test.go
git commit -m "feat: add link: prefix rule and note.link handling in extractor"
```

---

### Task 4: CaptureTyped for note.link

**Files:**
- Modify: `brain/service/entry.go`

- [ ] **Step 1: Add note.link case to CaptureTyped**

In `brain/service/entry.go`, update `CaptureTyped`. The current `note.thought` check is at line ~139:

```go
// For note.thought, skip LLM field extraction and wrap content directly.
if recordType == "note.thought" {
    env.Payload, _ = json.Marshal(map[string]any{"content": text})
} else {
```

Replace with:

```go
// For note.link, parse url+notes and sync-fetch metadata; no LLM involved.
if recordType == "note.link" {
    rawURL, notes := brain.ParseLinkText(text)

    fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    title, desc, fetchErr := brain.FetchLinkMeta(fetchCtx, rawURL)

    env.Payload, env.ContentText = brain.BuildLinkPayload(rawURL, title, desc, notes, fetchErr)
} else if recordType == "note.thought" {
    env.Payload, _ = json.Marshal(map[string]any{"content": text})
} else {
```

Also add `"context"` to the import if not already present. It is already imported via `"context"` at line 7.

Add `"time"` to the import block in `brain/service/entry.go` (it is already there via `"time"` at line 11 — confirm).

- [ ] **Step 2: Build to verify no compile errors**

```
go build ./...
```
Expected: success

- [ ] **Step 3: Run all tests**

```
go test ./brain/... -v
```
Expected: all PASS

- [ ] **Step 4: Commit**

```bash
git add brain/service/entry.go
git commit -m "feat: handle note.link in CaptureTyped — sync fetch, no LLM"
```

---

### Task 5: MCP alias

**Files:**
- Modify: `core/add_item.go`

- [ ] **Step 1: Add link aliases to typeAliases**

In `core/add_item.go`, update the `typeAliases` map and the error message:

```go
var typeAliases = map[string]string{
	"thought":     "note.thought",
	"note":        "note.thought",
	"contact":     "crm.contact",
	"interaction": "crm.interaction",
	"maintenance": "maintenance.task",
	"task":        "maintenance.task",
	"job":         "jobhunt.application",
	"application": "jobhunt.application",
	"link":        "note.link",           // ← add
	// canonical names also accepted
	"note.thought":        "note.thought",
	"note.unstructured":   "note.unstructured",
	"crm.contact":         "crm.contact",
	"crm.interaction":     "crm.interaction",
	"maintenance.task":    "maintenance.task",
	"jobhunt.application": "jobhunt.application",
	"note.link":           "note.link",   // ← add
}
```

Update the error message to include `link`:

```go
known := strings.Join([]string{"contact", "link", "thought", "maintenance", "job", "interaction"}, ", ")
```

- [ ] **Step 2: Build and run all tests**

```
go build ./... && go test ./...
```
Expected: success, all PASS

- [ ] **Step 3: Commit**

```bash
git add core/add_item.go
git commit -m "feat: add link alias to MCP add_item type aliases"
```

---

### Task 6: Start enrichment worker in main.go

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Start the worker goroutine**



In `main.go`, after `es := service.NewEntryService(app)` (line ~79), add:

```go
es := service.NewEntryService(app)

// Start background enrichment worker — retries failed link fetches and
// extracts full-text for richer semantic embeddings.
workerCtx, workerCancel := context.WithCancel(ctx)
defer workerCancel()
go brain.NewEnrichmentWorker(app).Run(workerCtx)
```

- [ ] **Step 2: Build to verify no compile errors**

```
go build ./...
```
Expected: success

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "feat: start enrichment worker goroutine on server startup"
```

---

### Task 8: PWA share target

**Files:**
- Modify: `pwa.go`

- [ ] **Step 1: Add share_target to manifest**

In `pwa.go`, update `manifestJSON` to add `share_target` before the closing `}`:

```go
var manifestJSON = `{
  "name": "Engram",
  "short_name": "Engram",
  "description": "Personal knowledge capture",
  "start_url": "/",
  "display": "standalone",
  "background_color": "#111827",
  "theme_color": "#4f46e5",
  "icons": [
    { "src": "/icon?s=192", "sizes": "192x192", "type": "image/png", "purpose": "any maskable" },
    { "src": "/icon?s=512", "sizes": "512x512", "type": "image/png", "purpose": "any maskable" }
  ],
  "share_target": {
    "action": "/share",
    "method": "GET",
    "params": {
      "title": "title",
      "text":  "text",
      "url":   "url"
    }
  }
}
`
```

- [ ] **Step 2: Update serviceWorkerJS to pass through /share**

Replace `serviceWorkerJS` with:

```go
var serviceWorkerJS = `
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', e => e.waitUntil(clients.claim()));
self.addEventListener('fetch', e => {
  // Pass through the share target endpoint — never cache it.
  if (new URL(e.request.url).pathname === '/share') return;
});
`
```

- [ ] **Step 3: Add shareHandler and register it**

Add `"net/url"` to the import block in `pwa.go`.

Add the handler function:

```go
func shareHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		shareURL := q.Get("url")
		if shareURL == "" {
			shareURL = q.Get("text") // some browsers send the URL in the text param
		}
		shareTitle := q.Get("title")

		rq := url.Values{}
		if shareURL != "" {
			rq.Set("share_url", shareURL)
		}
		if shareTitle != "" {
			rq.Set("share_title", shareTitle)
		}
		http.Redirect(w, r, "/?"+rq.Encode(), http.StatusFound)
	}
}
```

Register in `RegisterPWAHandlers`:

```go
func RegisterPWAHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) { ... })
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) { ... })
	mux.HandleFunc("GET /icon", iconHandler())
	mux.HandleFunc("GET /share", shareHandler())    // ← add
}
```

- [ ] **Step 4: Build and run all tests**

```
go build ./... && go test ./...
```
Expected: success, all PASS

- [ ] **Step 5: Commit**

```bash
git add pwa.go
git commit -m "feat: add PWA share target manifest entry and /share redirect handler"
```

---

### Task 9: web/index.html — share param pre-fill

**Files:**
- Modify: `web/index.html`

- [ ] **Step 1: Add maybePreFillShare function**

In `web/index.html`, add the `maybePreFillShare` function before the `init` function (around line 531):

```javascript
// ── PWA Share target ──────────────────────────────────────────────────────────

function maybePreFillShare() {
  const params = new URLSearchParams(window.location.search);
  const shareURL = params.get('share_url');
  if (!shareURL) return;

  const shareTitle = params.get('share_title');
  const textarea = document.getElementById('note-input');
  textarea.value = 'link:' + shareURL + (shareTitle ? '  ' + shareTitle : '');
  // Clean the URL so refreshing doesn't re-fill the textarea.
  window.history.replaceState({}, '', '/');
}
```

- [ ] **Step 2: Update init() to call maybePreFillShare**

Replace the current `init` function:

```javascript
async function init() {
  if (window.location.search.includes('code=')) {
    const ok = await handleCallback();
    if (ok) { showCaptureSection(); return; }
    showAuthSection();
    return;
  }

  const token = await getValidToken();
  if (token) {
    showCaptureSection();
  } else {
    showAuthSection();
  }
}
```

With:

```javascript
async function init() {
  if (window.location.search.includes('code=')) {
    const ok = await handleCallback();
    if (ok) { showCaptureSection(); maybePreFillShare(); return; }
    showAuthSection();
    return;
  }

  const token = await getValidToken();
  if (token) {
    showCaptureSection();
    maybePreFillShare();
  } else {
    showAuthSection();
  }
}
```

- [ ] **Step 3: Build and run all tests**

```
go build ./... && go test ./...
```
Expected: success, all PASS

- [ ] **Step 4: Commit**

```bash
git add web/index.html
git commit -m "feat: pre-fill capture textarea from PWA share target params"
```
