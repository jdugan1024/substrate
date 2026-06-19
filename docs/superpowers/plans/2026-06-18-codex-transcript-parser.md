# Codex Transcript Parser Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `CodexParser` to the `engram-capture` daemon so local Codex CLI rollout transcripts are captured into Engram, with subagents linked to their parent session.

**Architecture:** Codex stores full transcripts as `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`, one JSON object per line (`{timestamp, type, payload}`). A new parser implements the existing `Parser` interface and emits the common `Transcript` type; the runner, trimming, state, client, and server paths are reused unchanged. No server or schema changes.

**Tech Stack:** Go 1.25, stdlib `encoding/json`, `bufio`, `path/filepath`, table-driven tests with a JSONL fixture.

Spec: `docs/superpowers/specs/2026-06-18-codex-transcript-parser-design.md`.

---

## File Structure

| Path | Responsibility |
|------|----------------|
| `cmd/engram-capture/codex.go` | `CodexParser`: Discover rollout files + ParseFile → `Transcript` |
| `cmd/engram-capture/codex_test.go` | Table-driven parser + discover tests |
| `cmd/engram-capture/testdata/codex/sessions/2026/06/18/rollout-2026-06-18T10-00-00-sess-codex-1.jsonl` | Minimal rollout fixture |
| `cmd/engram-capture/config.go` | Add `CodexRoots` + default |
| `cmd/engram-capture/main.go` | Add `--codex-root` flag, register `CodexParser` + roots |

The parser reuses helpers already in `claude_code.go`: `normalizeRole` (returns `human`/`assistant`/`""`) and `firstLine`.

---

## Task 1: Codex Parser And Fixture

**Files:**
- Create: `cmd/engram-capture/testdata/codex/sessions/2026/06/18/rollout-2026-06-18T10-00-00-sess-codex-1.jsonl`
- Create: `cmd/engram-capture/codex.go`
- Create: `cmd/engram-capture/codex_test.go`

- [ ] **Step 1: Create the fixture**

Create `cmd/engram-capture/testdata/codex/sessions/2026/06/18/rollout-2026-06-18T10-00-00-sess-codex-1.jsonl` with exactly these lines:

```jsonl
{"timestamp":"2026-06-18T10:00:00.000Z","type":"session_meta","payload":{"id":"sess-codex-1","parent_thread_id":"parent-thread-9","cwd":"/home/jdugan/proj","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent-thread-9","depth":1,"agent_role":"worker"}}}}}
{"timestamp":"2026-06-18T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions> system boilerplate"}]}}
{"timestamp":"2026-06-18T10:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Implement the Codex parser."}]}}
{"timestamp":"2026-06-18T10:00:03.000Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"OPAQUE"}}
{"timestamp":"2026-06-18T10:00:04.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"On it. Reading the files."}]}}
{"timestamp":"2026-06-18T10:00:05.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"}}
{"timestamp":"2026-06-18T10:00:06.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"main.go\ncodex.go"}}
{"timestamp":"2026-06-18T10:00:07.000Z","type":"event_msg","payload":{"type":"agent_message","message":"On it. Reading the files."}}
{"timestamp":"2026-06-18T10:00:08.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}
```

Note: the `function_call_output` `output` value `"main.go\ncodex.go"` decodes to a 16-byte Go string (7 + 1 newline + 8).

- [ ] **Step 2: Write the failing tests**

Create `cmd/engram-capture/codex_test.go`:

