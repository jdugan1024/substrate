# Memory Browser Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only `/browse` page to the engram web UI for browsing and searching all captured entries with type filtering and infinite scroll.

**Architecture:** A new `GET /browse` route serves an embedded `web/browse.html` page (same plain HTML/CSS/JS pattern as the existing capture page). A new auth-protected `GET /entries` API endpoint serves paginated JSON for the browse page to consume. The existing `formatPayloadSummary` function in `core/search.go` is exported so `web.go` can use it.

**Tech Stack:** Go 1.22+, `net/http`, `pgx/v5`, vanilla HTML/CSS/JS, `//go:embed`

---

### Task 1: Export `FormatPayloadSummary` and add `note.link` case

**Files:**
- Modify: `core/search.go`

The function `formatPayloadSummary` is currently unexported (lowercase). The new entries handler in `web.go` (package `main`) needs to call it. Rename it and add a `note.link` case while we're touching it.

- [ ] **Step 1: Rename `formatPayloadSummary` to `FormatPayloadSummary` and add `note.link` case**

  In `core/search.go`, replace the function signature and body. The only internal call is inside `searchAll` at the bottom of the file.

  Change the function signature line from:
  ```go
  func formatPayloadSummary(recordType string, raw json.RawMessage) string {
  ```
  to:
  ```go
  func FormatPayloadSummary(recordType string, raw json.RawMessage) string {
  ```

  Add a `note.link` case to the switch statement, between `"note.thought"` and `"note.unstructured"`:
  ```go
  case "note.link":
      if v, _ := m["title"].(string); v != "" {
          parts = append(parts, "Title: "+v)
      }
      if v, _ := m["url"].(string); v != "" {
          parts = append(parts, v)
      }
  ```

  Update the internal call in `searchAll` (search for `formatPayloadSummary(` in the same file):
  ```go
  fmt.Fprintf(&sb, "%s\n\n", FormatPayloadSummary(r.RecordType, r.Payload))
  ```

- [ ] **Step 2: Verify the build passes**

  ```bash
  cd /home/jdugan/engram && go build ./...
  ```
  Expected: no output, exit 0.

- [ ] **Step 3: Run existing tests**

  ```bash
  cd /home/jdugan/engram && go test ./...
  ```
  Expected: all tests pass.

- [ ] **Step 4: Commit**

  ```bash
  git add core/search.go
  git commit -m "refactor: export FormatPayloadSummary and add note.link case"
  ```

---

### Task 2: Add `GET /entries` API handler

**Files:**
- Modify: `web.go`

- [ ] **Step 1: Update imports in `web.go`**

  Replace the existing import block with:
  ```go
  import (
  	"encoding/json"
  	_ "embed"
  	"fmt"
  	"log"
  	"net/http"
  	"strconv"
  	"time"

  	"github.com/jackc/pgx/v5"

  	"open-brain-go/brain"
  	"open-brain-go/brain/service"
  	"open-brain-go/core"
  )
  ```

