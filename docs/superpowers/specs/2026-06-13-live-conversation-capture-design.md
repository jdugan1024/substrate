# Live LLM Interaction Capture — Design Spec

**Date:** 2026-06-13
**Status:** Approved for planning

## Goal

Continuously capture LLM coding/chat sessions into Engram so that (a) the raw
conversation is preserved and semantically searchable, and (b) each session is
distilled into a memory that future LLM sessions can recall. Capture should
become available within ~30s of activity where cheap, but a 5–10 minute lag is
acceptable.

Two output streams per conversation, linked by `session_id`:

- **Raw** → append-only chunk entries (`record_type: conversation.chunk`)
- **Distilled** → one upserted summary entry per session (`record_type: conversation.summary`)

## Source tools

| Tool | Local transcript files? | Capture path |
|------|------|------|
| Claude Code | Yes — `~/.claude/projects/<project>/<session-uuid>.jsonl` | daemon (file watch) |
| Codex | Yes — `~/.codex/sessions/` | daemon (file watch) |
| Zed | Yes — local DB (SQLite threads) | daemon (file watch) |
| Pi | Yes — local session trees | daemon (file watch) |
| Claude Desktop | No (cloud only) | existing `conversation-import` (export JSON) — out of scope here |

Four of five write local transcript files. A single filesystem-watching daemon
covers all of them uniformly; per-tool hooks are an optional future accelerator,
not a dependency. Claude Desktop is handled by the separate, already-designed
batch `conversation-import` CLI and is out of scope for this spec.

## Architecture

```
  local machine                                    engram server
  ┌─────────────────────────┐                     ┌──────────────────────────┐
  │ capture-daemon          │                     │ POST /ingest             │
  │  fsnotify (debounce)    │                     │  - PAT auth → user (RLS) │
  │  per-tool parser        │   normalized batch  │  - dedup (captured_      │
  │  trim (placeholders)    │ ──────────────────► │      sessions)           │
  │  high-water mark        │   {tool, session_id,│  - raw chunking          │
  │  local state file       │    title, project,  │  - distillation (throttled)│
  │                         │    messages[],      │  - embed (OpenRouter)    │
  │                         │    session_ended}   │  - write via EntryService│
  └─────────────────────────┘                     └──────────────────────────┘
```

The daemon is a thin shipper: it parses tool-specific formats, trims, and sends
only new messages. All intelligence (chunking, embedding, distillation) and all
secrets (OpenRouter key) stay server-side, so adding a new tool is just a new
parser and every server-side improvement benefits all tools at once.

## Components

### a) Capture daemon — `cmd/capture-daemon` (new Go binary)

- **Config:** list of `{tool, watch_path, parser}` plus engram base URL + PAT;
  debounce interval; ended-by-age threshold; backfill rate limit.
- **Watching:** `fsnotify` (inotify) with a per-file debounce (~2s) so a burst of
  writes coalesces into one sweep.
- **Parsers:** convert a tool's native format into a normalized transcript:
  ```
  Transcript{ tool, session_id, title, project, messages []Message }
  Message{ role, text, ts, msg_id }
  ```
  `msg_id` must be stable across re-reads of the same file (e.g. the tool's own
  message UUID, or a deterministic hash of position+content when none exists).
- **Trimming (in the daemon, since it already parses):** keep human↔assistant
  dialogue; drop tool-call payloads, large file dumps, and thinking blocks — but
  replace each dropped span with a compact **placeholder** (`[tool: edit foo.go]`,
  `[12 KB output omitted]`) so the dropped content can be summarized later
  without re-reading originals.
- **High-water mark:** tracks the last shipped `msg_id` per session in a local
  state file so a restart doesn't re-ship everything. This is an *optimization
  only* — the server is authoritative for dedup (see Idempotency).
- **Output:** POSTs normalized new-message batches to `/ingest`.

### b) Ingestion endpoint — `POST /ingest` (new handler on engram)

- Authed via PAT (or existing OIDC). Body is a normalized batch.
- Resolves the user from the token; all DB work runs under that user's RLS.
- Orchestrates per batch: dedup → raw chunking → throttled distillation →
  embedding via the existing OpenRouter path → writes entries via `EntryService`.

### c) PAT auth — `api_tokens` table + middleware branch

- New table:
  ```sql
  CREATE TABLE api_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,   -- SHA-256 of the opaque token
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
  );
  ```
- `authMiddleware` gains a branch: if the bearer token hashes to a live (not
  revoked) `api_tokens` row, resolve that user and proceed; otherwise fall
  through to OIDC verification. Update `last_used_at` opportunistically.
- Web UI: a "Create capture token" action that generates a random opaque token,
  stores only its hash, and shows the plaintext **once**. Plus a list with
  revoke.

## Data model

`captured_sessions` — live-capture tracking. Separate from the batch
`conversation_imports` table (different corpus, different keyspace; they coexist
with no double-import):