```go
package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCodexParser_ParseFixture(t *testing.T) {
	parser := CodexParser{}
	path := filepath.Join("testdata", "codex", "sessions", "2026", "06", "18", "rollout-2026-06-18T10-00-00-sess-codex-1.jsonl")

	tr, err := parser.ParseFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if tr.Tool != ToolCodex {
		t.Fatalf("tool = %q", tr.Tool)
	}
	if tr.SessionID != "sess-codex-1" {
		t.Fatalf("session id = %q", tr.SessionID)
	}
	if tr.ParentSessionID != "parent-thread-9" {
		t.Fatalf("parent session id = %q", tr.ParentSessionID)
	}
	if tr.Project != "/home/jdugan/proj" {
		t.Fatalf("project = %q", tr.Project)
	}
	if tr.Title != "Implement the Codex parser." {
		t.Fatalf("title = %q", tr.Title)
	}

	want := []Message{
		{Role: "human", Text: "Implement the Codex parser.", MsgID: "0"},
		{Role: "assistant", Text: "On it. Reading the files.", MsgID: "1"},
		{Role: "assistant", Text: "[tool: shell]", MsgID: "2"},
		{Role: "assistant", Text: "[tool result omitted: 16 bytes]", MsgID: "3"},
		{Role: "assistant", Text: "Done.", MsgID: "4"},
	}
	if len(tr.Messages) != len(want) {
		t.Fatalf("message count = %d, want %d: %#v", len(tr.Messages), len(want), tr.Messages)
	}
	for i := range want {
		if tr.Messages[i].Role != want[i].Role || tr.Messages[i].Text != want[i].Text || tr.Messages[i].MsgID != want[i].MsgID {
			t.Fatalf("message %d = %#v, want %#v", i, tr.Messages[i], want[i])
		}
	}
}

func TestCodexParser_DiscoverFindsRollouts(t *testing.T) {
	parser := CodexParser{}
	root := filepath.Join("testdata", "codex")

	paths, err := parser.Discover(context.Background(), []string{root})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "rollout-2026-06-18T10-00-00-sess-codex-1.jsonl" {
		t.Fatalf("paths = %#v", paths)
	}
}
```

- [ ] **Step 3: Run tests and confirm failure**

Run: `go test ./cmd/engram-capture/ -run TestCodexParser`
Expected: FAIL — `CodexParser` undefined (build error).

- [ ] **Step 4: Implement the parser**

Create `cmd/engram-capture/codex.go`:

```go
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type CodexParser struct{}

func (CodexParser) Tool() string { return ToolCodex }

func (CodexParser) Discover(ctx context.Context, roots []string) ([]string, error) {
	var paths []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if d.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			if strings.HasPrefix(base, "rollout-") && filepath.Ext(base) == ".jsonl" {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (p CodexParser) ParseFile(ctx context.Context, path string) (Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return Transcript{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return Transcript{}, err
	}

	tr := Transcript{Tool: p.Tool(), Path: path, ModTime: st.ModTime()}
	seq := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return Transcript{}, ctx.Err()
		default:
		}

		var rec codexRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)

		switch rec.Type {
		case "session_meta":
			var meta codexSessionMeta
			if err := json.Unmarshal(rec.Payload, &meta); err != nil {
				continue
			}
			if tr.SessionID == "" {
				tr.SessionID = meta.ID
			}
			if tr.Project == "" {
				tr.Project = meta.CWD
			}
			if meta.ParentThreadID != "" && hasSubagentSource(meta.Source) {
				tr.ParentSessionID = meta.ParentThreadID
			}
		case "response_item":
			text, role := renderCodexPayload(rec.Payload)
			if role == "" || strings.TrimSpace(text) == "" {
				continue
			}
			if tr.Title == "" && role == "human" {
				tr.Title = firstLine(text)
			}
			tr.Messages = append(tr.Messages, Message{
				Role:  role,
				Text:  text,
				Ts:    ts,
				MsgID: strconv.Itoa(seq),
			})
			seq++
		}
	}
	if err := scanner.Err(); err != nil {
		return Transcript{}, err
	}
	if tr.SessionID == "" {
		tr.SessionID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if tr.Title == "" {
		tr.Title = tr.SessionID
	}
	return tr, nil
}

type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMeta struct {
	ID             string          `json:"id"`
	ParentThreadID string          `json:"parent_thread_id"`
	CWD            string          `json:"cwd"`
	Source         json.RawMessage `json:"source"`
}

type codexContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexPayload struct {
	Type    string              `json:"type"`
	Role    string              `json:"role"`
	Name    string              `json:"name"`
	Output  string              `json:"output"`
	Content []codexContentBlock `json:"content"`
}

// renderCodexPayload converts a response_item payload into (text, role). It
// returns role "" for payloads that should be skipped (developer messages,
// reasoning, token_count, unknown types, empty content).
func renderCodexPayload(raw json.RawMessage) (string, string) {
	var p codexPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", ""
	}
	switch p.Type {
	case "message":
		role := normalizeRole(p.Role) // developer -> ""
		if role == "" {
			return "", ""
		}
		var parts []string
		for _, b := range p.Content {
			if txt := strings.TrimSpace(b.Text); txt != "" {
				parts = append(parts, txt)
			}
		}
		return strings.Join(parts, "\n"), role
	case "function_call", "custom_tool_call":
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = "unknown"
		}
		return fmt.Sprintf("[tool: %s]", name), "assistant"
	case "function_call_output", "custom_tool_call_output":
		return fmt.Sprintf("[tool result omitted: %d bytes]", len(p.Output)), "assistant"
	default:
		return "", ""
	}
}

func hasSubagentSource(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m["subagent"]
	return ok
}
```