- [ ] **Step 2: Add response structs and handler function**

  Add the following after the `captureResponse` struct (around line 49 in the original file):

  ```go
  type entryItem struct {
  	ID             string `json:"id"`
  	RecordType     string `json:"record_type"`
  	ContentText    string `json:"content_text"`
  	PayloadSummary string `json:"payload_summary"`
  	CreatedAt      string `json:"created_at"`
  }

  type entriesResponse struct {
  	Entries []entryItem `json:"entries"`
  	HasMore bool        `json:"has_more"`
  }

  func listEntriesHandler(a *brain.App) http.HandlerFunc {
  	return func(w http.ResponseWriter, r *http.Request) {
  		q := r.URL.Query().Get("q")
  		recordType := r.URL.Query().Get("type")
  		limit := 50
  		offset := 0
  		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
  			limit = v
  		}
  		if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
  			offset = v
  		}

  		fetchLimit := limit + 1
  		var items []entryItem

  		err := a.WithUserTx(r.Context(), func(tx pgx.Tx) error {
  			qSQL := `SELECT id::text, record_type, content_text, payload, created_at
  				FROM entries
  				WHERE deleted_at IS NULL`
  			args := []any{}
  			n := 1

  			if q != "" {
  				qSQL += fmt.Sprintf(" AND content_text ILIKE $%d", n)
  				args = append(args, "%"+q+"%")
  				n++
  			}
  			if recordType != "" {
  				qSQL += fmt.Sprintf(" AND record_type = $%d", n)
  				args = append(args, recordType)
  				n++
  			}
  			qSQL += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", n, n+1)
  			args = append(args, fetchLimit, offset)

  			rows, err := tx.Query(r.Context(), qSQL, args...)
  			if err != nil {
  				return err
  			}
  			defer rows.Close()
  			for rows.Next() {
  				var item entryItem
  				var payload json.RawMessage
  				var createdAt time.Time
  				if err := rows.Scan(&item.ID, &item.RecordType, &item.ContentText, &payload, &createdAt); err != nil {
  					return err
  				}
  				item.CreatedAt = createdAt.UTC().Format(time.RFC3339)
  				item.PayloadSummary = core.FormatPayloadSummary(item.RecordType, payload)
  				items = append(items, item)
  			}
  			return rows.Err()
  		})
  		if err != nil {
  			log.Printf("list entries error: %v", err)
  			http.Error(w, `{"error":"failed to fetch entries"}`, http.StatusInternalServerError)
  			return
  		}

  		hasMore := len(items) > limit
  		if hasMore {
  			items = items[:limit]
  		}
  		if items == nil {
  			items = []entryItem{}
  		}

  		w.Header().Set("Content-Type", "application/json")
  		json.NewEncoder(w).Encode(entriesResponse{Entries: items, HasMore: hasMore})
  	}
  }
  ```

- [ ] **Step 3: Register the route in `RegisterWebHandlers`**

  In `RegisterWebHandlers`, add this line after the existing `POST /capture` registration:
  ```go
  mux.Handle("GET /entries", authMiddleware(a, http.HandlerFunc(listEntriesHandler(a))))
  ```

- [ ] **Step 4: Verify the build passes**

  ```bash
  cd /home/jdugan/engram && go build ./...
  ```
  Expected: no output, exit 0.

- [ ] **Step 5: Commit**

  ```bash
  git add web.go
  git commit -m "feat: add GET /entries API endpoint for browse page"
  ```

---

### Task 3: Create `web/browse.html` and add `GET /browse` route

**Files:**
- Create: `web/browse.html`
- Modify: `web.go`

