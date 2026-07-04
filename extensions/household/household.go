// ABOUTME: Household Knowledge Base extension — home items and vendor contacts.
// ABOUTME: Adds tools for storing and querying household facts and service providers.

package household

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

// Register adds household knowledge tools to the MCP server.
func Register(s *server.MCPServer, a *brain.App) {
	s.AddTool(mcp.NewTool("add_household_item",
		mcp.WithDescription("Add a household item to your home knowledge base (paint colors, appliances, measurements, documents, etc.)"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Name or description of the item")),
		mcp.WithString("category", mcp.Description("Category (e.g. 'paint', 'appliance', 'measurement', 'document')")),
		mcp.WithString("location", mcp.Description("Where in the home (e.g. 'Living Room', 'Kitchen')")),
		mcp.WithString("details", mcp.Description("Flexible metadata as JSON (e.g. '{\"brand\": \"Sherwin Williams\", \"color\": \"Sea Salt\"}')")),
		mcp.WithString("notes", mcp.Description("Additional notes or context")),
	), addItem(a))

	s.AddTool(mcp.NewTool("search_household_items",
		mcp.WithDescription("Search household items by name, category, or location"),
		mcp.WithString("query", mcp.Description("Search term (matches name, category, location, and notes)")),
		mcp.WithString("category", mcp.Description("Filter by specific category")),
		mcp.WithString("location", mcp.Description("Filter by specific location")),
	), searchItems(a))

	s.AddTool(mcp.NewTool("add_vendor",
		mcp.WithDescription("Add a service provider (plumber, electrician, landscaper, etc.)"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Vendor name")),
		mcp.WithString("service_type", mcp.Description("Type of service (e.g. 'plumber', 'electrician', 'landscaper')")),
		mcp.WithString("phone", mcp.Description("Phone number")),
		mcp.WithString("email", mcp.Description("Email address")),
		mcp.WithString("website", mcp.Description("Website URL")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
		mcp.WithNumber("rating", mcp.Description("Rating from 1–5")),
		mcp.WithString("last_used", mcp.Description("Date last used (YYYY-MM-DD)")),
	), addVendor(a))

	s.AddTool(mcp.NewTool("list_vendors",
		mcp.WithDescription("List service providers, optionally filtered by service type"),
		mcp.WithString("service_type", mcp.Description("Filter by service type (e.g. 'plumber', 'electrician')")),
	), listVendors(a))
}

type item struct {
	ID        string
	Name      string
	Category  *string
	Location  *string
	Details   json.RawMessage
	Notes     *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type vendor struct {
	ID          string
	Name        string
	ServiceType *string
	Phone       *string
	Email       *string
	Website     *string
	Notes       *string
	Rating      *int
	LastUsed    *time.Time
	CreatedAt   time.Time
}

func addItem(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, _ := req.GetArguments()["name"].(string)
		if name == "" {
			return brain.ToolError("name is required"), nil
		}
		category, _ := req.GetArguments()["category"].(string)
		location, _ := req.GetArguments()["location"].(string)
		detailsStr, _ := req.GetArguments()["details"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		details := json.RawMessage("{}")
		if detailsStr != "" {
			details = json.RawMessage(detailsStr)
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO household_items (user_id, name, category, location, details, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5)
				RETURNING id::text`,
				name,
				nullOrStr(category),
				nullOrStr(location),
				details,
				nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add item: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added: %s (id: %s)", name, id)}
		if category != "" {
			parts = append(parts, "Category: "+category)
		}
		if location != "" {
			parts = append(parts, "Location: "+location)
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func searchItems(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := req.GetArguments()["query"].(string)
		category, _ := req.GetArguments()["category"].(string)
		location, _ := req.GetArguments()["location"].(string)

		var items []item
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			sql := `SELECT id::text, name, category, location, details, notes, created_at, updated_at
			        FROM household_items WHERE true`
			args := []any{}
			n := 1

			if category != "" {
				sql += fmt.Sprintf(" AND category ILIKE $%d", n)
				args = append(args, "%"+category+"%")
				n++
			}
			if location != "" {
				sql += fmt.Sprintf(" AND location ILIKE $%d", n)
				args = append(args, "%"+location+"%")
				n++
			}
			if query != "" {
				sql += fmt.Sprintf(" AND (name ILIKE $%d OR category ILIKE $%d OR location ILIKE $%d OR notes ILIKE $%d)", n, n, n, n)
				args = append(args, "%"+query+"%")
				n++
			}
			sql += " ORDER BY created_at DESC"

			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var it item
				if err := rows.Scan(&it.ID, &it.Name, &it.Category, &it.Location, &it.Details, &it.Notes, &it.CreatedAt, &it.UpdatedAt); err != nil {
					return err
				}
				items = append(items, it)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Search error: " + err.Error()), nil
		}

		if len(items) == 0 {
			return brain.TextResult("No household items found."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d item(s):\n\n", len(items))
		for _, it := range items {
			fmt.Fprintf(&sb, "• %s (id: %s)\n", it.Name, it.ID)
			if it.Category != nil {
				fmt.Fprintf(&sb, "  Category: %s\n", *it.Category)
			}
			if it.Location != nil {
				fmt.Fprintf(&sb, "  Location: %s\n", *it.Location)
			}
			if it.Notes != nil {
				fmt.Fprintf(&sb, "  Notes: %s\n", *it.Notes)
			}
			fmt.Fprintf(&sb, "  Added: %s\n\n", it.CreatedAt.Format("2006-01-02"))
		}
		return brain.TextResult(sb.String()), nil
	}
}

func addVendor(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, _ := req.GetArguments()["name"].(string)
		if name == "" {
			return brain.ToolError("name is required"), nil
		}
		serviceType, _ := req.GetArguments()["service_type"].(string)
		phone, _ := req.GetArguments()["phone"].(string)
		email, _ := req.GetArguments()["email"].(string)
		website, _ := req.GetArguments()["website"].(string)
		notes, _ := req.GetArguments()["notes"].(string)
		lastUsed, _ := req.GetArguments()["last_used"].(string)

		var rating *int
		if v, ok := req.GetArguments()["rating"].(float64); ok && v >= 1 && v <= 5 {
			r := int(v)
			rating = &r
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO household_vendors (user_id, name, service_type, phone, email, website, notes, rating, last_used)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5, $6, $7, $8)
				RETURNING id::text`,
				name,
				nullOrStr(serviceType),
				nullOrStr(phone),
				nullOrStr(email),
				nullOrStr(website),
				nullOrStr(notes),
				rating,
				nullOrStr(lastUsed),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add vendor: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added vendor: %s (id: %s)", name, id)}
		if serviceType != "" {
			parts = append(parts, "Service: "+serviceType)
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func listVendors(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		serviceType, _ := req.GetArguments()["service_type"].(string)

		var vendors []vendor
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			sql := `SELECT id::text, name, service_type, phone, email, website, notes, rating, last_used, created_at
			        FROM household_vendors WHERE true`
			args := []any{}

			if serviceType != "" {
				sql += " AND service_type ILIKE $1"
				args = append(args, "%"+serviceType+"%")
			}
			sql += " ORDER BY name ASC"

			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var v vendor
				if err := rows.Scan(&v.ID, &v.Name, &v.ServiceType, &v.Phone, &v.Email, &v.Website, &v.Notes, &v.Rating, &v.LastUsed, &v.CreatedAt); err != nil {
					return err
				}
				vendors = append(vendors, v)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error listing vendors: " + err.Error()), nil
		}

		if len(vendors) == 0 {
			return brain.TextResult("No vendors found."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d vendor(s):\n\n", len(vendors))
		for _, v := range vendors {
			fmt.Fprintf(&sb, "• %s (id: %s)\n", v.Name, v.ID)
			if v.ServiceType != nil {
				fmt.Fprintf(&sb, "  Service: %s\n", *v.ServiceType)
			}
			if v.Phone != nil {
				fmt.Fprintf(&sb, "  Phone: %s\n", *v.Phone)
			}
			if v.Email != nil {
				fmt.Fprintf(&sb, "  Email: %s\n", *v.Email)
			}
			if v.Rating != nil {
				fmt.Fprintf(&sb, "  Rating: %d/5\n", *v.Rating)
			}
			if v.LastUsed != nil {
				fmt.Fprintf(&sb, "  Last used: %s\n", v.LastUsed.Format("2006-01-02"))
			}
			fmt.Fprintln(&sb)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func nullOrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
