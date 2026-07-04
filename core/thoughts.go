// ABOUTME: Core thought tools: search, list, and summarize entries of type note.thought.
// ABOUTME: Registered into the single MCP server alongside any active extensions.
// ABOUTME: capture_thought is superseded by add_item; search/list/stats query the entries table.

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

// Register adds the core thought read tools to the MCP server.
// capture_thought is removed — use add_item thought <content> instead.
func Register(s *server.MCPServer, a *brain.App) {
	s.AddTool(mcp.NewTool("search_thoughts",
		mcp.WithDescription("Search captured thoughts by meaning. Use this when the user asks about a topic, person, or idea they've previously captured."),
		mcp.WithString("query", mcp.Required(), mcp.Description("What to search for")),
		mcp.WithNumber("limit", mcp.Description("Max results to return (default 10)")),
		mcp.WithNumber("threshold", mcp.Description("Similarity threshold 0–1 (default 0.5). Lower = broader results.")),
	), searchThoughts(a))

	s.AddTool(mcp.NewTool("list_thoughts",
		mcp.WithDescription("List recently captured thoughts with optional filters by type, topic, person, or time range."),
		mcp.WithNumber("limit", mcp.Description("Max results (default 10)")),
		mcp.WithString("type", mcp.Description("Filter by type: observation, task, idea, reference, person_note")),
		mcp.WithString("topic", mcp.Description("Filter by topic tag")),
		mcp.WithString("person", mcp.Description("Filter by person mentioned")),
		mcp.WithNumber("days", mcp.Description("Only thoughts from the last N days")),
	), listThoughts(a))

	s.AddTool(mcp.NewTool("thought_stats",
		mcp.WithDescription("Get a summary of all captured thoughts: totals, types, top topics, and people."),
	), thoughtStats(a))
}

// thoughtPayload mirrors the note.thought JSON Schema fields we read back.
type thoughtPayload struct {
	Content     string   `json:"content"`
	ThoughtType string   `json:"thought_type"`
	Topics      []string `json:"topics"`
	People      []string `json:"people"`
	ActionItems []string `json:"action_items"`
}