- [ ] **Step 1: Create `web/browse.html`**

  Create the file with this complete content:

  ```html
  <!DOCTYPE html>
  <html lang="en">
  <head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta name="theme-color" content="#4f46e5">
  <title>Engram — Browse</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

    :root {
      --bg: #f5f5f5;
      --card: #ffffff;
      --text: #1a1a1a;
      --muted: #6b7280;
      --border: #e5e7eb;
      --accent: #4f46e5;
      --accent-hover: #4338ca;
      --chip-active-bg: #e0e7ff;
      --chip-active-text: #3730a3;
      --error-bg: #fef2f2;
      --error-text: #991b1b;
    }

    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #111827;
        --card: #1f2937;
        --text: #f9fafb;
        --muted: #9ca3af;
        --border: #374151;
        --accent: #6366f1;
        --accent-hover: #818cf8;
        --chip-active-bg: #312e81;
        --chip-active-text: #c7d2fe;
        --error-bg: #450a0a;
        --error-text: #fca5a5;
      }
    }

    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: var(--bg);
      color: var(--text);
      min-height: 100dvh;
    }

    /* ── Top bar ── */
    .topbar {
      position: sticky;
      top: 0;
      z-index: 10;
      background: var(--bg);
      border-bottom: 1px solid var(--border);
      padding: 0.75rem 1rem;
    }

    .topbar-row1 {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
    }

    .topbar-title {
      font-size: 1rem;
      font-weight: 700;
      letter-spacing: -0.02em;
      white-space: nowrap;
    }

    .topbar-title a {
      color: var(--muted);
      text-decoration: none;
      font-size: 0.875rem;
      font-weight: 500;
    }

    .topbar-title a:hover { color: var(--accent); }

    .search-input {
      flex: 1;
      padding: 0.5rem 0.75rem;
      font-size: 16px; /* prevents iOS auto-zoom */
      font-family: inherit;
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--card);
      color: var(--text);
      outline: none;
      transition: border-color 0.15s;
      min-width: 0;
    }

    .search-input:focus { border-color: var(--accent); }
    .search-input::placeholder { color: var(--muted); }

    /* ── Filter chips ── */
    .chips {
      display: flex;
      gap: 0.375rem;
      overflow-x: auto;
      scrollbar-width: none;
      -webkit-overflow-scrolling: touch;
      padding-bottom: 2px; /* prevent clipping focus rings */
    }

    .chips::-webkit-scrollbar { display: none; }

    .chip {
      flex-shrink: 0;
      padding: 0.3125rem 0.75rem;
      font-size: 0.8125rem;
      font-weight: 500;
      border: 1px solid var(--border);
      border-radius: 999px;
      background: var(--card);
      color: var(--muted);
      cursor: pointer;
      touch-action: manipulation;
      transition: background 0.1s, color 0.1s, border-color 0.1s;
      -webkit-tap-highlight-color: transparent;
    }

    .chip.active {
      background: var(--chip-active-bg);
      color: var(--chip-active-text);
      border-color: transparent;
    }

    /* ── Feed ── */
    .feed {
      max-width: 640px;
      margin: 0 auto;
      padding: 1rem;
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    /* ── Entry card ── */
    .entry-card {
      background: var(--card);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 1rem;
      box-shadow: 0 1px 3px rgba(0,0,0,0.06);
    }

    .entry-meta {
      display: flex;
      justify-content: space-between;
      align-items: baseline;
      margin-bottom: 0.5rem;
      gap: 0.5rem;
    }

    .entry-type {
      font-size: 0.6875rem;
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--muted);
    }

    .entry-date {
      font-size: 0.75rem;
      color: var(--muted);
      flex-shrink: 0;
    }

    .entry-text {
      font-size: 0.9375rem;
      line-height: 1.55;
      color: var(--text);
      word-break: break-word;
    }

    .entry-summary {
      font-size: 0.8125rem;
      color: var(--muted);
      margin-top: 0.375rem;
    }

    /* ── State messages ── */
    .state-msg {
      text-align: center;
      color: var(--muted);
      font-size: 0.9375rem;
      padding: 3rem 1rem;
    }

    .error-banner {
      background: var(--error-bg);
      color: var(--error-text);
      border-radius: 8px;
      padding: 0.75rem 1rem;
      font-size: 0.9375rem;
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
    }

    .retry-btn {
      font-size: 0.875rem;
      font-weight: 600;
      background: none;
      border: 1px solid currentColor;
      border-radius: 6px;
      color: inherit;
      padding: 0.25rem 0.625rem;
      cursor: pointer;
      flex-shrink: 0;
    }

    .load-more-err {
      text-align: center;
      padding: 1rem;
      font-size: 0.875rem;
      color: var(--muted);
    }

    .load-more-err a {
      color: var(--accent);
      cursor: pointer;
      text-decoration: underline;
    }

    #sentinel { height: 1px; }
  </style>
  </head>
  <body>

  <div class="topbar">
    <div class="topbar-row1">
      <span class="topbar-title">Engram &nbsp;<a href="/">← Capture</a></span>
      <input class="search-input" id="search" type="search" placeholder="Search memories…" autocomplete="off">
    </div>
    <div class="chips" id="chips">
      <button class="chip active" data-type="">All</button>
      <button class="chip" data-type="note.thought">Thoughts</button>
      <button class="chip" data-type="note.link">Links</button>
      <button class="chip" data-type="crm.contact">Contacts</button>
      <button class="chip" data-type="crm.interaction">Interactions</button>
      <button class="chip" data-type="maintenance.task">Tasks</button>
      <button class="chip" data-type="jobhunt.application">Jobs</button>
    </div>
  </div>

  <div class="feed" id="feed"></div>
  <div id="sentinel"></div>

  <script>
  // ── Token helpers (mirrors capture page logic) ────────────────────────────────

  function clearTokens() {
    ['engram_access_token', 'engram_refresh_token', 'engram_token_expires_at']
      .forEach(k => localStorage.removeItem(k));
  }

  async function refreshAccessToken() {
    const rt = localStorage.getItem('engram_refresh_token');
    if (!rt) { clearTokens(); return null; }
    const resp = await fetch('/oauth/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ grant_type: 'refresh_token', refresh_token: rt, client_id: 'engram' }),
    });
    if (!resp.ok) { clearTokens(); return null; }
    const tokens = await resp.json();
    localStorage.setItem('engram_access_token', tokens.access_token);
    if (tokens.refresh_token) localStorage.setItem('engram_refresh_token', tokens.refresh_token);
    localStorage.setItem('engram_token_expires_at', String(Date.now() + (tokens.expires_in || 3600) * 1000));
    return tokens.access_token;
  }

  async function getValidToken() {
    const token = localStorage.getItem('engram_access_token');
    const exp = parseInt(localStorage.getItem('engram_token_expires_at') || '0');
    if (!token) return null;
    if (Date.now() >= exp - 60_000) return await refreshAccessToken();
    return token;
  }

  // ── State ─────────────────────────────────────────────────────────────────────

  let offset = 0;
  let loading = false;
  let hasMore = true;
  let currentQ = '';
  let currentType = '';
  let observer = null;

  // ── Rendering ─────────────────────────────────────────────────────────────────

  function escapeHTML(str) {
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  function formatDate(iso) {
    const d = new Date(iso);
    const now = new Date();
    const opts = { month: 'short', day: 'numeric' };
    if (d.getFullYear() !== now.getFullYear()) opts.year = 'numeric';
    return d.toLocaleDateString('en-US', opts);
  }

  function formatType(t) {
    return t.replace('.', ' · ');
  }

  function renderCard(entry) {
    const card = document.createElement('div');
    card.className = 'entry-card';
    const summary = entry.payload_summary
      ? `<div class="entry-summary">${escapeHTML(entry.payload_summary)}</div>`
      : '';
    card.innerHTML = `
      <div class="entry-meta">
        <span class="entry-type">${escapeHTML(formatType(entry.record_type))}</span>
        <span class="entry-date">${escapeHTML(formatDate(entry.created_at))}</span>
      </div>
      <div class="entry-text">${escapeHTML(entry.content_text)}</div>
      ${summary}
    `;
    return card;
  }

  // ── Feed management ───────────────────────────────────────────────────────────

  function showStateMsg(msg) {
    const el = document.createElement('div');
    el.className = 'state-msg';
    el.textContent = msg;
    document.getElementById('feed').appendChild(el);
  }

  function showInitialError() {
    const feed = document.getElementById('feed');
    const banner = document.createElement('div');
    banner.className = 'error-banner';
    banner.innerHTML = `<span>Failed to load entries.</span><button class="retry-btn">Retry</button>`;
    banner.querySelector('.retry-btn').onclick = () => {
      reset();
      fetchEntries(true);
    };
    feed.appendChild(banner);
  }

  function showLoadMoreError() {
    if (document.getElementById('load-more-err')) return;
    const el = document.createElement('div');
    el.id = 'load-more-err';
    el.className = 'load-more-err';
    el.innerHTML = `Failed to load more. <a onclick="retryLoadMore()">Tap to retry.</a>`;
    document.getElementById('feed').appendChild(el);
  }

  function retryLoadMore() {
    const el = document.getElementById('load-more-err');
    if (el) el.remove();
    fetchEntries(false);
  }

  // ── Fetch ─────────────────────────────────────────────────────────────────────

  async function fetchEntries(isInitial) {
    if (loading) return;
    loading = true;

    const token = await getValidToken();
    if (!token) { clearTokens(); window.location.href = '/'; return; }

    const params = new URLSearchParams({ limit: '50', offset: String(offset) });
    if (currentQ) params.set('q', currentQ);
    if (currentType) params.set('type', currentType);

    let resp;
    try {
      resp = await fetch('/entries?' + params, {
        headers: { 'Authorization': 'Bearer ' + token },
      });
    } catch {
      loading = false;
      if (isInitial) showInitialError();
      else showLoadMoreError();
      return;
    }

    if (resp.status === 401) { clearTokens(); window.location.href = '/'; return; }

    if (!resp.ok) {
      loading = false;
      if (isInitial) showInitialError();
      else showLoadMoreError();
      return;
    }

    const data = await resp.json();
    loading = false;
    hasMore = data.has_more;

    const feed = document.getElementById('feed');

    if (isInitial && data.entries.length === 0) {
      showStateMsg(currentQ || currentType ? 'No entries found.' : 'No memories captured yet.');
      return;
    }

    const lme = document.getElementById('load-more-err');
    if (lme) lme.remove();

    for (const entry of data.entries) {
      feed.appendChild(renderCard(entry));
    }

    offset += data.entries.length;

    if (!hasMore && observer) {
      observer.disconnect();
      observer = null;
    }
  }

  function reset() {
    offset = 0;
    hasMore = true;
    loading = false;
    document.getElementById('feed').innerHTML = '';
    if (observer) observer.disconnect();
    setupObserver();
  }

  // ── Infinite scroll ───────────────────────────────────────────────────────────

  function setupObserver() {
    observer = new IntersectionObserver((entries) => {
      if (entries[0].isIntersecting && hasMore && !loading) {
        fetchEntries(false);
      }
    }, { rootMargin: '200px' });
    observer.observe(document.getElementById('sentinel'));
  }

  // ── Search (debounced 300ms) ──────────────────────────────────────────────────

  let searchTimer = null;
  document.getElementById('search').addEventListener('input', (e) => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      currentQ = e.target.value.trim();
      reset();
      fetchEntries(true);
    }, 300);
  });

  // ── Filter chips ──────────────────────────────────────────────────────────────

  document.getElementById('chips').addEventListener('click', (e) => {
    const chip = e.target.closest('.chip');
    if (!chip) return;
    document.querySelectorAll('.chip').forEach(c => c.classList.remove('active'));
    chip.classList.add('active');
    currentType = chip.dataset.type;
    reset();
    fetchEntries(true);
  });

  // ── Init ──────────────────────────────────────────────────────────────────────

  async function init() {
    const token = await getValidToken();
    if (!token) { window.location.href = '/'; return; }
    setupObserver();
    fetchEntries(true);
  }

  document.addEventListener('DOMContentLoaded', init);
  </script>
  </body>
  </html>
  ```

