# Memory Browser Design

**Date:** 2026-04-16
**Status:** Approved

## Overview

Add a read-only `/browse` page to the engram web UI for browsing and searching captured memories. The page is a separate route from the existing `/` capture UI, shares the same auth infrastructure, and follows the same plain HTML/CSS/JS approach with no build step or framework.

## Goals

- Browse all entries chronologically in a mixed feed
- Filter by record type (thoughts, contacts, tasks, etc.)
- Search by text across all entries
- Works well on phone, tablet, and desktop
- Read-only — no editing or deletion

## Architecture

### Backend

Two additions to `web.go`:

**1. `GET /entries` API endpoint** (auth-protected via existing `authMiddleware`)

Query parameters:
- `q` — text search against `content_text` using `ILIKE '%q%'`
- `type` — filter by `record_type` (e.g. `note.thought`, `crm.contact`)
- `limit` — default 50
- `offset` — default 0, used for pagination

Response shape:
```json
{
  "entries": [
    {
      "id": "uuid",
      "record_type": "note.thought",
      "content_text": "...",
      "payload_summary": "Topics: search, cognition",
      "created_at": "2026-04-14T10:23:00Z"
    }
  ],
  "has_more": true
}
```

`payload_summary` reuses the existing `formatPayloadSummary` function from `core/search.go`. Results are ordered by `created_at DESC`.

Text search uses `ILIKE` — no embedding API call needed. This is fast enough at personal-data volumes and avoids latency.

**2. `GET /browse` route** serving the embedded `web/browse.html`.

### Frontend

`web/browse.html` — embedded in the binary via `//go:embed`, same as `web/index.html`.

**Layout (Option A — cards with top bar):**

- **Sticky top bar:** search input (debounced 300ms) + horizontally scrollable type filter chips (All, Thoughts, Contacts, Tasks, Calendar, Meals, Interactions). Chips scroll horizontally on narrow screens.
- **Entry cards:** record type badge, date (top-right), `content_text` body, payload summary line.
- **Infinite scroll:** `IntersectionObserver` on a sentinel `<div>` at the bottom of the list. On intersection, fetches `GET /entries?offset=N` and appends cards. Stops when `has_more: false`.
- **Navigation:** "Browse →" link added to the capture page header. "← Capture" link in the browse page header.

**Auth:** Reads bearer token from `localStorage` (same keys as capture page: `engram_access_token`, `engram_refresh_token`, `engram_token_expires_at`). Uses the same `getValidToken` / `refreshAccessToken` logic. If no valid token, redirects to `/`.

**Styling:** Shares CSS custom properties (`--bg`, `--card`, `--accent`, etc.) with `index.html` for consistent appearance including dark mode.

## Data Flow

1. Page load → check token → if missing/expired, redirect to `/`
2. Initial fetch: `GET /entries?limit=50&offset=0`
3. Render cards into feed
4. Search input change (debounced 300ms) → reset offset to 0, re-fetch
5. Type chip click → reset offset to 0, re-fetch
6. IntersectionObserver fires at bottom → fetch next page (`offset += 50`), append cards

## Error Handling

- **401 response:** clear tokens, redirect to `/` for re-auth
- **Network error / 5xx:** inline error banner above the feed with a "Retry" button
- **Empty results:** "No entries found" message — different copy for empty search vs. empty account
- **Infinite scroll load failure:** stop observer, show "Failed to load more — tap to retry" link at bottom

## Files Changed

| File | Change |
|------|--------|
| `web/browse.html` | New — browse page HTML/CSS/JS |
| `web/index.html` | Add "Browse →" link in header |
| `web.go` | Add `GET /browse` handler and `GET /entries` API handler |

## Out of Scope

- Editing or deleting entries
- Semantic/vector search (text ILIKE is sufficient)
- Sorting options (chronological DESC only)
- Pagination controls (infinite scroll only)
