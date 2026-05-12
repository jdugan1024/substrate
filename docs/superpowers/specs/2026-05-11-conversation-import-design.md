# Conversation Import Design

**Date:** 2026-05-11  
**Status:** Approved

## Goal

Import a Claude conversation export (`conversations.json`) into Engram as searchable memories. One Engram entry per conversation, topic-indexed so you can later find "conversations where I discussed X."

## Schema Addition

New tracking table in `schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS conversation_imports (
    conversation_uuid       TEXT        PRIMARY KEY,
    entry_id                UUID        NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    conversation_updated_at TIMESTAMPTZ,
    content_hash            TEXT,        -- SHA256 of message UUIDs in order
    imported_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`content_hash` is the authoritative change signal on re-runs. `conversation_updated_at` is stored as a human-readable "last activity" timestamp. `ON DELETE CASCADE` keeps the tracking table clean if an entry is ever removed.

## Architecture

### `brain/repository/conversation_import.go`

Thin repository layer for the tracking table. Three functions:

- `GetConversationImport(ctx, tx, uuid)` — returns stored hash + entry ID, or nil if not found
- `InsertConversationImport(ctx, tx, params)` — writes a new tracking row
- `DeleteConversationImport(ctx, tx, uuid)` — removes the tracking row (cascades to entry deletion on re-import)

### `cmd/import-conversations/main.go`

CLI entry point. Uses the same `brain.App` initialization as the main server.

**Flags:**
- `--input` — path to `conversations.json` (required)
- `--user-id` — Engram user UUID to import as (required)
- `--dry-run` — compute hashes and log what would happen, no DB writes

**Flow:**

1. Parse flags, open DB connection via `brain.App`
2. Load all existing tracking rows into `map[string]importRecord` (UUID → hash)
3. For each conversation in the JSON:
   a. Compute `content_hash`: SHA256 of all message UUIDs concatenated in order
   b. If hash matches stored hash → log "skip", continue
   c. If hash differs → delete old entry (CASCADE removes tracking row), then re-import
   d. Build content text (see below)
   e. Call `EntryService.Capture(ctx, text, "claude-export")`
   f. Insert row into `conversation_imports`
4. Print final counts: imported / skipped / updated / failed

## Content Text Format

For conversations with an existing summary:

```
Conversation: {name}
Date: {created_at}
Summary: {summary}
```

For conversations without a summary (currently 46 of 328): generate one first via a direct OpenRouter chat completion call, then use the same format above.

## Summary Generation

Prompt sent to OpenRouter for unsummarized conversations:

```
Given the following conversation transcript, write a 2-3 sentence summary
capturing the main topic, key questions asked, and any conclusions reached.

Conversation: {name}
Messages:
{first 10 human messages, truncated to 2000 chars total}
```

Only human-side messages are included — the topic is clear from the questions, and it keeps the prompt small. If the LLM call fails, fall back to the conversation name alone and log a warning.

## Error Handling

- Per-conversation failures are isolated — one failure does not abort the import
- Failed conversation UUIDs are collected and printed in a summary at the end
- Re-runs are safe: the hash check skips already-imported conversations in microseconds
- No built-in rate limiting; 429s from OpenRouter are treated as per-conversation failures and the conversation can be re-imported on the next run

## Files Changed

| File | Change |
|------|--------|
| `schema.sql` | Add `conversation_imports` table |
| `brain/repository/conversation_import.go` | New — tracking table repository |
| `cmd/import-conversations/main.go` | New — CLI entry point |
