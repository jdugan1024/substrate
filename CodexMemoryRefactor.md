# Codex Memory Refactor

## Summary

You are proposing a shift from rigid, table-specific tool handlers to a schema-driven memory model where most records are represented as validated JSON in PostgreSQL (`JSONB`), with semantic search across all concerns from one canonical store.

This direction is sound and aligns with your current pain points:
- tools are too rigid and expensive to evolve,
- fuzzy matching across many domain tables is hard,
- LLM extraction logic is currently scattered and partially duplicated.

Recommended approach: **hybrid model**
- Use one canonical `entries` table as the source of truth.
- Store typed payloads in `JSONB`, validated by JSON Schemas.
- Use one-shot LLM extraction for ambiguous/unstructured inputs.
- Keep deterministic code paths for explicit/form-like inputs.
- Add selective projection tables/materialized views only where strict workflow/query performance is needed.

This keeps flexibility high while preserving operational reliability.

---

## Detailed Plan

### 1) Target Architecture

#### Canonical write model
- Every captured item becomes an `entry` with:
  - `record_type` (e.g. `crm.contact`, `maintenance.task`),
  - `payload` (`JSONB`, schema-validated),
  - `content_text` (canonical searchable narrative),
  - optional `tags`, `entities`, `confidence`,
  - `embedding` for semantic search.

#### Schema-driven behavior
- Define one JSON Schema per `record_type` and version it.
- LLM output is required to fit a strict envelope (below).
- Server always validates before write.

#### Retrieval model
- Fuzzy search and cross-domain filtering query `entries` first.
- Domain-specific tools can become formatters over filtered entry results.

#### Projection model (optional, selective)
- For high-value strict workflows (e.g. “upcoming interviews”, “maintenance due”), maintain projection tables or materialized views derived from `entries`.

---

### 2) One-Shot LLM Output Contract

Use one LLM call for extraction/classification when input is unstructured.

```json
{
  "record_type": "crm.contact",
  "schema_version": "1.0.0",
  "payload": {
    "name": "Ada Lovelace",
    "company": "Analytical Engines Ltd",
    "title": "Research Lead",
    "notes": "Met at AI meetup"
  },
  "content_text": "Met Ada Lovelace (Research Lead at Analytical Engines Ltd) at an AI meetup. Add as professional contact.",
  "tags": ["crm", "networking"],
  "entities": {
    "people": ["Ada Lovelace"],
    "orgs": ["Analytical Engines Ltd"],
    "dates": []
  },
  "confidence": 0.89
}
```

Rules:
- `record_type` must exist in registry.
- `schema_version` must match known schema major/minor compatibility rules.
- `payload` must pass JSON Schema validation.
- If validation fails or confidence below threshold, fallback to `note.unstructured`.

---

### 3) SQL Draft (Canonical Storage)

```sql
-- 3.1 Canonical entries table
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

-- 3.2 Useful indexes
CREATE INDEX IF NOT EXISTS idx_entries_user_id            ON entries(user_id);
CREATE INDEX IF NOT EXISTS idx_entries_record_type        ON entries(user_id, record_type);
CREATE INDEX IF NOT EXISTS idx_entries_created_at         ON entries(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_entries_payload_gin        ON entries USING gin(payload);
CREATE INDEX IF NOT EXISTS idx_entries_tags_gin           ON entries USING gin(tags);
CREATE INDEX IF NOT EXISTS idx_entries_entities_gin       ON entries USING gin(entities);
CREATE INDEX IF NOT EXISTS idx_entries_embedding_hnsw     ON entries USING hnsw (embedding vector_cosine_ops);

-- 3.3 RLS
ALTER TABLE entries ENABLE ROW LEVEL SECURITY;

CREATE POLICY entries_rls ON entries
    FOR ALL
    USING (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    );

-- 3.4 updated_at trigger
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS entries_updated_at ON entries;
CREATE TRIGGER entries_updated_at
    BEFORE UPDATE ON entries
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();

-- 3.5 Optional schema registry table (if not file-based)
CREATE TABLE IF NOT EXISTS entry_schemas (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    record_type     TEXT        NOT NULL,
    schema_version  TEXT        NOT NULL,
    json_schema     JSONB       NOT NULL,
    instructions    TEXT        NOT NULL,
    active          BOOLEAN     NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (record_type, schema_version)
);
```

---

### 4) First 5 JSON Schemas to Start

Use these as initial record types to validate the model across simple and relational workflows.

#### 4.1 `note.thought@1.0.0`
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "note.thought",
  "type": "object",
  "required": ["content"],
  "properties": {
    "content": { "type": "string", "minLength": 1 },
    "topics": { "type": "array", "items": { "type": "string" }, "default": [] },
    "people": { "type": "array", "items": { "type": "string" }, "default": [] },
    "action_items": { "type": "array", "items": { "type": "string" }, "default": [] },
    "dates_mentioned": { "type": "array", "items": { "type": "string", "format": "date" }, "default": [] },
    "thought_type": { "type": "string", "enum": ["observation", "task", "idea", "reference", "person_note"] }
  },
  "additionalProperties": false
}
```

#### 4.2 `crm.contact@1.0.0`
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "crm.contact",
  "type": "object",
  "required": ["name"],
  "properties": {
    "name": { "type": "string", "minLength": 1 },
    "company": { "type": "string" },
    "title": { "type": "string" },
    "email": { "type": "string", "format": "email" },
    "phone": { "type": "string" },
    "linkedin_url": { "type": "string", "format": "uri" },
    "how_we_met": { "type": "string" },
    "tags": { "type": "array", "items": { "type": "string" }, "default": [] },
    "notes": { "type": "string" }
  },
  "additionalProperties": false
}
```

