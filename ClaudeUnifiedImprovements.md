# Claude Unified Improvements

> Supersedes `ClaudeImprovementPlan.md`, `ClaudeExtensionRefactor.md`, and `ClaudeMemoryRefactor.md`.
> Items from those documents that conflict with, are subsumed by, or must be resequenced around the memory refactor are resolved here.

---

## What the Memory Refactor Changes

The canonical `entries` table replaces the current table-per-domain model. This has downstream effects on two other plans:

**Dispatch registry (Improvement PR7) → dropped.**
The one-shot LLM extractor with schema validation IS the dispatch registry. There is no longer a `switch`-based dispatch to replace with a `map` — the schema envelope selects the record type, and the service layer routes accordingly. Building a dispatch registry now would be building something to immediately tear out.

**Cross-tenant FK integrity (Improvement PR8) → scoped down.**
The legacy domain tables (CRM, jobhunt, maintenance) are the source of the FK integrity gaps. These tables will be retired in the memory refactor cutover. Adding DB-level constraints or triggers to tables that are scheduled for retirement is low-value. Instead: enforce ownership in the service layer during the dual-write phase, and treat the cutover as the natural fix.

**Extension `Register` signature → adapted.**
The current `Register func(s *server.MCPServer, a *brain.App)` is oriented around registering MCP tool handlers. After the memory refactor, extensions primarily contribute schemas and prompts, not handler code. The extension registry should be designed with this in mind from the start (see Phase 2).

---

## Unified Execution Plan

### Phase 0 — Safety (do immediately, no architecture dependency)

These are small, self-contained, and fix live risks.

**0a. HTTP client timeouts**

Replace all `http.DefaultClient` usage in `brain/app.go` and `main.go` with a package-level client:
```go
var httpClient = &http.Client{Timeout: 30 * time.Second}
```
Affects: `GetEmbedding`, `ExtractMetadata`, `DispatchCapture`, token proxy in `main.go`. One PR, ~10 lines changed.

**0b. Pending OAuth session TTL**

The `pendingAuths` map in `main.go` is unbounded with no expiry. Add a background goroutine that evicts entries older than 10 minutes. Cap at 500 concurrent pending sessions; reject new sessions with 503 when full. One PR.

**0c. Rate limiting**

`/capture` is auth-gated but rate-unlimited — a compromised token can run up OpenRouter costs without bound. Add per-user token bucket on `/capture` (e.g. 30 req/min) and per-IP bucket on `/oauth/*` (e.g. 20 req/min). Implement as middleware in `main.go`. One PR.

**0d. OAuth redirect allowlist**

Add `OAUTH_REDIRECT_ALLOWLIST` env var (comma-separated HTTPS URLs). Validate on startup; fatal on malformed entries. Apply `ValidateRedirectURI` in `oauthAuthorizeHandler` before storing in `pendingAuths`. This is defense in depth given the proxy pattern already constrains the attack surface. One PR.

---

### Phase 1 — Architecture Prerequisites

These must be complete before Phase 2 begins.

**1a. Service/repository split (first vertical slice)**

The memory refactor requires a clean service layer to write into. Split the capture flow first:

- `brain/repository/` — SQL only, typed query functions, no business logic
- `brain/service/` — orchestration: calls repository, embedding, metadata extraction
- Handlers in `web.go` and MCP handlers in extensions — thin: decode → call service → encode

Start with the `capture_thought` path. Extend to other tools as the memory refactor proceeds.

**1b. Extension registry**

Introduce the registry before the memory refactor adds more record types, because the memory refactor changes what "extension" means. Design it for where things are going, not where they are now.

```go
// brain/extensions/registry.go
type Definition struct {
    ID          ExtensionID
    Description string
    Schemas     []SchemaEntry      // record types this extension owns
    Register    func(s *server.MCPServer, a *brain.App) // MCP tools (optional post-refactor)
    DefaultOn   bool
    DependsOn   []ExtensionID
}

type SchemaEntry struct {
    RecordType    string
    SchemaVersion string
    Schema        []byte // embedded JSON Schema
    Prompt        string // LLM extraction instructions for this type
}
```