- [ ] **Step 2: Add embed directive and `serveBrowseUI` to `web.go`**

  In `web.go`, add the embed directive and variable directly below the existing `//go:embed web/index.html` line:
  ```go
  //go:embed web/browse.html
  var browseUI string
  ```

  Add the handler function after `serveWebUI()`:
  ```go
  func serveBrowseUI() http.HandlerFunc {
  	return func(w http.ResponseWriter, r *http.Request) {
  		w.Header().Set("Content-Type", "text/html; charset=utf-8")
  		w.Header().Set("Cache-Control", "no-cache")
  		fmt.Fprint(w, browseUI)
  	}
  }
  ```

- [ ] **Step 3: Register `GET /browse` in `RegisterWebHandlers`**

  Add after the existing `mux.Handle("POST /capture", ...)` line:
  ```go
  mux.HandleFunc("GET /browse", serveBrowseUI())
  ```

- [ ] **Step 4: Verify the build passes**

  ```bash
  cd /home/jdugan/engram && go build ./...
  ```
  Expected: no output, exit 0.

- [ ] **Step 5: Run all tests**

  ```bash
  cd /home/jdugan/engram && go test ./...
  ```
  Expected: all tests pass.

- [ ] **Step 6: Commit**

  ```bash
  git add web/browse.html web.go
  git commit -m "feat: add /browse page and GET /browse route"
  ```

