# Substrate — Project Specification

> A self-hosted Go MCP server for persistent AI memory, backed by
> PostgreSQL + pgvector.

**Status:** Living document · **Module:** `substrate` (formerly `open-brain-go`)
· **Derived from:** [Open Brain / OB1](https://github.com/NateBJones-Projects/OB1)

---

## 1. Purpose & Vision

Substrate gives any MCP-compatible AI client (Claude Desktop, Claude Code, Codex,
etc.) a **personal, persistent memory layer**. Instead of losing context when a
conversation ends, a user can:

- **Capture** freeform thoughts, notes, links, contacts, and tasks — with
  automatic classification, metadata extraction, and embeddings.
- **Recall** anything later by **semantic search** across every kind of record.
- **Store structured domain data** (household facts, recipes, family calendar,
  CRM, job-hunt pipeline) through first-class extensions.
- **Automatically remember AI conversations** — a local daemon watches Claude
  Code and Codex transcripts and ships them into memory as searchable,
  summarized sessions.

Substrate is **multi-user** with strict per-user isolation enforced at the database
layer via PostgreSQL Row Level Security (RLS). It is designed to be self-hosted
behind a private OIDC provider (Authelia).

### Design principles

- **One unified memory store.** All captured records are rows in a single
  `entries` table, typed by `record_type`, with JSONB payloads validated against
  JSON Schemas. Semantic search works uniformly across every type.
- **Server owns the intelligence.** Clients (web UI, capture daemon) are thin.
  Classification, embedding, chunking, and summarization all happen server-side.
- **No LLM output is trusted without validation.** Every extracted payload is
  checked against a JSON Schema before it is persisted; failures degrade
  gracefully to an unstructured note rather than being dropped.
- **Isolation is enforced by the database, not the application.** RLS policies
  are the backstop; a bug in application code cannot leak another user's data.

---

## 2. System Overview

Substrate is a **single Go process** listening on `:8080`, plus one **companion
daemon** that runs on each user's machine.

```
┌─────────────────────────────────────────────────────────────────┐
│                      Substrate server (:8080)                       │
│                                                                   │
│  MCP (Streamable HTTP) ──┐                                        │
│  Web UI / PWA ───────────┤                                        │
│  POST /ingest ───────────┼──► service layer ──► repository ──► DB │
│  OAuth/OIDC proxy ───────┘        (logic)          (SQL)          │
│                                                                   │
│  Background: EnrichmentWorker (link fetch + re-embed)             │
└─────────────────────────────────────────────────────────────────┘
        │                                    │
        ▼                                    ▼
  Authelia (OIDC,                   PostgreSQL 17 + pgvector
  auth.x1024.net)                   (RLS, HNSW cosine index)
        ▲
        │  embeddings + LLM extraction
        ▼
  OpenRouter (text-embedding-3-small, gpt-4o-mini)

  ┌──────────────────────────────────────┐
  │  substrate-capture daemon (per machine) │  watches transcripts,
  │  Claude Code + Codex transcripts ────┼─► POSTs /ingest (Bearer PAT)
  └──────────────────────────────────────┘
```

### External dependencies

| Dependency | Role |
|---|---|
| **PostgreSQL 17 + pgvector** | Primary datastore; vector similarity search; RLS isolation |
| **OpenRouter** | Single provider for embeddings (`openai/text-embedding-3-small`, 1536-dim) and LLM extraction/summarization (`openai/gpt-4o-mini`) |
| **Authelia** (`auth.x1024.net`) | OIDC identity provider for both MCP clients and the web UI |

### Required configuration (environment)

| Variable | Required | Purpose |
|---|---|---|
| `DATABASE_URL` | yes | App connection (non-superuser `app_user` role — required for RLS) |
| `OPENROUTER_API_KEY` | yes | Embeddings + LLM extraction |
| `AUTHELIA_ISSUER_URL` | yes | OIDC discovery |
| `OIDC_CLIENT_ID` | yes | OAuth client id advertised to MCP clients |
| `ENRICHMENT_DATABASE_URL` | no | Dedicated `BYPASSRLS` connection for the background enrichment worker; without it, cross-user enrichment fails with SQLSTATE 42501 |
| `PORT` | no | HTTP port (default `8080`) |

---

## 3. Core Concepts & Data Model

### 3.1 The unified `entries` table

The canonical memory unit. Every captured record — a thought, a link, a contact,
a conversation summary — is a row here.

| Column | Notes |
|---|---|
| `id` | UUID PK |
| `user_id` | FK → `mcp_users(id)`, `ON DELETE CASCADE`; drives RLS |
| `record_type` | e.g. `note.thought`, `note.link`, `crm.contact`, `conversation.summary` |
| `schema_version` | default `1.0.0` |
| `source` | `web` \| `mcp` \| tool name \| `migrated` |
| `confidence` | classifier confidence (double) |
| `failure_mode` | `low_confidence` \| `validation_failure` \| NULL |
| `content_text` | the embeddable natural-language summary |
| `payload` | typed JSONB body, validated against a JSON Schema |
| `tags` | JSONB array |
| `entities` | JSONB (people / orgs / dates; for conversations: session metadata) |
| `embedding` | `VECTOR(1536)` |
| `created_at`, `updated_at` | timestamps |
| `deleted_at` | soft delete |

**Indexes:** HNSW cosine (`vector_cosine_ops`) on `embedding`; GIN on
`payload`/`tags`/`entities`; btree on `(user_id, record_type)` and
`(user_id, created_at)`. **RLS enabled** with a null-guarded `USING` + `WITH CHECK`
policy on `app.current_user_id`.

### 3.2 Record types

Registered JSON Schemas (embedded in the binary, `brain/schemas/<type>@<version>.json`):

- `note.thought` — freeform thought/note
- `note.link` — a URL with fetched title/description and enrichment status
- `note.unstructured` — the fallback type for low-confidence / validation failures
- `crm.contact`, `crm.interaction`
- `jobhunt.application`
- `maintenance.task`
- `conversation.summary` / `conversation.chunk` — live-captured AI conversations

### 3.3 Supporting core tables

| Table | RLS | Purpose |
|---|---|---|
| `mcp_users` | **no** | Identity anchor: `id`, `name`, `oidc_subject` (UNIQUE). Every other table's `user_id` FKs here. Looked up at auth time before user context exists. |
| `api_tokens` | **no** | Personal Access Tokens: `token_hash` (SHA-256, UNIQUE), `last_used_at`, `revoked_at`. Looked up by hash at auth time; handlers filter by `user_id` explicitly. |
| `captured_sessions` | yes | Tracking for live-captured conversations: composite PK `(user_id, tool, session_id)`, `chunked_msg_count` (append-only high-water mark), `summary_entry_id`, session timestamps. |
| `thoughts` | yes | **Legacy** memory primitive (`content`, `embedding`, `metadata`). Superseded by `entries` but still present; read by `search_thoughts`/`list_thoughts`/`thought_stats`. |

---

## 4. Multi-User Isolation (RLS)

Isolation is uniform across core and every extension:

1. Every user-owned table has `user_id UUID NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE` and `ENABLE ROW LEVEL SECURITY`.
2. Each table has a policy `USING (user_id = current_setting('app.current_user_id')::uuid)`. Core tables additionally guard against a NULL setting and add `WITH CHECK` on writes.
3. The server sets context **per transaction**: `App.WithUserTx` opens a pgx transaction and runs `SET LOCAL app.current_user_id = '<uuid>'` before the callback. `SET LOCAL` scopes the setting to that transaction only. Every service persist and every extension handler goes through `WithUserTx`, so RLS is always active.
4. **Role separation is mandatory.** `DATABASE_URL` connects as a non-superuser `app_user` (superusers bypass RLS). A separate `enrichment_worker` role has `BYPASSRLS` and is granted only `SELECT, UPDATE ON entries` — used exclusively by the background enrichment worker via `WithAdminTx` (`SET LOCAL row_security = off` on a dedicated admin pool).

The authenticated user id originates from a trusted OIDC/token lookup (never
from user input), so it is formatted directly into the `SET LOCAL` statement.

---

## 5. Authentication

Substrate supports two auth paths, both resolving to a `user_id` injected into the
request context.

### 5.1 Bearer token middleware (`/mcp`, `/ingest`)

`Authorization: Bearer <token>` branches on prefix:

- **Personal Access Token** — prefix `substrate_pat_`, SHA-256 hashed, looked up in `api_tokens`; `last_used_at` is touched (throttled to 5 min). Used by headless clients and the capture daemon.
- **OIDC token** — any other token is verified against Authelia's **userinfo endpoint** (works for both JWT and opaque tokens), yielding the `sub` claim, mapped to a user via `mcp_users.oidc_subject`.

### 5.2 OAuth/OIDC proxy for MCP clients

MCP clients use ephemeral `http://localhost:PORT/callback` redirect URIs that
Authelia cannot pre-register. Substrate therefore **advertises itself as the OAuth
authorization server** (`issuer: https://substrate.x1024.net`) and proxies to
Authelia:

- `GET /.well-known/oauth-authorization-server` — RFC 8414 metadata.
- `POST /oauth/register` — RFC 7591 dynamic client registration (returns the pre-configured `OIDC_CLIENT_ID`).
- `GET /oauth/authorize` — stores the client's original `redirect_uri` in an in-memory map keyed inside Authelia's `state` param (10-min TTL), substitutes Substrate's fixed callback.
- `GET /oauth/callback` — unwraps state, forwards the code to the client's localhost redirect.
- `POST /oauth/token` — swaps the redirect URI back and forwards the token exchange to Authelia. Public client, PKCE S256, `token_endpoint_auth_method: none`.

### 5.3 Web session cookies (browser UI)

A separate, direct OIDC flow (bypassing the MCP proxy):

- `GET /web/login` — PKCE (S256) + state, redirects to Authelia (`scope=openid offline_access`).
- `GET /web/callback` — exchanges code directly, verifies token, looks up user, creates a server-side session, sets an httpOnly/Secure/SameSite=Lax `substrate_session` cookie (30-day TTL).
- `webAuthMiddleware` validates the cookie and injects the user id for RLS. Sessions live in an in-memory store with hourly cleanup — they do **not** survive a server restart.

---

## 6. Capture & Recall Pipelines

### 6.1 Two capture surfaces, one orchestrator

Both the web `POST /capture` and the MCP `add_item` tool converge on
`EntryService`:

- **`Capture(text, source)`** (classification unknown): Extract → validate → embed → insert.
- **`CaptureTyped(recordType, text, source)`** (type known, e.g. from `add_item`): skips classification.

### 6.2 Enrichment / classification (`brain/extractor.go`)

A **two-tier classifier**:

1. **Deterministic pattern rules** — regexes for email/phone/URL plus
   maintenance/job keyword vocabularies, and an explicit `link:` prefix
   (confidence 1.0). If confidence ≥ threshold (`SUBSTRATE_EXTRACTION_MIN_CONFIDENCE`,
   default 0.7), an envelope is built cheaply: `note.thought` skips the LLM
   entirely, `note.link` fetches synchronously, other types run a **constrained**
   LLM field-extraction prompt against the JSON Schema.
2. **LLM one-shot extraction** — full classify + extract via `gpt-4o-mini` with a
   system prompt describing every record type. On any failure it degrades to a
   `note.thought` at confidence 0.5 (below threshold → stored as
   `note.unstructured`).

### 6.3 Validation (`brain/validator.go`)

JSON Schemas are pre-compiled at init. `ValidateEnvelope` checks required fields
→ confidence threshold → schema existence → payload conformance, returning a
`FailureMode` (`low_confidence` | `validation_failure`). On failure,
`FallbackPayload` builds a `note.unstructured` payload preserving `raw_input`,
`attempted_record_type`, `failure_mode`, and `rejection_reason`.

### 6.4 Link enrichment (`brain/fetcher.go`)

- **On capture**, a `note.link` GETs the URL (512 KB cap), extracts
  og:title/`<title>` and og:description, and sets `fetch_status` /
  `extract_status`.
- **Background `EnrichmentWorker`** loops every `SUBSTRATE_ENRICHMENT_INTERVAL`
  (default 10 min) using `WithAdminTx` (cross-user scan). It retries pending
  links, fetches full body text (strips HTML, rejects non-text content types,
  truncates on rune boundary), and **regenerates embeddings** so links become
  richer semantic-search targets over time. Non-text content and invalid UTF-8
  are skipped/scrubbed.

### 6.5 Recall (semantic search)

- **`search`** (unified) — embeds the query, cosine similarity
  `1 - (embedding <=> query)` over all `entries`, default threshold 0.4, optional
  `record_type` and `since` filters. Each hit is rendered with a compact
  per-type human summary.
- **`search_thoughts`** — same mechanism scoped to `note.thought`, threshold 0.5.

---

## 7. MCP Tools

Substrate runs a single MCP server (`substrate` 1.0.0) served over **Streamable
HTTP** at `/mcp`. Registered tools:

**Core:**
- `add_item` — unified typed capture. `type` aliases map short names to record types (`thought`→`note.thought`, `contact`→`crm.contact`, `interaction`→`crm.interaction`, `maintenance`/`task`→`maintenance.task`, `job`/`application`→`jobhunt.application`, `link`→`note.link`). Replaces the old per-type capture tools.
- `search` — unified semantic search across all record types.
- `search_thoughts`, `list_thoughts`, `thought_stats` — thought-scoped read tools.

**household:** `add_household_item`, `search_household_items`, `add_vendor`, `list_vendors`

**calendar:** `add_family_member`, `add_activity`, `get_week_schedule`, `search_activities`, `add_important_date`, `get_upcoming_dates`

**meals:** `add_recipe`, `search_recipes`, `update_recipe`, `create_meal_plan`, `get_meal_plan`, `generate_shopping_list`

> **Migration note:** the `crm`, `jobhunt`, and `maintenance` extension packages
> exist, compile, and their tables are created — but their MCP tools are **not
> registered** in `main.go`. These domains are captured through the unified
> `add_item`/`search` path (via type aliases + JSON Schemas) instead. This is a
> deliberate, in-progress transition from dedicated per-domain tools toward the
> canonical `entries` store, not dead code.

---

## 8. Extensions

Two persistence approaches coexist:

1. **Legacy per-extension tables** — a package with `Register(s, app)` adding
   MCP tools whose handlers write raw SQL (inside `WithUserTx`), backed by a
   co-located `schema.sql`.
2. **The canonical `entries` store** — typed JSONB payloads validated against
   JSON Schemas, reached via `add_item`/`search`.

### Live, tool-backed extensions

| Extension | Domain | Key tables |
|---|---|---|
| **household** | Home knowledge base + service-provider directory | `household_items`, `household_vendors` (rating 1–5) |
| **calendar** | Family/multi-person scheduling + recurring dates | `family_members`, `activities` (one-time/recurring), `important_dates` (yearly re-projection) |
| **meals** | Recipes, weekly meal plans, shopping lists | `recipes` (JSONB ingredients/instructions), `meal_plans` (upsert on unique week/day/meal), `shopping_lists` (auto-generated from planned recipes) |

### Schema present, tools not yet wired (mid-migration)

| Extension | Domain | Notable |
|---|---|---|
| **maintenance** | Recurring/one-time home maintenance + logs | `log_maintenance` auto-recomputes `next_due = completed + frequency_days` |
| **crm** | Professional contacts, interactions, opportunity pipeline | `get_contact_history` returns profile + interactions + opportunities; `link_thought_to_contact` bridges into core memory |
| **jobhunt** | End-to-end job search (5 tables) | companies → postings → applications → interviews, plus `job_contacts` soft-linked to CRM; `get_pipeline_overview` dashboard |

### Adding a new extension

**Legacy tool pattern:**
1. `extensions/<name>/<name>.go` with `Register(s, a)` adding tools that use `a.WithUserTx` and insert `user_id` via `current_setting('app.current_user_id')::uuid`.
2. `extensions/<name>/schema.sql`: tables with the `user_id` FK, RLS enabled, an RLS policy, per-user indexes, and the `update_updated_at` trigger if mutable.
3. Mount the schema in `docker-compose.yml` as a numbered init file (core is `10-`, extensions `20–25`, grants `90-`).
4. Call `<name>.Register(s, app)` in `main.go`.

**Canonical-store pattern:** add `brain/schemas/<type>@1.0.0.json`, register a
type alias in `core/add_item.go`, and capture flows through `entries` with zero
new tables or tools.

> **Operational gotcha:** docker init scripts run **only on a fresh DB volume**.
> Schema changes to an existing database require migrations
> (`docker/migrate-*.sql`) or a wiped test volume. There is no automatic
> migration runner.

---

## 9. Live Conversation Capture

### 9.1 The `substrate-capture` daemon (`cmd/substrate-capture/`)

A thin local shipper that watches Claude Code and Codex transcripts and POSTs
them to `/ingest`. All intelligence stays server-side.

**Pipeline:** discover files → parse to a common `Transcript` → trim/reshape to
the `/ingest` wire format → client-side dedup check → POST with a Bearer PAT.

- **Discovery:** recursive `WalkDir` over configured roots (default
  `~/.claude/projects`, `~/.codex/sessions`). Claude Code = all `*.jsonl`; Codex
  = `rollout-*.jsonl`.
- **Watching:** `fsnotify` on Claude roots (Create/Write/Rename → 2s debounce →
  full scan) plus a 30s periodic sweep as fallback. *Asymmetry:* Codex roots are
  picked up only by the periodic sweep, not event-driven.
- **Claude Code parser:** JSONL, one record per line. Only `user`/`assistant`
  records become messages; `tool_use` → `[tool: <name>]`, `tool_result` →
  `[tool result omitted: <N> bytes]`. **Subagent linking:** a Task-tool subagent
  transcript carries the parent `sessionId` plus its own `agentId`; the daemon
  promotes `agentId` to the session id and sets `ParentSessionID` to the parent,
  preferring the title from a sibling `<agent>.meta.json`.
- **Codex parser:** `session_meta` supplies session id / project / parent
  linkage; `response_item` messages rendered similarly. No per-message ids, so
  MsgID is a running sequence index (not used for dedup).
- **Trimming:** messages over 64 KB are replaced with a placeholder;
  `session_ended` is set when the file has been idle ≥ 10 min.
- **Client-side dedup:** a local JSON state file keyed `tool/session_id` skips a
  POST when both message count and a SHA-256 content hash are unchanged.
- **Fatal auth:** HTTP 401/403 stops the watch loop and exits.

**Flags/env:** `-url`/`SUBSTRATE_URL` (default `https://substrate.x1024.net`),
`SUBSTRATE_PAT` (required unless `--dry-run`), `-claude-root`, `-codex-root`,
`-machine`, `-username`, `-sweep-interval`, `-debounce`, `-ended-after`,
`-max-message-bytes`, `-dry-run`, `-backfill`, `-watch`.

### 9.2 Server ingest (`POST /ingest` → `IngestService`)

1. Read the `captured_sessions` tracking row.
2. Enforce **append-only** (reject batches shorter than `chunked_msg_count`).
3. Pack new messages into ~1500-token chunks (trailing partial held back unless the session ended).
4. Embed each chunk; insert `conversation.chunk` entries.
5. **Throttled summarization** (`shouldSummarize`: on session end, ≥6 new msgs, or >5 min since last): `gpt-4o-mini` produces a structured `{title, summary, topics, decisions, preferences, open_threads}`, embedded and stored/updated as the single `conversation.summary` entry.
6. Propagate the resolved title to all chunk entries; upsert `captured_sessions`.

Both chunks and summary carry session metadata (`tool`, `session_id`,
`parent_session_id`, `seq`, `title`, `project`, `machine`, `username`) in
`entries.entities`.

---

## 10. Web UI / PWA

The server embeds its assets (`web/{index.html, browse.html, tokens.html,
shared.css}` + `icon.png`); shared CSS and nav are inlined at startup so each
page is a single self-contained response.

### Capture (`/`)

Auto-growing textarea → `POST /capture` → `EntryService.Capture(text, "web")`.
Friendly confirmation ("Saved as a thought/link/contact…") with a "View in
Browse →" deep link. Features: Web Speech API voice dictation, ⌘/Ctrl+Enter to
submit, draft preservation in `sessionStorage` on 401, and PWA share-target
pre-fill.

### Browse (`/browse`)

An infinite-scroll feed of entry cards (pages of 50 via `GET /entries`), filter
chips per record type, and a debounced search box. Tapping a card opens a
type-specific detail view (`GET /entries/{id}`); conversation summaries
reconstruct the full transcript from their chunks.

**Full-text search (server-side):** with a query, Postgres
`websearch_to_tsquery('english', …)` runs over four weighted per-field tsvectors
computed in a shared CTE — **title** (A), **summary** (B), **topics** (C),
**body** (D) — ranked by `ts_rank` then recency. Results report **match
reasons** (which fields matched → "Matched Title · Topic…") and, on the first
page, **per-type counts** across the whole query so every chip shows a number.
`conversation.chunk` and soft-deleted rows are excluded. Without a query, results
fall back to pure recency order.

### API tokens (`/tokens.html`)

A signed-in user can create a named PAT (plaintext shown **once**, prefixed
`substrate_pat_`, only the SHA-256 hash stored), list active tokens, and revoke
them. Used to authenticate the capture daemon to `/ingest`.

### PWA / mobile (`pwa.go`)

`manifest.json` (standalone, maskable icons, **share_target** so the mobile
share sheet pushes URLs/text into capture), a minimal service worker, on-the-fly
PNG icon resizing with an in-memory cache, and mobile CSS touches (16px inputs to
prevent iOS zoom, apple-mobile-web-app meta, horizontally-scrollable chips).

---

## 11. Architecture & Layering

```
handlers / MCP tools  →  service  →  repository  →  pgx transaction
   (web.go, tools)       (logic)      (SQL)          (RLS via SET LOCAL)
```

- **`main.go` + root handlers** (`web.go`, `web_auth.go`, `pwa.go`, `api_tokens.go`, `ingest_handler.go`) — process entrypoint, HTTP/MCP server, OIDC auth, web UI/PWA, PAT issuance.
- **`brain/`** — application wiring (`app.go`), the JSON Schema registry, enrichment (`extractor.go`, `fetcher.go`, `validator.go`), OIDC helpers.
  - **`brain/repository/`** — all database access (pgx). SQL lives here.
  - **`brain/service/`** — business logic (`EntryService`, `IngestService`).
- **`core/`** — memory primitives and the unified tools (`add_item.go`, `search.go`, `thoughts.go`).
- **`extensions/<name>/`** — self-contained domain modules, each with its own `schema.sql`.
- **`cmd/substrate-capture/`** — the live-capture daemon.

**Convention:** don't reach past a layer (no raw SQL in handlers). The legacy
extensions predate this rule and embed SQL directly; the core/entries path
follows it.

---

## 12. Build, Test, Run

```bash
go build ./...        # build everything
go test ./...         # full test suite (tests live next to the code)
gofmt -l .            # formatting check (should print nothing)
go vet ./...          # static checks
```

Run the stack (secrets injected via SOPS + age):

```bash
# live stack
sops exec-env secrets/substrate.env 'docker compose up -d --build'

# isolated test stack (distinct project name, wipes its own DB volume)
sops exec-env secrets/substrate.env \
  'docker compose -p substrate-test -f docker-compose.test.yml up -d --build'
docker compose -p substrate-test -f docker-compose.test.yml down -v
```

Secrets are managed with **SOPS + age** (`secrets/substrate.env`); never commit or
print decrypted values.

---

## 13. Roadmap & In-Progress Work

| Item | Status |
|---|---|
| Live conversation capture (Claude Code + Codex) | **Implemented** |
| Web session auth (server-side httpOnly cookies) | **Implemented** (replaced client-side localStorage PKCE) |
| Full-text Browse search (match reasons, counts, ranking) | **Implemented** |
| crm / jobhunt / maintenance migration to `entries` | **In progress** — schemas + type aliases live, dedicated tools retired |
| Conversation import (`cmd/import-conversations` for Claude Desktop exports) | **Planned** — designed, not yet implemented; idempotent via a `conversation_imports` tracking table |
| Zed / Pi transcript parsers | **Planned** — tool constants exist, parsers pending store discovery |

---

## Appendix: Key Facts at a Glance

- **Embeddings:** 1536-dim `text-embedding-3-small`; HNSW cosine index; recall is `1 - (embedding <=> query)`.
- **LLM:** `gpt-4o-mini` for classification, field extraction, and conversation summarization — all via OpenRouter.
- **Transport:** MCP over Streamable HTTP at `/mcp`; a single process on `:8080`.
- **Isolation:** PostgreSQL RLS keyed on `app.current_user_id`, set per-transaction; enforced by a non-superuser DB role.
- **Two capture surfaces** (web `/capture`, MCP `add_item`) → `EntryService`; **one ingest surface** (`/ingest`) → `IngestService`.
- **Module name is `substrate`** (renamed from the historical `open-brain-go`).
