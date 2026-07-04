// ABOUTME: Unified cross-domain search across all entry types.
// ABOUTME: Replaces per-extension search tools with a single semantic + filter query.

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"substrate/brain"
)

// RegisterSearch adds the unified search tool to the MCP server.
func RegisterSearch(s *server.MCPServer, a *brain.App) {
	s.AddTool(mcp.NewTool("search",
		mcp.WithDescription(
			"Search across all captured knowledge by meaning. "+
				"Returns results from thoughts, contacts, maintenance tasks, job applications, and any other record types. "+
				"Use record_type to narrow to a specific domain.",
		),
		mcp.WithString("query", mcp.Required(), mcp.Description("What to search for")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 10)")),
		mcp.WithNumber("threshold", mcp.Description("Similarity threshold 0–1 (default 0.4)")),
		mcp.WithString("record_type", mcp.Description(
			"Filter by type: note.thought, crm.contact, crm.interaction, maintenance.task, jobhunt.application, note.unstructured",
		)),
		mcp.WithString("since", mcp.Description("Only entries created on or after this date (YYYY-MM-DD)")),
	), searchAll(a))
}

func searchAll(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := req.GetArguments()["query"].(string)
		if query == "" {
			return brain.ToolError("query is required"), nil
		}
		limit := 10
		if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		threshold := 0.4
		if v, ok := req.GetArguments()["threshold"].(float64); ok && v > 0 {
			threshold = v
		}
		recordType, _ := req.GetArguments()["record_type"].(string)
		since, _ := req.GetArguments()["since"].(string)

		emb, err := a.GetEmbedding(ctx, query)
		if err != nil {
			return brain.ToolError("Failed to generate embedding: " + err.Error()), nil
		}

		type result struct {
			ID          string
			RecordType  string
			ContentText string
			Payload     json.RawMessage
			Similarity  float64
			CreatedAt   time.Time
		}
		var results []result

		err = a.WithUserTx(ctx, func(tx pgx.Tx) error {
			sql := `
				SELECT id::text, record_type, content_text, payload,
				       1 - (embedding <=> $1) AS similarity, created_at
				FROM entries
				WHERE deleted_at IS NULL
				  AND embedding IS NOT NULL
				  AND 1 - (embedding <=> $1) > $2`
			args := []any{emb, threshold}
			n := 3

			if recordType != "" {
				sql += fmt.Sprintf(" AND record_type = $%d", n)
				args = append(args, recordType)
				n++
			}
			if since != "" {
				sql += fmt.Sprintf(" AND created_at >= $%d::date", n)
				args = append(args, since)
				n++
			}

			sql += fmt.Sprintf(" ORDER BY embedding <=> $1 LIMIT $%d", n)
			args = append(args, limit)

			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r result
				if err := rows.Scan(&r.ID, &r.RecordType, &r.ContentText, &r.Payload, &r.Similarity, &r.CreatedAt); err != nil {
					return err
				}
				results = append(results, r)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Search error: " + err.Error()), nil
		}

		if len(results) == 0 {
			msg := fmt.Sprintf(`No results found for "%s".`, query)
			if recordType != "" {
				msg = fmt.Sprintf(`No %s entries found matching "%s".`, recordType, query)
			}
			return brain.TextResult(msg), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d result(s) for \"%s\":\n\n", len(results), query)
		for i, r := range results {
			fmt.Fprintf(&sb, "--- %d. %s (%.1f%% match, %s) ---\n",
				i+1, r.RecordType, r.Similarity*100, r.CreatedAt.Format("2006-01-02"))
			fmt.Fprintf(&sb, "%s\n", r.ContentText)
			fmt.Fprintf(&sb, "%s\n\n", FormatPayloadSummary(r.RecordType, r.Payload))
		}
		return brain.TextResult(sb.String()), nil
	}
}

// FormatPayloadSummary renders a compact human-readable summary of a payload
// based on its record type, without dumping raw JSON.
func FormatPayloadSummary(recordType string, raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}

	var parts []string
	switch recordType {
	case "crm.contact":
		if v, _ := m["company"].(string); v != "" {
			parts = append(parts, "Company: "+v)
		}
		if v, _ := m["title"].(string); v != "" {
			parts = append(parts, "Title: "+v)
		}
		if v, _ := m["email"].(string); v != "" {
			parts = append(parts, "Email: "+v)
		}
	case "crm.interaction":
		if v, _ := m["interaction_type"].(string); v != "" {
			parts = append(parts, "Type: "+v)
		}
		if v, _ := m["interaction_date"].(string); v != "" {
			parts = append(parts, "Date: "+v)
		}
		if v, _ := m["follow_up_needed"].(bool); v {
			parts = append(parts, "Follow-up needed")
		}
	case "maintenance.task":
		if v, _ := m["category"].(string); v != "" {
			parts = append(parts, "Category: "+v)
		}
		if v, _ := m["next_due"].(string); v != "" {
			parts = append(parts, "Next due: "+v)
		}
		if v, _ := m["frequency_days"].(float64); v > 0 {
			parts = append(parts, fmt.Sprintf("Every %d days", int(v)))
		}
	case "jobhunt.application":
		if v, _ := m["status"].(string); v != "" {
			parts = append(parts, "Status: "+v)
		}
		if v, _ := m["applied_date"].(string); v != "" {
			parts = append(parts, "Applied: "+v)
		}
	case "note.thought":
		if topics, _ := m["topics"].([]any); len(topics) > 0 {
			ts := make([]string, 0, len(topics))
			for _, t := range topics {
				if s, ok := t.(string); ok {
					ts = append(ts, s)
				}
			}
			if len(ts) > 0 {
				parts = append(parts, "Topics: "+strings.Join(ts, ", "))
			}
		}
	case "note.link":
		if v, _ := m["title"].(string); v != "" {
			parts = append(parts, "Title: "+v)
		}
		if v, _ := m["url"].(string); v != "" {
			parts = append(parts, v)
		}
	case "note.unstructured":
		if v, _ := m["failure_mode"].(string); v != "" {
			parts = append(parts, "[unstructured: "+v+"]")
		}
	case "conversation.summary":
		if topics, _ := m["topics"].([]any); len(topics) > 0 {
			ts := make([]string, 0, len(topics))
			for _, t := range topics {
				if s, ok := t.(string); ok {
					ts = append(ts, s)
				}
			}
			if len(ts) > 0 {
				parts = append(parts, "Topics: "+strings.Join(ts, ", "))
			}
		}
	}

	return strings.Join(parts, " | ")
}
