package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ClaudeCodeParser struct{}

func (ClaudeCodeParser) Tool() string { return ToolClaudeCode }

func (ClaudeCodeParser) Discover(ctx context.Context, roots []string) ([]string, error) {
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
			if filepath.Ext(path) == ".jsonl" {
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

func (p ClaudeCodeParser) ParseFile(ctx context.Context, path string) (Transcript, error) {
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
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return Transcript{}, ctx.Err()
		default:
		}

		var rec claudeCodeRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if tr.SessionID == "" {
			tr.SessionID = rec.SessionID
		}
		if tr.Project == "" {
			tr.Project = rec.CWD
		}

		msg, ok := rec.message()
		if !ok {
			continue
		}
		if tr.Title == "" && msg.Role == "human" {
			tr.Title = firstLine(msg.Text)
		}
		tr.Messages = append(tr.Messages, msg)
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

type claudeCodeRecord struct {
	Type      string          `json:"type"`
	UUID      string          `json:"uuid"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"sessionId"`
	CWD       string          `json:"cwd"`
	Message   json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeContentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
}

func (r claudeCodeRecord) message() (Message, bool) {
	if r.Type != "user" && r.Type != "assistant" {
		return Message{}, false
	}
	var cm claudeMessage
	if err := json.Unmarshal(r.Message, &cm); err != nil {
		return Message{}, false
	}
	role := normalizeRole(cm.Role)
	if role == "" {
		return Message{}, false
	}
	text := renderClaudeContent(cm.Content)
	if strings.TrimSpace(text) == "" {
		return Message{}, false
	}
	ts, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
	return Message{Role: role, Text: text, Ts: ts, MsgID: r.UUID}, true
}

func normalizeRole(role string) string {
	switch role {
	case "user", "human":
		return "human"
	case "assistant":
		return "assistant"
	default:
		return ""
	}
}

func renderClaudeContent(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}

	var blocks []claudeContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if txt := strings.TrimSpace(block.Text); txt != "" {
				parts = append(parts, txt)
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name == "" {
				name = "unknown"
			}
			parts = append(parts, fmt.Sprintf("[tool: %s]", name))
		case "tool_result":
			parts = append(parts, fmt.Sprintf("[tool result omitted: %d bytes]", len(block.Content)))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func firstLine(s string) string {
	line := strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(line) > 80 {
		return line[:80]
	}
	return line
}
