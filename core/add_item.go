// ABOUTME: add_item — unified MCP tool that replaces all per-type capture tools.
// ABOUTME: Usage: add_item <type> <content>
// ABOUTME: e.g. "add_item contact bob dobbs, engineer at acme"
// ABOUTME:      "add_item thought what is the meaning of life?"
// ABOUTME:      "add_item maintenance replace furnace filter every 90 days"

package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"open-brain-go/brain"
	"open-brain-go/brain/service"
)

// typeAliases maps short user-facing prefixes to canonical record_type values.
var typeAliases = map[string]string{
	"thought":     "note.thought",
	"note":        "note.thought",
	"contact":     "crm.contact",
	"interaction": "crm.interaction",
	"maintenance": "maintenance.task",
	"task":        "maintenance.task",
	"job":         "jobhunt.application",
	"application": "jobhunt.application",
	"link":        "note.link",
	// canonical names also accepted
	"note.thought":         "note.thought",
	"note.unstructured":    "note.unstructured",
	"crm.contact":          "crm.contact",
	"crm.interaction":      "crm.interaction",
	"maintenance.task":     "maintenance.task",
	"jobhunt.application":  "jobhunt.application",
	"note.link":            "note.link",
}

// RegisterAddItem adds the unified add_item tool to the MCP server.
// This tool replaces the per-type capture tools once cutover is complete.
func RegisterAddItem(s *server.MCPServer, a *brain.App, es *service.EntryService) {
	s.AddTool(mcp.NewTool("add_item",
		mcp.WithDescription(
			"Add any item to your knowledge base. "+
				"Prefix with a type: contact, thought, maintenance (or task), job (or application), interaction. "+
				"Examples: \"contact ada lovelace, engineer at acme\" or \"thought what is the meaning of life?\"",
		),
		mcp.WithString("type", mcp.Required(), mcp.Description(
			"Record type: contact, thought, maintenance, job, interaction "+
				"(or canonical: crm.contact, note.thought, maintenance.task, jobhunt.application, crm.interaction)",
		)),
		mcp.WithString("content", mcp.Required(), mcp.Description(
			"The content to capture. Include all relevant details.",
		)),
	), addItem(a, es))
}

func addItem(a *brain.App, es *service.EntryService) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		typeArg, _ := req.GetArguments()["type"].(string)
		content, _ := req.GetArguments()["content"].(string)

		typeArg = strings.TrimSpace(strings.ToLower(typeArg))
		content = strings.TrimSpace(content)

		if typeArg == "" {
			return brain.ToolError("type is required"), nil
		}
		if content == "" {
			return brain.ToolError("content is required"), nil
		}

		// Resolve type alias to canonical record_type.
		recordType, ok := typeAliases[typeArg]
		if !ok {
			known := strings.Join([]string{"contact", "link", "thought", "maintenance", "job", "interaction"}, ", ")
			return brain.ToolError(fmt.Sprintf("unknown type %q — use one of: %s", typeArg, known)), nil
		}

		result, err := es.CaptureTyped(ctx, recordType, content, "mcp")
		if err != nil {
			return brain.ToolError("Failed to add item: " + err.Error()), nil
		}
		return brain.TextResult(result.Message), nil
	}
}