---

### Task 4: Add "Browse →" link to capture page

**Files:**
- Modify: `web/index.html`

- [ ] **Step 1: Add browse link style to the CSS block**

  In `web/index.html`, find the closing `</style>` tag and add these rules just before it:
  ```css
  .browse-link {
    font-size: 0.875rem;
    font-weight: 500;
    color: var(--muted);
    text-decoration: none;
    margin-left: 0.5rem;
  }
  .browse-link:hover { color: var(--accent); }
  ```

- [ ] **Step 2: Update the `<h1>` element**

  Find:
  ```html
    <h1>Engram</h1>
  ```
  Replace with:
  ```html
    <h1>Engram <a href="/browse" class="browse-link">Browse →</a></h1>
  ```

- [ ] **Step 3: Verify the build passes**

  ```bash
  cd /home/jdugan/engram && go build ./...
  ```
  Expected: no output, exit 0.

- [ ] **Step 4: Commit**

  ```bash
  git add web/index.html
  git commit -m "feat: add Browse link to capture page header"
  ```

---

### Task 5: End-to-end smoke test

This task is manual verification. No code changes.

- [ ] **Step 1: Start the server locally**

  The server requires `DATABASE_URL`, `OPENROUTER_API_KEY`, `AUTHELIA_ISSUER_URL`, and `OIDC_CLIENT_ID`. Check how you normally run it locally (docker-compose or direct). Start it and confirm it's listening.