- [ ] **Step 5: Run tests and confirm pass**

Run: `go test ./cmd/engram-capture/ -run TestCodexParser`
Expected: PASS.

- [ ] **Step 6: Run full daemon tests + gofmt**

Run: `gofmt -l cmd/engram-capture/codex.go cmd/engram-capture/codex_test.go && go test ./cmd/engram-capture/`
Expected: gofmt prints nothing; all tests pass.

- [ ] **Step 7: Commit**

```bash
git add cmd/engram-capture/codex.go cmd/engram-capture/codex_test.go cmd/engram-capture/testdata/codex
git commit -m "feat: parse Codex rollout transcripts"
```

## Task 2: Wire Codex Into Config And CLI

**Files:**
- Modify: `cmd/engram-capture/config.go`
- Modify: `cmd/engram-capture/main.go`

- [ ] **Step 1: Add CodexRoots to Config**

In `cmd/engram-capture/config.go`, add the field to the `Config` struct after `ClaudeRoots`:

```go
	ClaudeRoots   []string
	CodexRoots    []string
```

- [ ] **Step 2: Set the default**

In `DefaultConfig()` in `cmd/engram-capture/config.go`, add after the `ClaudeRoots` line:

```go
		ClaudeRoots:   []string{filepath.Join(home, ".claude", "projects")},
		CodexRoots:    []string{filepath.Join(home, ".codex", "sessions")},
```

- [ ] **Step 3: Add the flag and register the parser**

In `cmd/engram-capture/main.go`, add a `codexRoots` flag variable and flag next to the `claude-root` flag:

```go
	var claudeRoots string
	var codexRoots string
```

and after the `claude-root` flag line:

```go
	flag.StringVar(&codexRoots, "codex-root", strings.Join(cfg.CodexRoots, string(os.PathListSeparator)), "Codex transcript root(s), path-list separated")
```

After `cfg.ClaudeRoots = filepathList(claudeRoots)` add:

```go
	cfg.CodexRoots = filepathList(codexRoots)
```

Change the `Runner` construction's `Parsers` and `Roots`:

```go
	runner := Runner{
		Parsers: []Parser{ClaudeCodeParser{}, CodexParser{}},
		Roots: map[string][]string{
			ToolClaudeCode: cfg.ClaudeRoots,
			ToolCodex:      cfg.CodexRoots,
		},
		State:      state,
		Poster:     NewIngestClient(cfg.BaseURL, cfg.PAT, &http.Client{Timeout: 30 * time.Second}),
		Trim:       cfg.Trim,
		Machine:    cfg.Machine,
		Username:   cfg.Username,
		EndedAfter: cfg.EndedAfter,
		Now:        time.Now,
		DryRun:     cfg.DryRun,
	}
```

