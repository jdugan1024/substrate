# Codex Extension Refactor Plan

Target outcome:
- Keep one binary with all extensions compiled in.
- Enable/disable extensions via config at startup.
- Avoid plugin IPC and `.so` loading complexity.

## Design

Introduce a central extension registry and metadata. Replace hardcoded extension registration in `main.go` with filtered registration based on config.

```go
// package extensions
type ExtensionID string

type Definition struct {
    ID          ExtensionID
    Description string
    Register    func(s *server.MCPServer, a *brain.App)
    DefaultOn   bool
    DependsOn   []ExtensionID // optional
}
```

```go
// package config (or main-local)
type ExtensionConfig struct {
    Enabled []string // from ENGRAM_ENABLED_EXTENSIONS
    Mode    string   // "allowlist" (recommended), optional "all_except"
}
```

## Execution Checklist

### 1) Create extension registry
- Add `extensions/registry.go` with:
  - `var All = map[ExtensionID]Definition{...}`
  - `ResolveEnabled(cfg ExtensionConfig) ([]Definition, error)`
- Initial IDs:
  - `core`
  - `household`
  - `maintenance`
  - `calendar`
  - `meals`
  - `crm`
  - `jobhunt`
- Include dependency validation (even if no dependencies are needed yet).

### 2) Config parsing
- Add env var `ENGRAM_ENABLED_EXTENSIONS`.
- Parse comma-separated values, normalize case/whitespace, reject unknown IDs.
- Semantics:
  - If unset: enable default set (all current extensions for backward compatibility).
  - If set: only listed IDs are enabled.
- Optional later: `ENGRAM_DISABLED_EXTENSIONS`.

### 3) Startup wiring
- In `main.go`, replace static calls (`core.Register`, etc.) with:
  - resolve enabled list
  - iterate and call each `Definition.Register`
- Startup logs should include:
  - enabled IDs
  - disabled IDs
  - unknown IDs (fatal error)

### 4) Operational visibility
- Add read-only endpoint `GET /-/extensions` that returns:
  - enabled IDs
  - disabled IDs
  - descriptions
- If endpoint is deferred, ensure startup logging is explicit enough for operators.

### 5) Schema and migrations (phase 1)
- Keep current migration behavior in `docker-compose.yml` (all extension schemas still applied).
- Disabled extension means tools are hidden, not that schema is removed.
- This keeps rollout low-risk and avoids migration complexity.

### 6) Tests
- Unit tests for parsing and resolving:
  - unset config -> defaults
  - valid allowlist subset
  - unknown extension ID -> error
  - duplicate IDs handled cleanly
- Integration smoke test:
  - run with subset enabled
  - verify only expected tools are registered
- Regression test:
  - no env var -> current tool surface unchanged

### 7) Documentation
- Update `README.md`:
  - list extension IDs
  - include env examples:
    - `ENGRAM_ENABLED_EXTENSIONS=core,crm,jobhunt`
    - `ENGRAM_ENABLED_EXTENSIONS=core`
  - mention schemas remain installed even when extensions are disabled.

## Suggested PR Breakdown

1. PR1: Registry + config parser + unit tests.
2. PR2: Startup wiring + logging + integration smoke test.
3. PR3: Optional `GET /-/extensions` endpoint + docs.

## Non-goals (for now)

- No runtime loading/unloading after process start.
- No per-tenant extension enablement.
- No per-extension migration gating.