Extensions self-register via `init()`:
```go
// extensions/crm/extension.go
func init() {
    registry.Register(registry.Definition{
        ID:          "crm",
        Description: "Professional contacts and interactions",
        Schemas:     []registry.SchemaEntry{contactSchema, interactionSchema},
        Register:    RegisterMCPTools, // will shrink as memory refactor progresses
        DefaultOn:   true,
    })
}
```

Config: `ENGRAM_ENABLED_EXTENSIONS` (allowlist) and `ENGRAM_DISABLED_EXTENSIONS` (denylist; ENABLED wins if both set). Unset → all defaults on.

`core` is always enabled regardless of config.

`GET /-/extensions` endpoint returns enabled/disabled state and descriptions.

Add in the same PR: startup logging of enabled/disabled/unknown IDs; fatal on unknown IDs.

**1c. Interfaces for testability**

Extract two interfaces before Phase 2 adds more dependencies on them:

```go
type EmbeddingProvider interface {
    GetEmbedding(ctx context.Context, text string) (pgvector.Vector, error)
}

type TokenVerifier interface {
    Verify(ctx context.Context, rawToken string) (string, error)
}
```

`App` satisfies `EmbeddingProvider`. `OIDCVerifier` satisfies `TokenVerifier`. This unblocks unit testing of the extractor and service layer without network dependencies.

---

### Phase 2 — Memory Refactor: Foundation

**2a. Entries table + indexes + RLS**

```sql
CREATE TABLE IF NOT EXISTS entries (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
    record_type     TEXT        NOT NULL,
    schema_version  TEXT        NOT NULL DEFAULT '1.0.0',
    source          TEXT        NOT NULL DEFAULT 'web',
    confidence      DOUBLE PRECISION,
    content_text    TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    tags            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    entities        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    embedding       VECTOR(1536),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);
```

Indexes, RLS policy, and `updated_at` trigger as specified in `ClaudeMemoryRefactor.md`. No `entry_schemas` DB table — use file-based registry (see below).

**2b. File-based schema registry**

Embed JSON Schemas alongside extension definitions:

```
brain/extensions/
  core/
    note.thought@1.0.0.json
    note.unstructured@1.0.0.json
  crm/
    crm.contact@1.0.0.json
    crm.interaction@1.0.0.json
  maintenance/
    maintenance.task@1.0.0.json
  jobhunt/
    jobhunt.application@1.0.0.json
```

The `SchemaEntry` in each extension's `Definition` holds the embedded bytes. The registry loads and validates all schemas at startup.

Initial 6 schemas (5 from original plan + `note.unstructured`):

`note.unstructured` additions beyond the original:
```json
{
  "attempted_record_type": { "type": "string" },
  "rejection_reason": { "type": "string" }
}
```
This enables future re-classification when extraction improves.

**2c. Envelope validator**

Server-side validation of LLM output before any write:
- `record_type` must exist in registry for the user's enabled extensions
- `schema_version` must match a known version for that type
- `payload` must pass the JSON Schema for that version
- On failure or `confidence < ENGRAM_EXTRACTION_MIN_CONFIDENCE` (default 0.7): store as `note.unstructured`

No LLM output is written to `entries` without passing this gate.

---

### Phase 3 — Memory Refactor: Dual-Write

**3a. Dual-write bridge**

Keep all existing handlers and tables. On each successful write to the legacy table, also write a canonical `entries` record. Start with:
- `capture_thought` → `note.thought`
- CRM contact create → `crm.contact`
- Maintenance task create → `maintenance.task`

Add observability: `dual_write_success_total`, `dual_write_failure_total` (by record type), `schema_validation_failure_total` (by record type).

Any dual-write failure is logged but does not fail the primary write. The legacy path remains authoritative during this phase.

**3b. One-shot extractor**

Replace `DispatchCapture` in `brain/dispatch.go` with the schema-envelope extractor:

