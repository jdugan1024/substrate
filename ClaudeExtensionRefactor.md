# Claude Extension Refactor — Review

> Response to `CodexExtensionRefactor.md`

## Overall Assessment

Adopt this. The design is minimal, the scope is correct, and the non-goals are well-chosen. Single binary with compile-time extensions and runtime enable/disable via config is the right tradeoff for a self-hosted tool at this scale.

---

## Agreement

The core `Definition` struct is exactly right:

```go
type Definition struct {
    ID          ExtensionID
    Description string
    Register    func(s *server.MCPServer, a *brain.App)
    DefaultOn   bool
    DependsOn   []ExtensionID
}
```

`DependsOn` is worth keeping even before any dependencies are needed. It makes dependency validation possible when extensions are first added that actually need it, without a second refactor.

The allowlist semantics (`ENGRAM_ENABLED_EXTENSIONS` unset → enable all defaults) preserve backward compatibility cleanly. Good default.

---

## Modifications

### Registry initialization

The plan shows `var All = map[ExtensionID]Definition{...}`. Prefer a slice for `All` to preserve registration order, and expose a `Register(d Definition)` function that appends to it. Each extension package's `init()` can self-register:

```go
// extensions/registry.go
var all []Definition

func Register(d Definition) {
    all = append(all, d)
}

func All() []Definition {
    return all
}
```

```go
// extensions/crm/extension.go
func init() {
    registry.Register(registry.Definition{
        ID:          "crm",
        Description: "CRM contacts and interactions",
        Register:    Register,
        DefaultOn:   true,
    })
}
```

This avoids a central file that needs editing every time an extension is added. `main.go` just imports the extension packages for their side effects.

### `ENGRAM_DISABLED_EXTENSIONS` — add now, not later

The plan marks this as optional. It's worth adding in the same PR as the allowlist parsing. The implementation is symmetric and the "I want everything except X" use case appears constantly in practice. Semantics: if both are set, `ENABLED` wins.

### Unknown extension ID behavior

The plan says "fatal error" for unknown IDs in `ENGRAM_ENABLED_EXTENSIONS`. Agree. Log each unknown ID explicitly before the fatal, so the operator can identify and fix the config without guesswork.

### `GET /-/extensions` endpoint

Build this in PR2, not PR3. It's a 20-line handler and the operational value (confirming what's actually enabled in a running process) is high enough to justify earlier delivery. The startup logging alone is not enough when diagnosing a staging vs production config difference.

---

## Prioritization

**PR1: Registry + config parser + unit tests** — start here.

The unit tests for `ResolveEnabled` are the most valuable artifact in this PR. Test:
- Unset → all defaults enabled
- Valid subset → only listed enabled
- Unknown ID → error
- Duplicate IDs in input → handled (deduplicated or error, pick one)
- `ENGRAM_DISABLED_EXTENSIONS` with overlap → defined behavior

**PR2: Startup wiring + `/-/extensions` endpoint + logging** — combine with endpoint.

**PR3: Docs** — update `README.md` after PR2 is deployed and behavior is confirmed.

---

## Current Extension List

Based on the current codebase, the initial registry should include:

| ID | DefaultOn | Notes |
|----|-----------|-------|
| `core` | true | Thoughts, search, recall — always required |
| `household` | true | Shopping lists, household notes |
| `maintenance` | true | Maintenance tasks |
| `calendar` | true | Important dates |
| `meals` | true | Meal planning |
| `crm` | true | Contacts, interactions |
| `jobhunt` | true | Job applications, companies, interviews |

`core` should validate at startup that it's always in the enabled set — or be unconditionally registered regardless of config, since it provides the base memory functionality the whole system is built on.

---

## Non-goals Confirmed

- No runtime loading/unloading after process start. Correct — restart is cheap and the complexity of hot-reload is not justified.
- No per-tenant extension enablement. Correct for now. If this ever becomes a multi-tenant product, it belongs in the database config, not env vars.
- No per-extension migration gating. Correct. Schema stays installed regardless of which extensions are enabled.
