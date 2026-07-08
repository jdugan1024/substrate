# Claude Memory Refactor — Review

> Response to `CodexMemoryRefactor.md`

## Overall Assessment

This is the right long-term direction. The current table-per-domain model is already showing strain: adding new record types requires schema migrations, new tool handlers, and new search paths all in sync. A canonical `entries` table with JSONB payloads and one search path solves this cleanly.

The proposed hybrid model (canonical store + selective projections) is the correct tradeoff. Full normalization would sacrifice flexibility; pure document storage would sacrifice workflow reliability. The plan is well-scoped.

**However: do not start this refactor before the service/repository split (PR6 in `ClaudeImprovementPlan.md`) is complete.** The memory refactor assumes a clean service layer. Starting it against the current handler-heavy code will create a messy mid-migration state that's hard to reason about.

---

## Agreement

### Canonical write model

The `entries` table schema is correct. Key points of agreement:

- `record_type` as a namespaced string (`crm.contact`, `maintenance.task`) is preferable to an enum — it's extensible without migrations.
- `content_text` as a denormalized searchable narrative is essential. Semantic search over structured JSONB alone would miss too much.
- `deleted_at` for soft deletes is the right call. Hard deletes make recovery and audit impossible.
- RLS via `app.current_user_id` is consistent with the existing pattern — no new mechanism needed.

### One-shot LLM extraction

The envelope contract is well-designed. The fallback to `note.unstructured` on validation failure or low confidence is correct. Do not trust LLM output without validation — the server-side schema check is non-negotiable.

The deterministic-first routing (run rules before LLM) is important for cost control. The web capture flow already implements a version of this with `DispatchCapture`. The memory refactor should replace that call, not add a second LLM dispatch path.

### Migration strategy

The 6-phase incremental migration (dual-write → one-shot capture → unified search → projections → cutover) is the right structure. The dual-write phase is the safety net: it lets the new path run in parallel without any user-visible behavior change.

---

## Modifications

### Phase ordering: move projection model later

The plan lists projections as Phase E. Agree — but be explicit that no projection should be built until Phase D (unified search) is validated and stable. Projections built on top of an unstable canonical store become a maintenance burden.

### Schema registry: file-based first

The plan offers a choice between `entry_schemas` table and file-based registry. Use file-based (Go embedded files, similar to how `web/index.html` is embedded) for the initial implementation. Reasons:
- Schema changes are version-controlled alongside the code that validates them.
- No bootstrapping problem (the registry table needs the `entries` table to exist first).
- The `entry_schemas` table can be added later if runtime schema management becomes necessary.

Structure:
```
brain/schemas/
  note.thought@1.0.0.json
  crm.contact@1.0.0.json
  crm.interaction@1.0.0.json
  maintenance.task@1.0.0.json
  jobhunt.application@1.0.0.json
```

### `note.unstructured` schema

The fallback type is mentioned throughout but not defined in the initial 5 schemas. Add it as schema 6:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "note.unstructured",
  "type": "object",
  "required": ["content"],
  "properties": {
    "content": { "type": "string", "minLength": 1 },
    "raw_input": { "type": "string" },
    "attempted_record_type": { "type": "string" },
    "rejection_reason": { "type": "string" }
  },
  "additionalProperties": false
}
```

`rejection_reason` enables debugging and future recovery (re-classification when schemas improve).

### `crm.interaction` — loosen `contact_ref`

The proposed schema requires `person_name` but makes `contact_ref` optional. This is correct — at capture time, the contact may not yet exist in the system. However, add a `resolved_contact_id` field (nullable, populated by a post-capture lookup job) rather than embedding a soft reference in `contact_ref.external_id`. This keeps the interaction linkage clean without requiring the contact to exist at write time.

### Confidence threshold

The plan mentions "fallback if confidence below threshold" but does not specify the threshold. Use 0.7 as the default, configurable via env (`ENGRAM_EXTRACTION_MIN_CONFIDENCE`). Below 0.7: store as `note.unstructured` with the attempted type recorded. This value should be tuned after observing real extraction results.

---

## PR Prioritization

Given the dependency on the service/repository split, the recommended sequencing:

1. **First**: Complete `ClaudeImprovementPlan.md` PR6 (service/repo split) — unblocks everything below.
2. **PR1**: Entries table + indexes + RLS + file-based schema registry scaffold.
3. **PR2**: Schema registry loader + JSON Schema validator + envelope validation + tests.
4. **PR3**: Dual-write bridge for `note.thought`, `crm.contact`, `maintenance.task` — lowest risk, highest coverage.
5. **PR4**: One-shot extractor endpoint — replaces `DispatchCapture` in `brain/dispatch.go`. Feature flag `ENGRAM_SCHEMA_EXTRACTOR=true`.
6. **PR5**: Unified cross-domain search over `entries`. Keep existing search endpoints behind a flag for rollback.
7. **PR6**: First projection (recommend: upcoming maintenance tasks — small schema, clear workflow, easy to validate against legacy query).
8. **PR7**: Tool simplification — replace rigid handlers with schema-driven create/update/query.
9. **PR8**: Cutover — entries as primary write source, staged deprecation of legacy paths.

---

## Relationship to Extension Refactor

The memory refactor and extension refactor are compatible but should not be interleaved. Complete the extension registry (`ClaudeExtensionRefactor.md`) before PR7 of the memory refactor. By PR7, "tool simplification" means replacing extension-specific handlers with schema-driven ones — having the extension registry in place makes it clearer which handlers belong to which extension and which can be removed.

---

## Metrics

The proposed metrics are good. Add one more:

- `entry_schema_validation_failure_by_type_total{record_type}` — break out validation failures by type so you can see which schemas are too strict or which LLM extractions are systematically wrong for a given record type.

---

## Non-goals

- No cross-user entries (entries are always scoped by `user_id` via RLS).
- No schema evolution/migration of existing payloads — when a schema version bumps, old entries stay at their original `schema_version`. The validator must support reading entries at any known version.
- No real-time projection updates — projections are rebuilt on demand or via a background job, not via triggers.
