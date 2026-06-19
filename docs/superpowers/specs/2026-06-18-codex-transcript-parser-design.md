# Codex Transcript Parser — Design

**Date:** 2026-06-18
**Status:** Approved design; ready for implementation plan.

## Goal

Add a Codex parser to the `engram-capture` daemon so local Codex CLI sessions are
captured into Engram the same way Claude Code sessions are. The server is already
tool-agnostic and supports `parent_session_id`, so this is a **daemon-only**
change — no server or schema changes.

## Background — on-disk format

Codex stores full transcripts as rollout files:

```
~/.codex/sessions/YYYY/MM/DD/rollout-<ISO8601>-<uuid>.jsonl
```

(`~/.codex/history.jsonl` is one-sided prompt history only — **not** used.)

Each line is a JSON object `{timestamp, type, payload}`. Relevant records:

- **`session_meta`** (first line): `payload.id` (session UUID, matches filename),
  `payload.cwd` (project), `payload.parent_thread_id`, and `payload.source` which
  is either `{cli: ...}` (root session, no parent) or `{subagent: {thread_spawn:
  {parent_thread_id, ...}}}` (worker subagent).
- **`response_item`** with `payload.type == "message"`: the canonical, role-tagged
  turn. `payload.role` ∈ {`user`, `assistant`, `developer`}; `payload.content` is
  an array of blocks `{type: "input_text"|"output_text", text}`. Has **no id**.
- **`response_item`** with `payload.type` of `function_call`
  (`{name, arguments, call_id}`), `function_call_output` (`{call_id, output}`),
  `custom_tool_call` / `custom_tool_call_output`, `patch_apply_end` — tool activity.
- **`response_item`** with `payload.type == "reasoning"`: `encrypted_content`
  (opaque/unreadable).
- **`event_msg`** with `payload.type` `user_message` / `agent_message`: UI event
  **duplicates** of the `message` records (verified identical text). Ignored to
  avoid double-counting.
- Other (`token_count`, `task_started`, `turn_context`): metadata, ignored.

Across the 40 local sessions, `source` is `subagent` (37, all with
`parent_thread_id`) or `cli` (5 roots) — the same parent/child shape Claude Code
has, so the existing `parent_session_id` linking applies directly.

## Design decisions (approved)

1. **Message source:** read only the canonical `message` records; ignore the
   duplicate `user_message` / `agent_message` event echoes.
2. **Content scope (match Claude Code parser):** emit `[tool: <name>]` for
   `function_call` / `custom_tool_call` and `[tool result omitted: <n> bytes]`
   for their `*_output`; **skip** `developer` (system boilerplate) and `reasoning`
   (encrypted).
3. **Subagent linking:** each subagent rollout is its own session keyed on the
   session UUID, linked via `parent_session_id = parent_thread_id` when
   `source.subagent` is present. Root (`source.cli`) sessions have no parent.

## Components

### `CodexParser` — `cmd/engram-capture/codex.go`

Implements the existing `Parser` interface:

- **`Tool() string`** → `ToolCodex` ("codex", already defined in `types.go`).
- **`Discover(ctx, roots)`** → `filepath.WalkDir` each root, collect files whose
  base name matches `rollout-*.jsonl`, sorted. (Mirrors `ClaudeCodeParser.Discover`.)
- **`ParseFile(ctx, path)`** → builds a `Transcript`:
  - Read the file line by line (`bufio.Scanner` with an enlarged buffer, as the
    Claude parser does — rollout files reach ~1 MB).
  - From `session_meta`: set `SessionID = payload.id` (fallback: filename stem),
    `Project = payload.cwd`; if `payload.source` has a `subagent` key, set
    `ParentSessionID = payload.parent_thread_id`.
  - For each `response_item`:
    - `payload.type == "message"`: map role (`user`→`human`, `assistant`→
      `assistant`, `developer`→skip); text = trimmed concat of `content[]` block
      `text` joined by newlines; skip if empty. First human line sets `Title`.
    - `function_call` / `custom_tool_call`: append assistant message
      `[tool: <name>]` (name fallback "unknown").
    - `function_call_output` / `custom_tool_call_output`: append assistant message
      `[tool result omitted: <len(output)> bytes]`.
    - all other payload types: skip.
  - `ts` for each message comes from the record's top-level `timestamp`
    (RFC3339).
  - **`msg_id`:** synthesize a stable per-file id `fmt.Sprintf("%d", seq)` (running
    index of emitted messages). Codex messages carry no native id; the server
    dedups on message count and the daemon on count + content hash, so a stable
    sequential id is sufficient.
  - Title fallback to `SessionID` if no human message produced one.

### Config / wiring — `config.go`, `main.go`, `runner.go`

- `Config.CodexRoots []string`, default `[]string{~/.codex/sessions}`.
- `--codex-root` flag (path-list separated), parallel to `--claude-root`.
- Register `CodexParser{}` in the runner's `Parsers` and add its roots to the
  `Roots` map keyed by `ToolCodex`.

### Reused without change

`BuildIngestBatch` (already carries `ParentSessionID`, `Machine`, `Username`),
`StateStore` dedup, `IngestClient`, watch mode, and the entire server ingest path
(`tool="codex"` and `parent_session_id` already supported and stored in entry
`entities`).

## Testing

- **Fixture:** `cmd/engram-capture/testdata/codex/` with a small redacted rollout
  containing: a `session_meta` (subagent, with `parent_thread_id` + `source.subagent`),
  a `developer` message (must be skipped), a `user` message, an `assistant`
  message, a `function_call` + `function_call_output`, a `reasoning` record (must
  be skipped), and a duplicate `agent_message` event echo (must be ignored).
- **`ParseFile` tests:** assert `SessionID`, `ParentSessionID`, `Project`, `Title`,
  the exact emitted message sequence (roles + text incl. tool placeholders), and
  that developer/reasoning/echo records are excluded.
- **`Discover` test:** finds `rollout-*.jsonl` under a nested `YYYY/MM/DD` tree and
  ignores non-rollout files.
- Full suite green (`go test ./...`).

## Rollout

1. `engram-capture --dry-run --backfill --codex-root ~/.codex/sessions` — confirm
   nonzero sessions/messages, 0 parse failures.
2. Real `--backfill` (PAT) — verify rows land with `tool=codex` and subagent
   entries carry `parent_session_id` in `entities`.
3. Add `--codex-root` to the systemd unit `ExecStart` (capture both tools).

## Out of scope

Zed and Pi parsers (separate future work); Codex `history.jsonl`; reading
encrypted reasoning.