- One LLM call to `gpt-4o-mini` with `response_format: json_object`
- System prompt constructed from the registry: today's date + all enabled record types with their `Prompt` field and trigger conditions
- Output: the full envelope (`record_type`, `schema_version`, `payload`, `content_text`, `tags`, `entities`, `confidence`)
- Validate envelope → write to `entries` → dual-write to legacy table (in that order; legacy write failure non-fatal)
- Feature flag: `ENGRAM_SCHEMA_EXTRACTOR=true` (default false during rollout)

This is where the dispatch registry concern fully dissolves — the extractor is the router.

---

### Phase 4 — Memory Refactor: Unified Search

**4a. Cross-domain search over `entries`**

New search method: semantic similarity over `entries.embedding` + filters by `record_type`, `tags`, date range. Returns entries sorted by cosine similarity.

Keep existing domain-specific search endpoints for rollback. Route new search queries to `entries` first; fall back to legacy if `entries` returns no results (during dual-write phase, coverage is partial).

**4b. Read path for MCP tools**

Update MCP read tools (recall, search) to query `entries` as the primary source when `ENGRAM_SCHEMA_EXTRACTOR=true`. Extension-specific read tools become formatters over filtered entry results rather than domain-table queries.

---

### Phase 5 — Projections

Build selective projections only for strict workflow queries that require structured access (not just retrieval):

- **Upcoming maintenance**: `SELECT payload->>'next_due', payload->>'name' FROM entries WHERE record_type = 'maintenance.task' AND deleted_at IS NULL ORDER BY (payload->>'next_due')::date`
- **Active job applications**: filter by `payload->>'status'` not in `('rejected','withdrawn','accepted')`

These can be implemented as views over `entries` rather than separate tables. Materialize only if query performance requires it.

Build projections only after Phase 4 is stable and has been running in production for at least two weeks.

---

### Phase 6 — Cutover and Cleanup

**6a. Entries as primary write source**

Switch write path: `entries` is written first and is authoritative. Legacy domain tables become projections populated from `entries` (or are retired).

Retire legacy tables in order of lowest risk:
1. `maintenance_tasks` — smallest schema, easiest to validate parity
2. CRM tables — validate interaction linkage first
3. Jobhunt tables — most complex, retire last

**6b. Extension simplification**

By this phase, the `Register func(s *server.MCPServer, a *brain.App)` in each extension `Definition` is minimal or empty. MCP tools become thin wrappers around the unified service layer. Remove handler code that has been replaced.

**6c. Legacy table removal**

After two weeks of stable `entries`-primary operation with no parity issues, drop the legacy tables. Include a migration that verifies row counts are equivalent before dropping.

**6d. Integration tests**

Before any cutover deployment:
- RLS: verify user A cannot read user B's entries
- Schema validation matrix: valid payloads pass, invalid fail, low-confidence falls back
- OAuth callback validation: loopback passes, allowlisted HTTPS passes, arbitrary HTTPS rejected
- Token refresh: expired token silently refreshes on next `/capture` call

---

## What's Dropped

| Original item | Reason dropped |
|---|---|
| Improvement PR7: Dispatch registry | Superseded by schema-envelope extractor in Phase 3 |
| Improvement PR8: Cross-tenant FK integrity (DB-level) | Legacy tables retired in Phase 6; service-layer ownership check during dual-write is sufficient |
| Memory refactor `entry_schemas` DB table | File-based registry is simpler, version-controlled, no bootstrapping problem |
| Improvement PR5: `EmbeddingProvider` interface via field injection | Package-level `httpClient` + interface extraction (Phase 1c) is sufficient |

---

## Summary Timeline

| Phase | Contents | Dependency |
|-------|----------|------------|
| 0 | HTTP timeouts, OAuth TTL, rate limiting, redirect allowlist | None — do now |
| 1 | Service/repo split, extension registry, interfaces | Phase 0 done |
| 2 | Entries table, file schema registry, envelope validator | Phase 1 done |
| 3 | Dual-write, one-shot extractor (flagged) | Phase 2 done |
| 4 | Unified search, MCP read path | Phase 3 stable |
| 5 | Projections | Phase 4 stable (2+ weeks) |
| 6 | Cutover, legacy table removal, extension simplification | Phase 5 validated |
