// ABOUTME: Home Maintenance Tracker extension — recurring tasks and service logs.
// ABOUTME: Adds tools for tracking maintenance schedules, logging completed work, and querying history.

package maintenance

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

// Register adds home maintenance tools to the MCP server.
func Register(s *server.MCPServer, a *brain.App) {
	s.AddTool(mcp.NewTool("add_maintenance_task",
		mcp.WithDescription("Add a recurring or one-time maintenance task (HVAC filter, gutter cleaning, etc.)"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Name of the maintenance task")),
		mcp.WithString("category", mcp.Description("Category (e.g. 'HVAC', 'plumbing', 'exterior', 'appliance')")),
		mcp.WithString("location", mcp.Description("Where in the home")),
		mcp.WithNumber("frequency_days", mcp.Description("How often in days (e.g. 90 for quarterly). Omit for one-time tasks.")),
		mcp.WithString("next_due", mcp.Description("Next due date (YYYY-MM-DD)")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), addTask(a))

	s.AddTool(mcp.NewTool("log_maintenance",
		mcp.WithDescription("Log completed maintenance work. Auto-calculates next due date for recurring tasks."),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("ID of the maintenance task")),
		mcp.WithString("completed_date", mcp.Description("Date completed (YYYY-MM-DD, defaults to today)")),
		mcp.WithString("performed_by", mcp.Description("Who did the work (you, vendor name, etc.)")),
		mcp.WithNumber("cost", mcp.Description("Cost of the maintenance")),
		mcp.WithString("notes", mcp.Description("Notes about the work done")),
	), logMaintenance(a))

	s.AddTool(mcp.NewTool("get_upcoming_maintenance",
		mcp.WithDescription("Show maintenance tasks due in the next N days"),
		mcp.WithNumber("days", mcp.Description("Number of days to look ahead (default 30)")),
	), getUpcoming(a))

	s.AddTool(mcp.NewTool("search_maintenance_history",
		mcp.WithDescription("Search maintenance logs by task, category, or date range"),
		mcp.WithString("task_id", mcp.Description("Filter by specific task ID")),
		mcp.WithString("category", mcp.Description("Filter by task category")),
		mcp.WithString("from_date", mcp.Description("Start of date range (YYYY-MM-DD)")),
		mcp.WithString("to_date", mcp.Description("End of date range (YYYY-MM-DD)")),
	), searchHistory(a))
}

type task struct {
	ID            string
	Name          string
	Category      *string
	Location      *string
	FrequencyDays *int
	NextDue       *time.Time
	LastCompleted *time.Time
	Notes         *string
	CreatedAt     time.Time
}

type logEntry struct {
	ID            string
	TaskName      string
	CompletedDate time.Time
	PerformedBy   *string
	Cost          *float64
	Notes         *string
}

func addTask(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, _ := req.GetArguments()["name"].(string)
		if name == "" {
			return brain.ToolError("name is required"), nil
		}
		category, _ := req.GetArguments()["category"].(string)
		location, _ := req.GetArguments()["location"].(string)
		notes, _ := req.GetArguments()["notes"].(string)
		nextDue, _ := req.GetArguments()["next_due"].(string)

		var freqDays *int
		if v, ok := req.GetArguments()["frequency_days"].(float64); ok && v > 0 {
			d := int(v)
			freqDays = &d
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO maintenance_tasks (user_id, name, category, location, frequency_days, next_due, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5::date, $6)
				RETURNING id::text`,
				name,
				nullOrStr(category),
				nullOrStr(location),
				freqDays,
				nullOrStr(nextDue),
				nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add task: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added maintenance task: %s (id: %s)", name, id)}
		if freqDays != nil {
			parts = append(parts, fmt.Sprintf("Frequency: every %d days", *freqDays))
		} else {
			parts = append(parts, "Type: one-time")
		}
		if nextDue != "" {
			parts = append(parts, "Next due: "+nextDue)
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func logMaintenance(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		taskID, _ := req.GetArguments()["task_id"].(string)
		if taskID == "" {
			return brain.ToolError("task_id is required"), nil
		}
		completedDate, _ := req.GetArguments()["completed_date"].(string)
		if completedDate == "" {
			completedDate = time.Now().Format("2006-01-02")
		}
		performedBy, _ := req.GetArguments()["performed_by"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		var cost *float64
		if v, ok := req.GetArguments()["cost"].(float64); ok {
			cost = &v
		}

		var logID string
		var taskName string
		var nextDueStr *string

		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Insert the log entry.
			err := tx.QueryRow(ctx, `
				INSERT INTO maintenance_logs (user_id, task_id, completed_date, performed_by, cost, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2::date, $3, $4, $5)
				RETURNING id::text`,
				taskID, completedDate, nullOrStr(performedBy), cost, nullOrStr(notes),
			).Scan(&logID)
			if err != nil {
				return err
			}

			// Update the task: set last_completed, recalculate next_due if recurring.
			err = tx.QueryRow(ctx, `
				UPDATE maintenance_tasks
				SET last_completed = $1::date,
				    next_due = CASE
				        WHEN frequency_days IS NOT NULL
				        THEN ($1::date + (frequency_days || ' days')::interval)::date
				        ELSE NULL
				    END
				WHERE id = $2
				RETURNING name, next_due::text`,
				completedDate, taskID,
			).Scan(&taskName, &nextDueStr)
			return err
		})
		if err != nil {
			return brain.ToolError("Failed to log maintenance: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Logged maintenance for: %s (log id: %s)", taskName, logID)}
		parts = append(parts, "Completed: "+completedDate)
		if cost != nil {
			parts = append(parts, fmt.Sprintf("Cost: $%.2f", *cost))
		}
		if nextDueStr != nil {
			parts = append(parts, "Next due: "+*nextDueStr)
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func getUpcoming(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		days := 30
		if v, ok := req.GetArguments()["days"].(float64); ok && v > 0 {
			days = int(v)
		}

		var tasks []task
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT id::text, name, category, location, frequency_days, next_due, last_completed, notes, created_at
				FROM maintenance_tasks
				WHERE next_due IS NOT NULL AND next_due <= CURRENT_DATE + $1 * interval '1 day'
				ORDER BY next_due ASC`, days)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var t task
				if err := rows.Scan(&t.ID, &t.Name, &t.Category, &t.Location, &t.FrequencyDays, &t.NextDue, &t.LastCompleted, &t.Notes, &t.CreatedAt); err != nil {
					return err
				}
				tasks = append(tasks, t)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(tasks) == 0 {
			return brain.TextResult(fmt.Sprintf("No maintenance due in the next %d days.", days)), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d task(s) due in the next %d days:\n\n", len(tasks), days)
		for _, t := range tasks {
			fmt.Fprintf(&sb, "• %s (id: %s)\n", t.Name, t.ID)
			if t.Category != nil {
				fmt.Fprintf(&sb, "  Category: %s\n", *t.Category)
			}
			if t.Location != nil {
				fmt.Fprintf(&sb, "  Location: %s\n", *t.Location)
			}
			if t.NextDue != nil {
				fmt.Fprintf(&sb, "  Due: %s\n", t.NextDue.Format("2006-01-02"))
			}
			if t.LastCompleted != nil {
				fmt.Fprintf(&sb, "  Last completed: %s\n", t.LastCompleted.Format("2006-01-02"))
			}
			if t.FrequencyDays != nil {
				fmt.Fprintf(&sb, "  Frequency: every %d days\n", *t.FrequencyDays)
			}
			fmt.Fprintln(&sb)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func searchHistory(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		taskID, _ := req.GetArguments()["task_id"].(string)
		category, _ := req.GetArguments()["category"].(string)
		fromDate, _ := req.GetArguments()["from_date"].(string)
		toDate, _ := req.GetArguments()["to_date"].(string)

		var entries []logEntry
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			sql := `SELECT l.id::text, t.name, l.completed_date, l.performed_by, l.cost, l.notes
			        FROM maintenance_logs l
			        JOIN maintenance_tasks t ON t.id = l.task_id
			        WHERE true`
			args := []any{}
			n := 1

			if taskID != "" {
				sql += fmt.Sprintf(" AND l.task_id = $%d", n)
				args = append(args, taskID)
				n++
			}
			if category != "" {
				sql += fmt.Sprintf(" AND t.category ILIKE $%d", n)
				args = append(args, "%"+category+"%")
				n++
			}
			if fromDate != "" {
				sql += fmt.Sprintf(" AND l.completed_date >= $%d::date", n)
				args = append(args, fromDate)
				n++
			}
			if toDate != "" {
				sql += fmt.Sprintf(" AND l.completed_date <= $%d::date", n)
				args = append(args, toDate)
				n++
			}
			sql += " ORDER BY l.completed_date DESC"

			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var e logEntry
				if err := rows.Scan(&e.ID, &e.TaskName, &e.CompletedDate, &e.PerformedBy, &e.Cost, &e.Notes); err != nil {
					return err
				}
				entries = append(entries, e)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(entries) == 0 {
			return brain.TextResult("No maintenance history found."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d log(s):\n\n", len(entries))
		for _, e := range entries {
			fmt.Fprintf(&sb, "• %s — %s\n", e.TaskName, e.CompletedDate.Format("2006-01-02"))
			if e.PerformedBy != nil {
				fmt.Fprintf(&sb, "  Performed by: %s\n", *e.PerformedBy)
			}
			if e.Cost != nil {
				fmt.Fprintf(&sb, "  Cost: $%.2f\n", *e.Cost)
			}
			if e.Notes != nil {
				fmt.Fprintf(&sb, "  Notes: %s\n", *e.Notes)
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