func searchThoughts(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := req.GetArguments()["query"].(string)
		if query == "" {
			return brain.ToolError("query is required"), nil
		}
		limit := 10
		if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		threshold := 0.5
		if v, ok := req.GetArguments()["threshold"].(float64); ok {
			threshold = v
		}

		emb, err := a.GetEmbedding(ctx, query)
		if err != nil {
			return brain.ToolError("Failed to generate embedding: " + err.Error()), nil
		}

		type result struct {
			ContentText string
			Payload     thoughtPayload
			Similarity  float64
			CreatedAt   time.Time
		}
		var results []result

		err = a.WithUserTx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT content_text, payload,
				       1 - (embedding <=> $1) AS similarity,
				       created_at
				FROM entries
				WHERE record_type = 'note.thought'
				  AND deleted_at IS NULL
				  AND embedding IS NOT NULL
				  AND 1 - (embedding <=> $1) > $2
				ORDER BY embedding <=> $1
				LIMIT $3`,
				emb, threshold, limit,
			)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r result
				var payloadRaw []byte
				if err := rows.Scan(&r.ContentText, &payloadRaw, &r.Similarity, &r.CreatedAt); err != nil {
					return err
				}
				json.Unmarshal(payloadRaw, &r.Payload)
				results = append(results, r)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Search error: " + err.Error()), nil
		}

		if len(results) == 0 {
			return brain.TextResult(fmt.Sprintf(`No thoughts found matching "%s".`, query)), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d thought(s):\n\n", len(results))
		for i, r := range results {
			fmt.Fprintf(&sb, "--- Result %d (%.1f%% match) ---\n", i+1, r.Similarity*100)
			fmt.Fprintf(&sb, "Captured: %s\nType: %s\n", r.CreatedAt.Format("2006-01-02"), r.Payload.ThoughtType)
			if len(r.Payload.Topics) > 0 {
				fmt.Fprintf(&sb, "Topics: %s\n", strings.Join(r.Payload.Topics, ", "))
			}
			if len(r.Payload.People) > 0 {
				fmt.Fprintf(&sb, "People: %s\n", strings.Join(r.Payload.People, ", "))
			}
			if len(r.Payload.ActionItems) > 0 {
				fmt.Fprintf(&sb, "Actions: %s\n", strings.Join(r.Payload.ActionItems, "; "))
			}
			fmt.Fprintf(&sb, "\n%s\n\n", r.Payload.Content)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func listThoughts(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		limit := 10
		if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		typeFilter, _ := req.GetArguments()["type"].(string)
		topicFilter, _ := req.GetArguments()["topic"].(string)
		personFilter, _ := req.GetArguments()["person"].(string)
		var days int
		if v, ok := req.GetArguments()["days"].(float64); ok && v > 0 {
			days = int(v)
		}

		type thought struct {
			Payload   thoughtPayload
			CreatedAt time.Time
		}
		var thoughts []thought

		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			sql := `SELECT payload, created_at FROM entries WHERE record_type = 'note.thought' AND deleted_at IS NULL`
			args := []any{}
			n := 1

			if typeFilter != "" {
				sql += fmt.Sprintf(" AND payload->>'thought_type' = $%d", n)
				args = append(args, typeFilter)
				n++
			}
			if topicFilter != "" {
				b, _ := json.Marshal([]string{topicFilter})
				sql += fmt.Sprintf(" AND payload->'topics' @> $%d::jsonb", n)
				args = append(args, string(b))
				n++
			}
			if personFilter != "" {
				b, _ := json.Marshal([]string{personFilter})
				sql += fmt.Sprintf(" AND payload->'people' @> $%d::jsonb", n)
				args = append(args, string(b))
				n++
			}
			if days > 0 {
				sql += fmt.Sprintf(" AND created_at >= now() - $%d * interval '1 day'", n)
				args = append(args, days)
				n++
			}
			sql += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", n)
			args = append(args, limit)

			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var t thought
				var payloadRaw []byte
				if err := rows.Scan(&payloadRaw, &t.CreatedAt); err != nil {
					return err
				}
				json.Unmarshal(payloadRaw, &t.Payload)
				thoughts = append(thoughts, t)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(thoughts) == 0 {
			return brain.TextResult("No thoughts found."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d recent thought(s):\n\n", len(thoughts))
		for i, t := range thoughts {
			ttype := t.Payload.ThoughtType
			if ttype == "" {
				ttype = "??"
			}
			meta := ttype
			if tags := strings.Join(t.Payload.Topics, ", "); tags != "" {
				meta += " - " + tags
			}
			fmt.Fprintf(&sb, "%d. [%s] (%s)\n   %s\n\n",
				i+1, t.CreatedAt.Format("2006-01-02"), meta, t.Payload.Content)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func thoughtStats(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var lines []string

		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			var total int
			var earliest, latest time.Time
			if err := tx.QueryRow(ctx, `
				SELECT COUNT(*), MIN(created_at), MAX(created_at)
				FROM entries WHERE record_type = 'note.thought' AND deleted_at IS NULL`,
			).Scan(&total, &earliest, &latest); err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("Total thoughts: %d", total))
			if total > 0 {
				lines = append(lines, fmt.Sprintf("Date range: %s → %s",
					earliest.Format("2006-01-02"), latest.Format("2006-01-02")))
			}

			// Types breakdown.
			rows, err := tx.Query(ctx, `
				SELECT COALESCE(payload->>'thought_type', 'unknown'), COUNT(*)
				FROM entries
				WHERE record_type = 'note.thought' AND deleted_at IS NULL
				GROUP BY 1 ORDER BY 2 DESC`)
			if err != nil {
				return err
			}
			var section []string
			for rows.Next() {
				var k string
				var c int
				if err := rows.Scan(&k, &c); err != nil {
					rows.Close()
					return err
				}
				section = append(section, fmt.Sprintf("  %s: %d", k, c))
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(section) > 0 {
				lines = append(lines, "", "Types:")
				lines = append(lines, section...)
			}

			// Top topics.
			rows2, err := tx.Query(ctx, `
				SELECT topic, COUNT(*)
				FROM entries,
				     jsonb_array_elements_text(payload->'topics') AS topic
				WHERE record_type = 'note.thought'
				  AND deleted_at IS NULL
				  AND payload ? 'topics'
				GROUP BY topic ORDER BY 2 DESC LIMIT 10`)
			if err != nil {
				return err
			}
			var topics []string
			for rows2.Next() {
				var k string
				var c int
				if err := rows2.Scan(&k, &c); err != nil {
					rows2.Close()
					return err
				}
				topics = append(topics, fmt.Sprintf("  %s: %d", k, c))
			}
			rows2.Close()
			if err := rows2.Err(); err != nil {
				return err
			}
			if len(topics) > 0 {
				lines = append(lines, "", "Top topics:")
				lines = append(lines, topics...)
			}

			// People mentioned.
			rows3, err := tx.Query(ctx, `
				SELECT person, COUNT(*)
				FROM entries,
				     jsonb_array_elements_text(payload->'people') AS person
				WHERE record_type = 'note.thought'
				  AND deleted_at IS NULL
				  AND payload ? 'people'
				GROUP BY person ORDER BY 2 DESC LIMIT 10`)
			if err != nil {
				return err
			}
			var people []string
			for rows3.Next() {
				var k string
				var c int
				if err := rows3.Scan(&k, &c); err != nil {
					rows3.Close()
					return err
				}
				people = append(people, fmt.Sprintf("  %s: %d", k, c))
			}
			rows3.Close()
			if err := rows3.Err(); err != nil {
				return err
			}
			if len(people) > 0 {
				lines = append(lines, "", "People mentioned:")
				lines = append(lines, people...)
			}

			return nil
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		return brain.TextResult(strings.Join(lines, "\n")), nil
	}
}