- [ ] **Step 4: Build and run full suite**

Run: `gofmt -l cmd/engram-capture/*.go && go build ./... && go test ./...`
Expected: gofmt prints nothing; build succeeds; all tests pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/engram-capture/config.go cmd/engram-capture/main.go
git commit -m "feat: register Codex parser in capture daemon"
```

## Task 3: Verify And Roll Out

**Files:** none (verification + ops).

- [ ] **Step 1: Dry-run against real Codex transcripts**

Run: `go run ./cmd/engram-capture --dry-run --backfill --codex-root ~/.codex/sessions --claude-root /tmp/engram-none`
Expected: `scan: sessions=<nonzero> messages=<nonzero> would_post=<nonzero> skipped_noop=0 parse_failures=0 estimated_bytes=<nonzero>`. (Setting `--claude-root /tmp/engram-none` isolates Codex for this check.)

- [ ] **Step 2: Real backfill (requires PAT)**

Run:

```bash
ENGRAM_PAT='engram_pat_...' go run ./cmd/engram-capture --backfill --codex-root ~/.codex/sessions --claude-root /tmp/engram-none
```

Expected: exit 0, no `non-append-only` or `ingest failed` errors.

- [ ] **Step 3: Verify Codex rows landed with parent links**

Run:

```bash
docker exec engram-db-1 psql -U postgres -d openbrain -c "select count(*) from entries where source='codex';"
docker exec engram-db-1 psql -U postgres -d openbrain -c "select entities->>'session_id' as session, entities->>'parent_session_id' as parent from entries where source='codex' and record_type='conversation.summary' and entities ? 'parent_session_id' limit 3;"
```

Expected: nonzero codex entries; subagent summaries show a non-null `parent`.

- [ ] **Step 4: Add Codex to the systemd unit**

Edit `~/.config/systemd/user/engram-capture.service`, change the `ExecStart` line to include the Codex root (the daemon already defaults to it, so this is explicit-belt-and-suspenders):

```
ExecStart=%h/.local/bin/engram-capture --watch --machine %H --username %u --claude-root %h/.claude/projects --codex-root %h/.codex/sessions
```

Add `%h/.codex/sessions` to `ReadWritePaths`? No — Codex roots are read-only; only `%h/.local/state/engram-capture` needs write. Leave `ReadWritePaths` unchanged. (`ProtectHome=read-only` already permits reading `~/.codex`.)

Then rebuild the binary and restart:

```bash
go build -o ~/.local/bin/engram-capture ./cmd/engram-capture
systemctl --user daemon-reload
systemctl --user restart engram-capture
journalctl --user -u engram-capture --no-pager -n 5
```

Expected: a `scan:` line showing both tools' sessions, no errors.

## Verification Before Completion

Run:

```bash
gofmt -l cmd/engram-capture/*.go
go test ./...
go run ./cmd/engram-capture --dry-run --backfill --codex-root ~/.codex/sessions --claude-root /tmp/engram-none
```

Expected: gofmt silent; all tests pass; dry-run reports nonzero Codex sessions with 0 parse failures.

## Self-Review

- **Spec coverage:** Discover (Task 1), message-only canonical records + role mapping + developer/reasoning skip + tool placeholders (Task 1 `renderCodexPayload`), subagent linking via `parent_session_id` (Task 1 `hasSubagentSource`), config/CLI/runner wiring (Task 2), dry-run/backfill/systemd rollout (Task 3). No server changes — consistent with spec.
- **Placeholder scan:** none.
- **Type consistency:** uses existing `Transcript`, `Message`, `Parser`, `ToolCodex`, `normalizeRole`, `firstLine`; new types `codexRecord`, `codexSessionMeta`, `codexPayload`, `codexContentBlock` are self-contained in `codex.go`.
