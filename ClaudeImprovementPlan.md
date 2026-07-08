# Claude Improvement Plan — Review

> Response to `CodexImprovementPlan.md`

## Overall Assessment

The plan is sound. All 10 PRs address real weaknesses in the current codebase. Priority order below reflects risk-adjusted value: security items first, then reliability, then testability.

---

## Status Update

**PR1 (OAuth callback policy) is partially superseded.** The OAuth proxy pattern has already been implemented: engram now acts as the authorization server from the client's perspective, proxying to Authelia with a fixed registered callback. This eliminates the redirect_uri open-redirect risk at the architectural level.

What remains from PR1:
- The `pendingAuths` map still lacks TTL eviction (covered in PR3).
- Sensitive logging (full forwarded URLs) should still be audited.
- `OAUTH_REDIRECT_ALLOWLIST` is not yet implemented — but the proxy pattern means only loopback callbacks flow through the map. Still worth adding for defense in depth.

---

## Revised Priority Order

### Tier 1 — Do First (Security + Stability)

**PR3: Pending OAuth session lifecycle controls**

The `pendingAuths` map is currently unbounded and has no TTL. This is the highest-risk item still open. A targeted goroutine with a 10-minute TTL and a cap of 1000 entries is the right fix. Reject with 503 when full.

**PR4: HTTP client safety**

`http.DefaultClient` with no timeout is used in three places (`GetEmbedding`, `ExtractMetadata`, `DispatchCapture`, and the token proxy). A slow OpenRouter response will block a goroutine indefinitely. A shared client with `Timeout: 30s` is a one-line fix per call site — do this before anything else that adds more HTTP calls.

**PR9: Rate limiting**

`/capture` is now publicly accessible (with auth). Without rate limiting, a compromised token can run up OpenRouter costs unboundedly. Per-user rate limiting on `/capture` and per-IP limiting on `/oauth/*` are both needed. Start with a simple token bucket in middleware.

---

### Tier 2 — Do Soon (Correctness + Testability)

**PR8: Cross-tenant FK integrity**

The CRM and jobhunt tables have referential integrity gaps across user boundaries. This is exploitable if RLS ever has a gap. Add `user_id` to FK constraints or use triggers. Prioritize `contact_interactions → professional_contacts` and `job_applications → job_postings → job_companies`.

**PR5: App interfaces for composability**

`EmbeddingProvider` and `TokenVerifier` interfaces would make `dispatch.go` and `oidc.go` independently testable. Keep the concrete implementations; just introduce the interface boundary. This unblocks unit testing without requiring full Postgres/OpenRouter availability.

**PR2: Config and bootstrap for allowlist**

`OAUTH_REDIRECT_ALLOWLIST` validation on startup is straightforward. Include this with any deployment that exposes the OAuth proxy to external clients.

---

### Tier 3 — Architectural (Plan Ahead)

**PR6: Service/repository split**

The first vertical slice (capture flow) is a good choice — it's the highest-traffic path and already has the most logic accumulation. The split is not urgent but will pay off before the memory refactor (see `ClaudeMemoryRefactor.md`), since that refactor assumes a clean service layer to write into.

**PR7: Dispatch registry**

The current `switch` in `web.go:executeDispatch` is already getting long. As more tools are added (especially via the memory refactor), a `map[DispatchTool]Handler` pattern will prevent the switch from becoming unmaintainable. Recommended before adding more than 2–3 new tool types.

**PR1 remainder: OAuth redirect allowlist**

Add `ValidateRedirectURI` as described, backed by `OAUTH_REDIRECT_ALLOWLIST`. Given the proxy pattern already constrains the attack surface, this is defense in depth rather than a critical gap.

**PR10: Integration tests + rollout hardening**

The RLS + ownership integrity test matrix is the most valuable item here. Add it alongside PR8 so the constraints have coverage. The deployment checklist can be maintained in `docs/`.

---

## Modifications to Specific PRs

### PR4 — HTTP client safety

The plan says "inject shared `*http.Client`." Prefer a package-level `var httpClient = &http.Client{Timeout: 30 * time.Second}` in `brain/app.go` rather than field injection into `App`. This is simpler and sufficient since all callers are in the same package.

### PR5 — Interfaces

For `TokenVerifier`, the interface already exists conceptually in `brain/oidc.go`. The change is just extracting it. For `EmbeddingProvider`, keep the method on `App` and extract the interface when the first mock is needed in a test — not before.

### PR6 — Service/repository split

Keep the HTTP handlers in `web.go` and `main.go`. The split should be:
- `brain/repository/` — SQL only, no business logic
- `brain/service/` — orchestration (call embedding + metadata, then repository)
- handlers — thin: decode request, call service, encode response

Don't split before the dispatch registry (PR7) is in place, or the two refactors will conflict.

### PR8 — Cross-tenant FK integrity

Do not add `user_id` to every FK. Instead, enforce ownership at the parent level and validate in write paths. Example: when inserting a `contact_interaction`, the handler already has the user's ID — check that the referenced `contact_id` belongs to that user in the same transaction (SELECT FOR UPDATE pattern). Reserve DB-level triggers for cases where the write path cannot be controlled.

---

## Non-goals Confirmed

- No per-user feature flags (keep simple).
- No distributed rate limiting (single-process is fine for now).
- No API versioning.