- [ ] **Step 2: Verify `GET /entries` returns JSON**

  With a valid bearer token (copy from browser localStorage after logging in on `/`):
  ```bash
  curl -s -H "Authorization: Bearer <token>" \
    "https://engram.x1024.net/entries?limit=5" | python3 -m json.tool
  ```
  Expected: JSON with `entries` array and `has_more` boolean. Each entry has `id`, `record_type`, `content_text`, `payload_summary`, `created_at`.

- [ ] **Step 3: Verify search filtering**

  ```bash
  curl -s -H "Authorization: Bearer <token>" \
    "https://engram.x1024.net/entries?q=test&limit=5" | python3 -m json.tool
  ```
  Expected: only entries whose `content_text` contains "test" (case-insensitive).

- [ ] **Step 4: Verify type filtering**

  ```bash
  curl -s -H "Authorization: Bearer <token>" \
    "https://engram.x1024.net/entries?type=note.thought&limit=5" | python3 -m json.tool
  ```
  Expected: only entries with `record_type: "note.thought"`.

- [ ] **Step 5: Open `/browse` in the browser**

  Navigate to `https://engram.x1024.net/browse`. Verify:
  - Page loads, shows entry cards
  - Cards show record type badge, date, content, and payload summary where present
  - Dark mode applies if system is in dark mode

- [ ] **Step 6: Test search in browser**

  Type a word in the search box. Verify results update after ~300ms and show only matching entries.

- [ ] **Step 7: Test type filter chips**

  Click "Thoughts" chip. Verify only `note.thought` entries appear and the chip highlights.
  Click "All" to reset. Verify all entries return.

- [ ] **Step 8: Test infinite scroll**

  If you have more than 50 entries: scroll to the bottom. Verify additional cards load automatically.

- [ ] **Step 9: Test unauthenticated redirect**

  Open a private/incognito window, navigate to `/browse`. Verify redirect to `/` (login page).

- [ ] **Step 10: Test capture → browse navigation**

  From `/`, verify "Browse →" link appears in the header. Click it. Verify navigation to `/browse`. Click "← Capture". Verify navigation back to `/`.