```sql
CREATE TABLE captured_sessions (
  user_id            UUID        NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
  tool               TEXT        NOT NULL,
  session_id         TEXT        NOT NULL,
  summary_entry_id   UUID        REFERENCES entries(id) ON DELETE SET NULL,
  high_water_msg_id  TEXT,       -- last raw message folded into a finalized chunk
  message_count      INT         NOT NULL DEFAULT 0,
  session_started_at TIMESTAMPTZ,
  session_ended_at   TIMESTAMPTZ,
  last_summarized_at TIMESTAMPTZ,
  last_ingested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, tool, session_id)
);
```

This table is RLS-scoped like `entries`/`thoughts` (policy keyed on
`app.current_user_id`).

Raw chunk and summary entries store `tool` and `session_id` in their
`entities`/`payload` JSONB so the web UI can regroup fragments under one
conversation. `source` on each entry is set to the tool name
(`claude-code`, `codex`, `zed`, `pi`).

## Ingestion flow (per batch)

1. **Upsert** the `captured_sessions` row. Compute which incoming messages are
   *new* (after `high_water_msg_id`); discard already-seen ones (idempotency).
2. **Raw chunking.** Accumulate new messages into chunks by a token budget
   (~1500 tokens). Emit only *full* chunks as append-only `conversation.chunk`
   entries, each embedded. Hold a partial tail until it fills **or** the session
   ends. Advance `high_water_msg_id` to the last message folded into an emitted
   chunk.
3. **Distillation (throttled).** Regenerate the structured summary only when one
   of these holds: ≥ N new messages since `last_summarized_at`; **or**
   `session_ended`; **or** > ~5 minutes elapsed with new content. Summary
   captures: topics discussed, decisions/conclusions reached, preferences the
   user expressed, and open threads/TODOs. Upsert the single
   `conversation.summary` entry, re-embed it, set `last_summarized_at`.

Net effect: the **summary stays fresh every qualifying sweep** (recall is
current) even though the newest raw chunk may still be an un-emitted partial
tail; and a session left open all day does not burn an LLM call on every 2s
debounce.

## Backfill / historical catch-up

Backfill reuses the live pipeline; it is not a separate system.

- **`--backfill` one-shot mode** in the daemon: walk every configured watch path
  once, ship all sessions through `/ingest`, then exit (or hand off to watch
  mode). Run once per machine to catch up; the watcher keeps you current after.
- **Re-runnable by construction:** because the server is authoritative for dedup
  (`captured_sessions.high_water_msg_id` + per-`msg_id` filtering), a backfill
  that dies halfway — or a lost local state file — just resumes on re-run with no
  double-import.
- **Ended-by-age detection:** any file not modified in the last ~N minutes is
  treated as `session_ended`, so historical (closed) sessions get their partial
  tail flushed into a final chunk and a final summary, instead of waiting
  forever for more messages. This also cleanly finishes abandoned live sessions.
- **Cost guardrails:**
  - `--backfill --dry-run`: walk everything and report counts (sessions,
    messages, estimated chunks/embeddings/summaries) without writing — see the
    bill before paying it.
  - Configurable rate limit / concurrency cap so the first sweep doesn't hammer
    OpenRouter or the DB.

## Error handling

- **Daemon:** network failure → leave the high-water mark unadvanced and retry
  next sweep (at-least-once delivery; safe because the server dedups by
  `msg_id`). Unparseable lines are logged and skipped, never fatal.
- **Server:** raw chunks are written before distillation, so a failed
  embedding/summary call never loses raw data. Summary regeneration is
  best-effort and retried on the next qualifying sweep — mirroring the tolerance
  of the existing enrichment worker.

## Testing

- **Pure functions (TDD, matching the `conversation-import` plan's style):**
  per-tool parsers (fixture transcript → normalized form), trimming +
  placeholder generation, chunk-packing / token budgeting, high-water-mark
  advancement, ended-by-age logic.
- **Ingestion handler:** `httptest` server stubbing OpenRouter (same pattern as
  `generateSummary` in the import plan) to assert chunk/summary writes and dedup.
- **PAT middleware:** tested alongside the existing `web_auth_test.go`.

## Build order (phasing)

1. **PAT auth** — `api_tokens` table, middleware branch, web UI create/list/revoke. Unblocks everything.
2. **Server ingestion** — `/ingest` endpoint, `captured_sessions` table, raw
   chunking, throttled distillation, embedding — with **one** parser (Claude
   Code) wired end-to-end.
3. **Daemon shell + backfill** — `fsnotify`, debounce, local state file, POST
   loop, plus `--backfill`, `--dry-run`, and ended-by-age (they share all the
   same code).
4. **Additional parsers** — Codex, Pi, Zed, one at a time.
5. **Later** — hook-nudge for instant latency; atomic-fact extraction ("B");
   summarize the trimmed placeholders.

## Out of scope

- Claude Desktop / claude.ai capture (covered by the existing
  `conversation-import` batch CLI).
- Atomic per-fact memory extraction (deferred — raw chunks let us add it later).
- Per-tool native hooks (optional future accelerator).
```