#### 4.3 `crm.interaction@1.0.0`
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "crm.interaction",
  "type": "object",
  "required": ["person_name", "interaction_type", "summary", "interaction_date"],
  "properties": {
    "person_name": { "type": "string", "minLength": 1 },
    "interaction_type": { "type": "string", "enum": ["meeting", "call", "coffee", "email", "conference", "linkedin", "other"] },
    "summary": { "type": "string", "minLength": 1 },
    "follow_up_needed": { "type": "boolean", "default": false },
    "follow_up_notes": { "type": "string" },
    "interaction_date": { "type": "string", "format": "date" },
    "contact_ref": {
      "type": "object",
      "properties": {
        "entry_id": { "type": "string", "format": "uuid" },
        "external_id": { "type": "string" }
      },
      "additionalProperties": false
    }
  },
  "additionalProperties": false
}
```

#### 4.4 `maintenance.task@1.0.0`
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "maintenance.task",
  "type": "object",
  "required": ["name"],
  "properties": {
    "name": { "type": "string", "minLength": 1 },
    "category": { "type": "string" },
    "location": { "type": "string" },
    "frequency_days": { "type": "integer", "minimum": 1 },
    "next_due": { "type": "string", "format": "date" },
    "last_completed": { "type": "string", "format": "date" },
    "notes": { "type": "string" }
  },
  "additionalProperties": false
}
```

#### 4.5 `jobhunt.application@1.0.0`
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "jobhunt.application",
  "type": "object",
  "required": ["company_name", "title", "status", "applied_date"],
  "properties": {
    "company_name": { "type": "string", "minLength": 1 },
    "title": { "type": "string", "minLength": 1 },
    "posting_url": { "type": "string", "format": "uri" },
    "status": { "type": "string", "enum": ["applied", "screening", "interviewing", "offer", "accepted", "rejected", "withdrawn"] },
    "applied_date": { "type": "string", "format": "date" },
    "resume_version": { "type": "string" },
    "referral_contact": { "type": "string" },
    "salary_min": { "type": "integer", "minimum": 0 },
    "salary_max": { "type": "integer", "minimum": 0 },
    "notes": { "type": "string" }
  },
  "additionalProperties": false
}
```

---

### 5) Ingestion Decision Tree (Token/Cost Aware)

For each incoming note:
1. Run deterministic parser/rules first (cheap path).
2. If unambiguous, build payload directly in Go and validate.
3. If ambiguous, run one-shot LLM extraction into envelope.
4. Validate envelope + payload.
5. On failure/low confidence, store as `note.unstructured` with raw content.

This keeps token use controlled while preserving flexibility.

---

### 6) Incremental Migration Strategy

#### Phase A: Foundation
- Add `entries` table and indexes.
- Add schema registry (table or files).
- Build server-side schema validator and envelope validator.

#### Phase B: Dual-write (no behavior break)
- Keep existing extension tools and tables.
- On successful existing writes, also write canonical `entries`.
- Start with `note.thought`, `crm.contact`, `maintenance.task`.

#### Phase C: One-shot capture path
- Replace current dispatch path for web capture with one-shot schema envelope extraction.
- Keep old path behind feature flag fallback.

#### Phase D: Unified search/read
- Route semantic search to `entries` first.
- Add filters by `record_type`, tags, and date from one query path.

#### Phase E: Projectionization
- Add selective projections/views only for strict workflows.
- Keep projection build idempotent and rebuildable from canonical entries.

#### Phase F: Cutover
- Make `entries` primary write source.
- Gradually retire redundant table-specific writes.

---

### 7) PR-by-PR Execution Board

#### PR1: Canonical schema + storage
- Add `entries` SQL migration + RLS + indexes.
- Add `entry_schemas` storage (or file loader scaffold).
- Add migration tests/smoke checks.

#### PR2: Schema registry + validation engine
- Add record-type registry in Go.
- Add JSON Schema validator integration.
- Add envelope validation logic and tests.

#### PR3: Dual-write bridge
- Add `entries` write from existing handlers for first 3 types.
- Add observability (`dual_write_success_rate`, validation failures).

#### PR4: One-shot extractor endpoint/service
- Add extraction prompt + strict JSON envelope output.
- Add deterministic-first router and fallback behavior.
- Feature flag: `ENABLE_SCHEMA_EXTRACTOR=true`.

#### PR5: Unified cross-domain search
- Add new search method against `entries` with semantic + metadata filters.
- Keep old search endpoint for rollback.

#### PR6: First projection workflow
- Implement one projection (recommended: upcoming maintenance or interviews).
- Compare projection results with legacy query for parity.

#### PR7: Tool simplification
- Replace selected rigid tool handlers with schema-driven create/update/query handlers.
- Keep compatibility aliases for existing tool names.

#### PR8: Cutover and cleanup
- Switch primary write path to `entries`.
- Deprecate legacy write paths in staged rollout.
- Document operational runbook + rollback.

---

### 8) Guardrails and Success Metrics

#### Guardrails
- Server-side JSON Schema validation is mandatory for all schema-driven writes.
- Confidence threshold + fallback to `note.unstructured`.
- No direct trust of LLM output without validation.
- Keep deterministic parser in front of LLM for cost control.

#### Suggested metrics
- `extractor_invocations_total`
- `extractor_validation_failures_total`
- `extractor_fallback_total`
- `entry_write_success_total`
- `cross_domain_search_latency_ms`
- `% of writes using deterministic path vs LLM path`

---

## Recommendation

Adopt **`entries` as source of truth** with selective projections.

This directly solves your rigidity and cross-domain retrieval pain while preserving reliability where strict workflows matter.
