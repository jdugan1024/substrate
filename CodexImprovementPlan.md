# Codex Improvement Plan

Chosen OAuth policy: **Option 2** - allow loopback callbacks plus an explicit allowlist of trusted HTTPS callback URLs.

## Execution Checklist

### 1) PR1: OAuth callback policy hardening (Option 2)
- Implement `ValidateRedirectURI(redirect string) error` in a dedicated auth/security package (e.g. `internal/security/oauthpolicy`).
- Policy rules:
  - Allow `http://localhost:<port>` and `http://127.0.0.1:<port>` only (loopback).
  - Allow `https://...` only if exact match in `OAUTH_REDIRECT_ALLOWLIST`.
  - Deny all other schemes/hosts/ports.
- Apply validation in `/oauth/authorize` before storing `pendingAuths`.
- On callback, re-validate the stored redirect before forwarding.
- Remove sensitive logging:
  - Do not log full forwarded URLs (contains auth code).
  - Log only host/path + mapping key.
- **Acceptance:** malicious redirects are rejected; loopback + allowlisted HTTPS succeed.

### 2) PR2: Config and bootstrap for allowlist
- Add config loader for:
  - `OAUTH_REDIRECT_ALLOWLIST` (comma-separated absolute HTTPS URLs).
  - Optional: `OAUTH_PENDING_MAX`, `OAUTH_PENDING_TTL_MINUTES`.
- Validate allowlist on startup (invalid URL should fail fast).
- Update `README.md` and env docs with examples.
- **Acceptance:** startup fails on invalid allowlist entries; documentation includes safe defaults.

### 3) PR3: Pending OAuth session lifecycle controls
- Add background cleanup goroutine for `pendingAuths` (TTL eviction).
- Add upper bound (`max pending`) with reject behavior when exceeded.
- Add counters/logs for created/expired/rejected entries.
- **Acceptance:** pending auth map cannot grow unbounded under load.

### 4) PR4: HTTP client safety for upstream calls
- Replace `http.DefaultClient` usage in:
  - `brain/app.go` (OpenRouter embedding/metadata)
  - `brain/oidc.go` (userinfo verification)
  - `main.go` (token proxy)
- Inject shared `*http.Client` with sane timeouts.
- Return structured upstream errors without sensitive payload leakage.
- **Acceptance:** slow/unavailable upstreams fail with controlled timeout behavior.

### 5) PR5: App interfaces for composability
- Introduce injectable interfaces:
  - `EmbeddingProvider`
  - `MetadataProvider`
  - `TokenVerifier`
- Keep default concrete implementations to preserve behavior.
- Add unit tests with mocks (no network dependency).
- **Acceptance:** core logic can be tested without external services.

### 6) PR6: Service/repository split (first vertical slice)
- Start with capture flow as the first slice:
  - Move SQL into repository package.
  - Move orchestration into service package.
  - Keep HTTP handlers thin.
- Preserve API responses and behavior.
- **Acceptance:** no behavior drift; improved testability and separation of concerns.

### 7) PR7: Dispatch registry replacement
- Replace `switch`-based dispatch with registry pattern:
  - `map[DispatchTool]Handler`
  - Handler owns param decode + validation + execute.
- Keep fallback to `capture_thought`.
- **Acceptance:** adding new tools requires registration only (no large switch edits).

### 8) PR8: Cross-tenant FK integrity migration
- Add constraints/triggers to enforce ownership match (`user_id`) across parent/child rows.
- Prioritize:
  - CRM: interactions/opportunities -> contacts
  - Jobhunt: postings -> companies, applications -> postings, interviews -> applications
- Add migration checks/fixes for existing inconsistent rows.
- **Acceptance:** cross-user FK references are prevented at DB level.

### 9) PR9: Rate limiting and abuse controls
- Add middleware limits for `/oauth/*`, `/capture`, `/mcp`.
- Use per-IP with burst controls and conservative defaults.
- Return standard `429` responses.
- **Acceptance:** endpoint floods are throttled; service remains stable.

### 10) PR10: Test + rollout hardening
- Add integration tests for:
  - OAuth callback validation matrix
  - RLS + ownership integrity
  - token refresh/proxy edge cases
- Add deployment checklist:
  - Set `OAUTH_REDIRECT_ALLOWLIST`
  - Smoke test loopback callback and one allowlisted HTTPS callback
- **Acceptance:** release checks pass and rollout is repeatable.

## `OAUTH_REDIRECT_ALLOWLIST` format

Use exact URL matches (no wildcards), comma-separated.

Example:
`OAUTH_REDIRECT_ALLOWLIST=https://app.example.com/oauth/callback,https://staging.example.com/oauth/callback`
