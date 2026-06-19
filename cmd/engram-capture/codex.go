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
