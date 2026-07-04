// ABOUTME: Professional CRM extension — contact management and interaction tracking.
// ABOUTME: Adds tools for managing professional contacts, logging interactions, tracking opportunities, and linking thoughts.

package crm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"substrate/brain"
)

// Register adds professional CRM tools to the MCP server.
func Register(s *server.MCPServer, a *brain.App) {
	s.AddTool(mcp.NewTool("add_professional_contact",
		mcp.WithDescription("Add a professional contact to your CRM"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Contact's full name")),
		mcp.WithString("company", mcp.Description("Company name")),
		mcp.WithString("title", mcp.Description("Job title")),
		mcp.WithString("email", mcp.Description("Email address")),
		mcp.WithString("phone", mcp.Description("Phone number")),
		mcp.WithString("linkedin_url", mcp.Description("LinkedIn profile URL")),
		mcp.WithString("how_we_met", mcp.Description("How you met this person")),
		mcp.WithString("tags", mcp.Description("Comma-separated tags (e.g. 'AI, conference, potential-client')")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), addContact(a))

	s.AddTool(mcp.NewTool("search_contacts",
		mcp.WithDescription("Search professional contacts by name, company, or tag"),
		mcp.WithString("query", mcp.Description("Search term (matches name, company, notes)")),
		mcp.WithString("company", mcp.Description("Filter by company")),
		mcp.WithString("tag", mcp.Description("Filter by tag")),
	), searchContacts(a))

	s.AddTool(mcp.NewTool("log_interaction",
		mcp.WithDescription("Log a professional interaction (meeting, call, email, etc.)"),
		mcp.WithString("contact_id", mcp.Required(), mcp.Description("Contact ID")),
		mcp.WithString("interaction_type", mcp.Required(), mcp.Description("Type: email, call, meeting, coffee, conference, linkedin, other")),
		mcp.WithString("summary", mcp.Required(), mcp.Description("Summary of the interaction")),
		mcp.WithBoolean("follow_up_needed", mcp.Description("Whether follow-up is needed")),
		mcp.WithString("follow_up_notes", mcp.Description("Notes about what to follow up on")),
		mcp.WithString("interaction_date", mcp.Description("Date of interaction (YYYY-MM-DD, defaults to today)")),
	), logInteraction(a))

	s.AddTool(mcp.NewTool("get_contact_history",
		mcp.WithDescription("Get a contact's full profile with interaction history and opportunities"),
		mcp.WithString("contact_id", mcp.Required(), mcp.Description("Contact ID")),
	), getContactHistory(a))

	s.AddTool(mcp.NewTool("create_opportunity",
		mcp.WithDescription("Create a business opportunity linked to a contact"),
		mcp.WithString("title", mcp.Required(), mcp.Description("Opportunity title")),
		mcp.WithString("contact_id", mcp.Description("Contact ID to link")),
		mcp.WithString("description", mcp.Description("Description of the opportunity")),
		mcp.WithString("stage", mcp.Description("Pipeline stage: identified, in_conversation, proposal, negotiation, won, lost (default: identified)")),
		mcp.WithNumber("value", mcp.Description("Estimated value in dollars")),
		mcp.WithString("expected_close_date", mcp.Description("Expected close date (YYYY-MM-DD)")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), createOpportunity(a))

	s.AddTool(mcp.NewTool("get_follow_ups_due",
		mcp.WithDescription("List contacts that need follow-up in the next N days"),
		mcp.WithNumber("days", mcp.Description("Number of days to look ahead (default 7)")),
	), getFollowUpsDue(a))

	s.AddTool(mcp.NewTool("link_thought_to_contact",
		mcp.WithDescription("Link a thought from your Open Brain to a professional contact"),
		mcp.WithString("thought_id", mcp.Required(), mcp.Description("ID of the thought to link")),
		mcp.WithString("contact_id", mcp.Required(), mcp.Description("ID of the contact to link to")),
	), linkThought(a))
}

type contact struct {
	ID            string
	Name          string
	Company       *string
	Title         *string
	Email         *string
	Phone         *string
	LinkedInURL   *string
	HowWeMet      *string
	Tags          []string
	Notes         *string
	LastContacted *time.Time
	FollowUpDate  *time.Time
	CreatedAt     time.Time
}

type interaction struct {
	ID              string
	InteractionType string
	Summary         string
	FollowUpNeeded  bool
	FollowUpNotes   *string
	InteractionDate time.Time
}

type opportunity struct {
	ID                string
	Title             string
	Description       *string
	Stage             string
	Value             *float64
	ExpectedCloseDate *time.Time
	Notes             *string
}

func addContact(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, _ := req.GetArguments()["name"].(string)
		if name == "" {
			return brain.ToolError("name is required"), nil
		}
		company, _ := req.GetArguments()["company"].(string)
		title, _ := req.GetArguments()["title"].(string)
		email, _ := req.GetArguments()["email"].(string)
		phone, _ := req.GetArguments()["phone"].(string)
		linkedinURL, _ := req.GetArguments()["linkedin_url"].(string)
		howWeMet, _ := req.GetArguments()["how_we_met"].(string)
		tagsStr, _ := req.GetArguments()["tags"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		var tags []string
		if tagsStr != "" {
			for _, t := range strings.Split(tagsStr, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					tags = append(tags, t)
				}
			}
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO professional_contacts (user_id, name, company, title, email, phone, linkedin_url, how_we_met, tags, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5, $6, $7, $8, $9)
				RETURNING id::text`,
				name, nullOrStr(company), nullOrStr(title), nullOrStr(email),
				nullOrStr(phone), nullOrStr(linkedinURL), nullOrStr(howWeMet),
				tags, nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add contact: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added contact: %s (id: %s)", name, id)}
		if company != "" {
			parts = append(parts, "Company: "+company)
		}
		if title != "" {
			parts = append(parts, "Title: "+title)
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func searchContacts(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := req.GetArguments()["query"].(string)
		company, _ := req.GetArguments()["company"].(string)
		tag, _ := req.GetArguments()["tag"].(string)

		var contacts []contact
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			sql := `SELECT id::text, name, company, title, email, phone, linkedin_url,
			               how_we_met, tags, notes, last_contacted, follow_up_date, created_at
			        FROM professional_contacts WHERE true`
			args := []any{}
			n := 1

			if query != "" {
				sql += fmt.Sprintf(" AND (name ILIKE $%d OR company ILIKE $%d OR notes ILIKE $%d)", n, n, n)
				args = append(args, "%"+query+"%")
				n++
			}
			if company != "" {
				sql += fmt.Sprintf(" AND company ILIKE $%d", n)
				args = append(args, "%"+company+"%")
				n++
			}
			if tag != "" {
				sql += fmt.Sprintf(" AND $%d = ANY(tags)", n)
				args = append(args, tag)
				n++
			}
			sql += " ORDER BY name ASC"

			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var c contact
				if err := rows.Scan(&c.ID, &c.Name, &c.Company, &c.Title, &c.Email, &c.Phone,
					&c.LinkedInURL, &c.HowWeMet, &c.Tags, &c.Notes, &c.LastContacted,
					&c.FollowUpDate, &c.CreatedAt); err != nil {
					return err
				}
				contacts = append(contacts, c)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(contacts) == 0 {
			return brain.TextResult("No contacts found."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d contact(s):\n\n", len(contacts))
		for _, c := range contacts {
			fmt.Fprintf(&sb, "• %s (id: %s)\n", c.Name, c.ID)
			if c.Company != nil {
				fmt.Fprintf(&sb, "  Company: %s\n", *c.Company)
			}
			if c.Title != nil {
				fmt.Fprintf(&sb, "  Title: %s\n", *c.Title)
			}
			if c.LastContacted != nil {
				fmt.Fprintf(&sb, "  Last contacted: %s\n", c.LastContacted.Format("2006-01-02"))
			}
			if len(c.Tags) > 0 {
				fmt.Fprintf(&sb, "  Tags: %s\n", strings.Join(c.Tags, ", "))
			}
			fmt.Fprintln(&sb)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func logInteraction(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		contactID, _ := req.GetArguments()["contact_id"].(string)
		interactionType, _ := req.GetArguments()["interaction_type"].(string)
		summary, _ := req.GetArguments()["summary"].(string)
		if contactID == "" || interactionType == "" || summary == "" {
			return brain.ToolError("contact_id, interaction_type, and summary are required"), nil
		}

		followUpNotes, _ := req.GetArguments()["follow_up_notes"].(string)
		interactionDate, _ := req.GetArguments()["interaction_date"].(string)
		if interactionDate == "" {
			interactionDate = time.Now().Format("2006-01-02")
		}

		followUpNeeded := false
		if v, ok := req.GetArguments()["follow_up_needed"].(bool); ok {
			followUpNeeded = v
		}

		var logID string
		var contactName string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Insert interaction.
			err := tx.QueryRow(ctx, `
				INSERT INTO contact_interactions (user_id, contact_id, interaction_type, summary, follow_up_needed, follow_up_notes, interaction_date)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5, $6::date)
				RETURNING id::text`,
				contactID, interactionType, summary, followUpNeeded,
				nullOrStr(followUpNotes), interactionDate,
			).Scan(&logID)
			if err != nil {
				return err
			}

			// Update contact's last_contacted.
			return tx.QueryRow(ctx,
				"UPDATE professional_contacts SET last_contacted = $1::date WHERE id = $2 RETURNING name",
				interactionDate, contactID,
			).Scan(&contactName)
		})
		if err != nil {
			return brain.ToolError("Failed to log interaction: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Logged %s with %s (id: %s)", interactionType, contactName, logID)}
		if followUpNeeded {
			parts = append(parts, "Follow-up needed: yes")
			if followUpNotes != "" {
				parts = append(parts, "Follow-up: "+followUpNotes)
			}
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func getContactHistory(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		contactID, _ := req.GetArguments()["contact_id"].(string)
		if contactID == "" {
			return brain.ToolError("contact_id is required"), nil
		}

		var c contact
		var interactions []interaction
		var opportunities []opportunity

		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Get contact.
			err := tx.QueryRow(ctx, `
				SELECT id::text, name, company, title, email, phone, linkedin_url,
				       how_we_met, tags, notes, last_contacted, follow_up_date, created_at
				FROM professional_contacts WHERE id = $1`, contactID,
			).Scan(&c.ID, &c.Name, &c.Company, &c.Title, &c.Email, &c.Phone,
				&c.LinkedInURL, &c.HowWeMet, &c.Tags, &c.Notes, &c.LastContacted,
				&c.FollowUpDate, &c.CreatedAt)
			if err != nil {
				return err
			}

			// Get interactions.
			rows, err := tx.Query(ctx, `
				SELECT id::text, interaction_type, summary, follow_up_needed, follow_up_notes, interaction_date
				FROM contact_interactions
				WHERE contact_id = $1
				ORDER BY interaction_date DESC`, contactID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var i interaction
				if err := rows.Scan(&i.ID, &i.InteractionType, &i.Summary,
					&i.FollowUpNeeded, &i.FollowUpNotes, &i.InteractionDate); err != nil {
					return err
				}
				interactions = append(interactions, i)
			}
			if err := rows.Err(); err != nil {
				return err
			}

			// Get opportunities.
			rows2, err := tx.Query(ctx, `
				SELECT id::text, title, description, stage, value, expected_close_date, notes
				FROM opportunities
				WHERE contact_id = $1
				ORDER BY created_at DESC`, contactID)
			if err != nil {
				return err
			}
			defer rows2.Close()
			for rows2.Next() {
				var o opportunity
				if err := rows2.Scan(&o.ID, &o.Title, &o.Description, &o.Stage,
					&o.Value, &o.ExpectedCloseDate, &o.Notes); err != nil {
					return err
				}
				opportunities = append(opportunities, o)
			}
			return rows2.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "=== %s ===\n", c.Name)
		if c.Company != nil {
			fmt.Fprintf(&sb, "Company: %s\n", *c.Company)
		}
		if c.Title != nil {
			fmt.Fprintf(&sb, "Title: %s\n", *c.Title)
		}
		if c.Email != nil {
			fmt.Fprintf(&sb, "Email: %s\n", *c.Email)
		}
		if c.Phone != nil {
			fmt.Fprintf(&sb, "Phone: %s\n", *c.Phone)
		}
		if c.HowWeMet != nil {
			fmt.Fprintf(&sb, "How we met: %s\n", *c.HowWeMet)
		}
		if len(c.Tags) > 0 {
			fmt.Fprintf(&sb, "Tags: %s\n", strings.Join(c.Tags, ", "))
		}
		if c.LastContacted != nil {
			fmt.Fprintf(&sb, "Last contacted: %s\n", c.LastContacted.Format("2006-01-02"))
		}
		if c.Notes != nil {
			fmt.Fprintf(&sb, "Notes: %s\n", *c.Notes)
		}
		fmt.Fprintf(&sb, "Added: %s\n", c.CreatedAt.Format("2006-01-02"))

		if len(interactions) > 0 {
			fmt.Fprintf(&sb, "\n--- Interactions (%d) ---\n", len(interactions))
			for _, i := range interactions {
				fmt.Fprintf(&sb, "• [%s] %s — %s\n", i.InteractionDate.Format("2006-01-02"), i.InteractionType, i.Summary)
				if i.FollowUpNeeded && i.FollowUpNotes != nil {
					fmt.Fprintf(&sb, "  Follow-up: %s\n", *i.FollowUpNotes)
				}
			}
		}

		if len(opportunities) > 0 {
			fmt.Fprintf(&sb, "\n--- Opportunities (%d) ---\n", len(opportunities))
			for _, o := range opportunities {
				fmt.Fprintf(&sb, "• %s [%s]", o.Title, o.Stage)
				if o.Value != nil {
					fmt.Fprintf(&sb, " — $%.0f", *o.Value)
				}
				fmt.Fprintln(&sb)
				if o.Description != nil {
					fmt.Fprintf(&sb, "  %s\n", *o.Description)
				}
			}
		}

		return brain.TextResult(sb.String()), nil
	}
}

func createOpportunity(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title, _ := req.GetArguments()["title"].(string)
		if title == "" {
			return brain.ToolError("title is required"), nil
		}
		contactID, _ := req.GetArguments()["contact_id"].(string)
		description, _ := req.GetArguments()["description"].(string)
		stage, _ := req.GetArguments()["stage"].(string)
		if stage == "" {
			stage = "identified"
		}
		expectedCloseDate, _ := req.GetArguments()["expected_close_date"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		var value *float64
		if v, ok := req.GetArguments()["value"].(float64); ok {
			value = &v
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO opportunities (user_id, contact_id, title, description, stage, value, expected_close_date, notes)
				VALUES (current_setting('app.current_user_id')::uuid, NULLIF($1, '')::uuid, $2, $3, $4, $5, $6::date, $7)
				RETURNING id::text`,
				contactID, title, nullOrStr(description), stage,
				value, nullOrStr(expectedCloseDate), nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to create opportunity: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Created opportunity: %s (id: %s)", title, id)}
		parts = append(parts, "Stage: "+stage)
		if value != nil {
			parts = append(parts, fmt.Sprintf("Value: $%.0f", *value))
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func getFollowUpsDue(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		days := 7
		if v, ok := req.GetArguments()["days"].(float64); ok && v > 0 {
			days = int(v)
		}

		var contacts []contact
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT DISTINCT c.id::text, c.name, c.company, c.title, c.email, c.phone,
				       c.linkedin_url, c.how_we_met, c.tags, c.notes,
				       c.last_contacted, c.follow_up_date, c.created_at
				FROM professional_contacts c
				LEFT JOIN contact_interactions ci ON ci.contact_id = c.id AND ci.follow_up_needed = true
				WHERE c.follow_up_date <= CURRENT_DATE + $1 * interval '1 day'
				   OR ci.id IS NOT NULL
				ORDER BY c.follow_up_date ASC NULLS LAST, c.last_contacted ASC NULLS FIRST`, days)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var c contact
				if err := rows.Scan(&c.ID, &c.Name, &c.Company, &c.Title, &c.Email, &c.Phone,
					&c.LinkedInURL, &c.HowWeMet, &c.Tags, &c.Notes, &c.LastContacted,
					&c.FollowUpDate, &c.CreatedAt); err != nil {
					return err
				}
				contacts = append(contacts, c)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(contacts) == 0 {
			return brain.TextResult("No follow-ups due."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d contact(s) need follow-up:\n\n", len(contacts))
		for _, c := range contacts {
			fmt.Fprintf(&sb, "• %s (id: %s)\n", c.Name, c.ID)
			if c.Company != nil {
				fmt.Fprintf(&sb, "  Company: %s\n", *c.Company)
			}
			if c.LastContacted != nil {
				fmt.Fprintf(&sb, "  Last contacted: %s\n", c.LastContacted.Format("2006-01-02"))
			}
			if c.FollowUpDate != nil {
				fmt.Fprintf(&sb, "  Follow-up date: %s\n", c.FollowUpDate.Format("2006-01-02"))
			}
			fmt.Fprintln(&sb)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func linkThought(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		thoughtID, _ := req.GetArguments()["thought_id"].(string)
		contactID, _ := req.GetArguments()["contact_id"].(string)
		if thoughtID == "" || contactID == "" {
			return brain.ToolError("thought_id and contact_id are required"), nil
		}

		var contactName, thoughtContent string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Get the thought content.
			err := tx.QueryRow(ctx,
				"SELECT content FROM thoughts WHERE id = $1", thoughtID,
			).Scan(&thoughtContent)
			if err != nil {
				return fmt.Errorf("thought not found: %w", err)
			}

			// Append to contact notes.
			err = tx.QueryRow(ctx, `
				UPDATE professional_contacts
				SET notes = COALESCE(notes, '') || E'\n\n[Linked thought ' || $1 || ']: ' || $2
				WHERE id = $3
				RETURNING name`,
				thoughtID, thoughtContent, contactID,
			).Scan(&contactName)
			return err
		})
		if err != nil {
			return brain.ToolError("Failed to link thought: " + err.Error()), nil
		}

		snippet := thoughtContent
		if len(snippet) > 100 {
			snippet = snippet[:100] + "..."
		}
		return brain.TextResult(fmt.Sprintf("Linked thought to %s: \"%s\"", contactName, snippet)), nil
	}
}

func nullOrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
