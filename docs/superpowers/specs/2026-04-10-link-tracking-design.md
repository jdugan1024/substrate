# Link Tracking Design

**Date:** 2026-04-10
**Status:** Approved

## Overview

Add `note.link` as a new entry record type for saving and finding links to articles and posts. Capture is fast (save now, enrich async); retrieval is semantic via the existing `search` tool. Three capture surfaces: web UI trigger, MCP `add_item link`, and PWA share target for mobile/desktop.

---

## Data Model

New record type `note.link` stored in the canonical `entries` table.

### Schema: `note.link@1.0.0.json`

```json
{
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

### Field Notes

- `url` — required; the saved link
- `title` / `description` — populated by the fetch worker from `og:title`, `og:description`, `<title>`, `<meta name="description">`
- `notes` — user-supplied annotation; may be added at capture or later
- `fetch_status` — tracks title/description fetch (`pending` → `fetched` | `failed`)
- `extract_status` — tracks full-text extraction for richer embeddings (`pending` → `extracted` | `failed`)
- `fetch_error` / `extract_error` — reason string when status is `failed`

### `content_text` and `tags`

`content_text` is built as `"{title} — {description} ({url})"` when title/description are available; falls back to the raw URL. This is what gets embedded for semantic search.

Tags live in `entries.tags` (the standard JSONB column), not duplicated in the payload. This keeps tag-based filtering consistent across all record types.

---

## Capture Surfaces

All three paths call `EntryService.CaptureTyped(ctx, "note.link", content, source)`.

### 1. Web UI — `link:` Prefix

Pattern matcher gets a new deterministic rule: text starting with `link:` (case-insensitive) strips the prefix, routes to `note.link`. Anything after the URL is treated as `notes`.

```
link:https://example.com/article  great piece on distributed systems
```

No LLM involved. Confidence: 1.0 (explicit trigger).

### 2. MCP — `add_item link`

Add `"link": "note.link"` to `typeAliases` in `core/add_item.go`. Works immediately once the schema is registered.

```
add_item link https://example.com/article  great piece on distributed systems
```

### 3. PWA Share Target

Add `share_target` to the manifest in `pwa.go`:

```json
"share_target": {
  "action": "/share",
  "method": "GET",
  "params": {
    "title": "title",
    "text":  "text",
    "url":   "url"
  }
}
```

The browser navigates to `/share?url=...&title=...` when the user shares to Engram. Since auth is Bearer token based, `/share` returns a lightweight HTML redirect to `/?share_url=...&share_title=...`. The main page JS reads these params on load and pre-fills the capture textarea with `link:<url>`, optionally appending the browser-supplied title as a note prompt. The user can add notes and submit normally.

The service worker is updated to pass-through `/share` requests (no caching).

---

## Fetch & Enrichment

### At Capture (Synchronous, 5s Timeout)

New function `brain.FetchLinkMeta(ctx, url) (title, description string, err error)`:

1. GET the URL with a realistic User-Agent
2. Extract in priority order: `og:title` → `<title>`, `og:description` → `<meta name="description">`
3. Return first non-empty values

On success: `content_text` is set to `"{title} — {description} ({url})"`, `fetch_status: "fetched"`, embedding generated.

On timeout or error: `content_text = url`, `fetch_status: "pending"`, `fetch_error` populated. Entry is still saved and searchable by URL.

### Background Enrichment Worker

A goroutine launched at server startup. Interval configurable via `ENGRAM_ENRICHMENT_INTERVAL` (default `10m`).

**Pass 1 — Fetch pending links:**
Query `entries WHERE record_type = 'note.link' AND payload->>'fetch_status' = 'pending'`. For each: retry `FetchLinkMeta`, update payload, regenerate embedding. On persistent failure (e.g. 4xx): set `fetch_status: "failed"`, do not retry.

**Pass 2 — Full-text extract:**
Query `entries WHERE record_type = 'note.link' AND payload->>'fetch_status' = 'fetched' AND payload->>'extract_status' = 'pending'`. For each: fetch full page, strip HTML to plain text, use as embedding source, set `extract_status: "extracted"`. Paywalled/JS-heavy pages that return no usable text: set `extract_status: "failed"`.

Both passes process entries sequentially with a short delay between requests. The worker runs under a background context (no user RLS) since it operates on its own rows.

---

## Files Affected

| File | Change |
|------|--------|
| `brain/schemas/note.link@1.0.0.json` | New schema file |
| `brain/fetcher.go` | New — `FetchLinkMeta` + enrichment worker |
| `brain/extractor.go` | Add `link:` prefix rule to `patternMatch` |
| `brain/service/entry.go` | Post-save: trigger async fetch for `note.link` entries |
| `core/add_item.go` | Add `"link": "note.link"` to `typeAliases` |
| `pwa.go` | Add `share_target` to manifest; add `/share` handler |
| `web/index.html` | Read `share_url` / `share_title` query params on load |
| `main.go` | Start enrichment worker goroutine |

---

## Non-Goals

- No explicit read/unread status tracking
- No browser extension (Phase C, separate deliverable)
- No retry logic for `fetch_status: "failed"` entries (manual re-save if needed)
- No full-text extraction for paywalled or JS-rendered pages
