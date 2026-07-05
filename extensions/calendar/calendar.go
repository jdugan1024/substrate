// ABOUTME: Family Calendar extension — multi-person scheduling and important dates.
// ABOUTME: Adds tools for managing family members, activities, and date reminders.

package calendar

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

// Register adds family calendar tools to the MCP server.
func Register(s *server.MCPServer, a *brain.App) {
	s.AddTool(mcp.NewTool("add_family_member",
		mcp.WithDescription("Add a person to your family roster"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Person's name")),
		mcp.WithString("relationship", mcp.Description("Relationship (e.g. 'self', 'spouse', 'child')")),
		mcp.WithString("date_of_birth", mcp.Description("Date of birth (YYYY-MM-DD)")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), addMember(a))

	s.AddTool(mcp.NewTool("add_activity",
		mcp.WithDescription("Schedule a one-time or recurring activity"),
		mcp.WithString("title", mcp.Required(), mcp.Description("Activity title")),
		mcp.WithString("family_member_id", mcp.Description("Family member ID (omit for whole-family events)")),
		mcp.WithString("activity_type", mcp.Description("Type (e.g. 'appointment', 'class', 'practice', 'event')")),
		mcp.WithString("location", mcp.Description("Location")),
		mcp.WithString("start_date", mcp.Required(), mcp.Description("Start date (YYYY-MM-DD)")),
		mcp.WithString("end_date", mcp.Description("End date (YYYY-MM-DD)")),
		mcp.WithString("start_time", mcp.Description("Start time (HH:MM)")),
		mcp.WithString("end_time", mcp.Description("End time (HH:MM)")),
		mcp.WithString("day_of_week", mcp.Description("For recurring: day of week (monday, tuesday, etc.)")),
		mcp.WithBoolean("recurring", mcp.Description("Whether this is a recurring activity")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), addActivity(a))

	s.AddTool(mcp.NewTool("get_week_schedule",
		mcp.WithDescription("View everyone's schedule for a given week"),
		mcp.WithString("week_start", mcp.Required(), mcp.Description("Monday of the week (YYYY-MM-DD)")),
	), getWeekSchedule(a))

	s.AddTool(mcp.NewTool("search_activities",
		mcp.WithDescription("Search activities by title, type, or family member"),
		mcp.WithString("query", mcp.Description("Search term (matches title and notes)")),
		mcp.WithString("activity_type", mcp.Description("Filter by activity type")),
		mcp.WithString("family_member_id", mcp.Description("Filter by family member")),
	), searchActivities(a))

	s.AddTool(mcp.NewTool("add_important_date",
		mcp.WithDescription("Track a birthday, anniversary, deadline, or other important date"),
		mcp.WithString("title", mcp.Required(), mcp.Description("What the date is for")),
		mcp.WithString("event_date", mcp.Required(), mcp.Description("The date (YYYY-MM-DD)")),
		mcp.WithString("family_member_id", mcp.Description("Family member ID if applicable")),
		mcp.WithBoolean("recurring_yearly", mcp.Description("Recurs every year (e.g. birthdays)")),
		mcp.WithNumber("reminder_days_before", mcp.Description("Days before to remind (default 7)")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), addDate(a))

	s.AddTool(mcp.NewTool("get_upcoming_dates",
		mcp.WithDescription("Show important dates coming up in the next N days"),
		mcp.WithNumber("days", mcp.Description("Number of days to look ahead (default 30)")),
	), getUpcomingDates(a))
}

type member struct {
	ID           string
	Name         string
	Relationship *string
	DateOfBirth  *time.Time
	Notes        *string
}

type activity struct {
	ID           string
	Title        string
	MemberName   *string
	ActivityType *string
	Location     *string
	StartDate    time.Time
	EndDate      *time.Time
	StartTime    *string
	EndTime      *string
	DayOfWeek    *string
	Recurring    bool
	Notes        *string
}

type importantDate struct {
	ID              string
	Title           string
	MemberName      *string
	EventDate       time.Time
	RecurringYearly bool
	ReminderDays    *int
	Notes           *string
}

func addMember(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, _ := req.GetArguments()["name"].(string)
		if name == "" {
			return brain.ToolError("name is required"), nil
		}
		relationship, _ := req.GetArguments()["relationship"].(string)
		dob, _ := req.GetArguments()["date_of_birth"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO family_members (user_id, name, relationship, date_of_birth, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3::date, $4)
				RETURNING id::text`,
				name, nullOrStr(relationship), nullOrStr(dob), nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add member: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added family member: %s (id: %s)", name, id)}
		if relationship != "" {
			parts = append(parts, "Relationship: "+relationship)
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func addActivity(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title, _ := req.GetArguments()["title"].(string)
		if title == "" {
			return brain.ToolError("title is required"), nil
		}
		startDate, _ := req.GetArguments()["start_date"].(string)
		if startDate == "" {
			return brain.ToolError("start_date is required"), nil
		}
		memberID, _ := req.GetArguments()["family_member_id"].(string)
		actType, _ := req.GetArguments()["activity_type"].(string)
		location, _ := req.GetArguments()["location"].(string)
		endDate, _ := req.GetArguments()["end_date"].(string)
		startTime, _ := req.GetArguments()["start_time"].(string)
		endTime, _ := req.GetArguments()["end_time"].(string)
		dayOfWeek, _ := req.GetArguments()["day_of_week"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		recurring := false
		if v, ok := req.GetArguments()["recurring"].(bool); ok {
			recurring = v
		}
		// If day_of_week is set, it's recurring.
		if dayOfWeek != "" {
			recurring = true
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO activities (user_id, family_member_id, title, activity_type, location,
				    start_date, end_date, start_time, end_time, day_of_week, recurring, notes)
				VALUES (current_setting('app.current_user_id')::uuid,
				    NULLIF($1, '')::uuid, $2, $3, $4,
				    $5::date, $6::date, $7::time, $8::time, $9, $10, $11)
				RETURNING id::text`,
				memberID, title, nullOrStr(actType), nullOrStr(location),
				startDate, nullOrStr(endDate), nullOrStr(startTime), nullOrStr(endTime),
				nullOrStr(strings.ToLower(dayOfWeek)), recurring, nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add activity: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added activity: %s (id: %s)", title, id)}
		if recurring {
			parts = append(parts, "Recurring: yes")
			if dayOfWeek != "" {
				parts = append(parts, "Day: "+strings.ToLower(dayOfWeek))
			}
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func getWeekSchedule(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		weekStart, _ := req.GetArguments()["week_start"].(string)
		if weekStart == "" {
			return brain.ToolError("week_start is required"), nil
		}

		var activities []activity
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT a.id::text, a.title, fm.name, a.activity_type, a.location,
				       a.start_date, a.end_date, a.start_time::text, a.end_time::text,
				       a.day_of_week, a.recurring, a.notes
				FROM activities a
				LEFT JOIN family_members fm ON fm.id = a.family_member_id
				WHERE (
				    -- One-time events in this week
				    (a.recurring = false AND a.start_date BETWEEN $1::date AND ($1::date + interval '6 days'))
				    OR
				    -- Recurring events that started on or before this week's end
				    (a.recurring = true AND a.start_date <= ($1::date + interval '6 days')
				     AND (a.end_date IS NULL OR a.end_date >= $1::date))
				)
				ORDER BY a.day_of_week, a.start_time NULLS LAST, a.title`,
				weekStart)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var act activity
				if err := rows.Scan(&act.ID, &act.Title, &act.MemberName, &act.ActivityType,
					&act.Location, &act.StartDate, &act.EndDate, &act.StartTime, &act.EndTime,
					&act.DayOfWeek, &act.Recurring, &act.Notes); err != nil {
					return err
				}
				activities = append(activities, act)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(activities) == 0 {
			return brain.TextResult("No activities scheduled for the week of " + weekStart), nil
		}

		// Group by day.
		days := []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
		ws, _ := time.Parse("2006-01-02", weekStart)

		var sb strings.Builder
		fmt.Fprintf(&sb, "Schedule for week of %s:\n", weekStart)

		for i, day := range days {
			date := ws.AddDate(0, 0, i)
			var dayActs []activity
			for _, act := range activities {
				if act.Recurring && act.DayOfWeek != nil && *act.DayOfWeek == day {
					dayActs = append(dayActs, act)
				} else if !act.Recurring {
					actDate := act.StartDate.Format("2006-01-02")
					if actDate == date.Format("2006-01-02") {
						dayActs = append(dayActs, act)
					}
				}
			}
			if len(dayActs) == 0 {
				continue
			}
			fmt.Fprintf(&sb, "\n%s %s:\n", titleCase(day), date.Format("01/02"))
			for _, act := range dayActs {
				who := "Family"
				if act.MemberName != nil {
					who = *act.MemberName
				}
				timeStr := ""
				if act.StartTime != nil {
					timeStr = " " + *act.StartTime
					if act.EndTime != nil {
						timeStr += "–" + *act.EndTime
					}
				}
				fmt.Fprintf(&sb, "  • %s [%s]%s\n", act.Title, who, timeStr)
				if act.Location != nil {
					fmt.Fprintf(&sb, "    Location: %s\n", *act.Location)
				}
			}
		}
		return brain.TextResult(sb.String()), nil
	}
}

func searchActivities(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := req.GetArguments()["query"].(string)
		actType, _ := req.GetArguments()["activity_type"].(string)
		memberID, _ := req.GetArguments()["family_member_id"].(string)

		var activities []activity
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			sql := `SELECT a.id::text, a.title, fm.name, a.activity_type, a.location,
			               a.start_date, a.end_date, a.start_time::text, a.end_time::text,
			               a.day_of_week, a.recurring, a.notes
			        FROM activities a
			        LEFT JOIN family_members fm ON fm.id = a.family_member_id
			        WHERE true`
			args := []any{}
			n := 1

			if query != "" {
				sql += fmt.Sprintf(" AND (a.title ILIKE $%d OR a.notes ILIKE $%d)", n, n)
				args = append(args, "%"+query+"%")
				n++
			}
			if actType != "" {
				sql += fmt.Sprintf(" AND a.activity_type ILIKE $%d", n)
				args = append(args, "%"+actType+"%")
				n++
			}
			if memberID != "" {
				sql += fmt.Sprintf(" AND a.family_member_id = $%d::uuid", n)
				args = append(args, memberID)
				n++
			}
			sql += " ORDER BY a.start_date DESC, a.title"

			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var act activity
				if err := rows.Scan(&act.ID, &act.Title, &act.MemberName, &act.ActivityType,
					&act.Location, &act.StartDate, &act.EndDate, &act.StartTime, &act.EndTime,
					&act.DayOfWeek, &act.Recurring, &act.Notes); err != nil {
					return err
				}
				activities = append(activities, act)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(activities) == 0 {
			return brain.TextResult("No activities found."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d activit(ies):\n\n", len(activities))
		for _, act := range activities {
			who := "Family"
			if act.MemberName != nil {
				who = *act.MemberName
			}
			fmt.Fprintf(&sb, "• %s [%s] (id: %s)\n", act.Title, who, act.ID)
			if act.ActivityType != nil {
				fmt.Fprintf(&sb, "  Type: %s\n", *act.ActivityType)
			}
			if act.Recurring {
				fmt.Fprintf(&sb, "  Recurring: %s\n", safeStr(act.DayOfWeek))
			} else {
				fmt.Fprintf(&sb, "  Date: %s\n", act.StartDate.Format("2006-01-02"))
			}
			if act.Location != nil {
				fmt.Fprintf(&sb, "  Location: %s\n", *act.Location)
			}
			fmt.Fprintln(&sb)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func addDate(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title, _ := req.GetArguments()["title"].(string)
		if title == "" {
			return brain.ToolError("title is required"), nil
		}
		eventDate, _ := req.GetArguments()["event_date"].(string)
		if eventDate == "" {
			return brain.ToolError("event_date is required"), nil
		}
		memberID, _ := req.GetArguments()["family_member_id"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		recurringYearly := false
		if v, ok := req.GetArguments()["recurring_yearly"].(bool); ok {
			recurringYearly = v
		}

		reminderDays := 7
		if v, ok := req.GetArguments()["reminder_days_before"].(float64); ok && v > 0 {
			reminderDays = int(v)
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO important_dates (user_id, family_member_id, title, event_date, recurring_yearly, reminder_days_before, notes)
				VALUES (current_setting('app.current_user_id')::uuid, NULLIF($1, '')::uuid, $2, $3::date, $4, $5, $6)
				RETURNING id::text`,
				memberID, title, eventDate, recurringYearly, reminderDays, nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add date: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added important date: %s — %s (id: %s)", title, eventDate, id)}
		if recurringYearly {
			parts = append(parts, "Recurs yearly")
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func getUpcomingDates(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		days := 30
		if v, ok := req.GetArguments()["days"].(float64); ok && v > 0 {
			days = int(v)
		}

		var dates []importantDate
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// For recurring yearly dates, check if the date (adjusted to current year) falls in window.
			// For non-recurring, check raw event_date.
			rows, err := tx.Query(ctx, `
				SELECT d.id::text, d.title, fm.name, d.event_date, d.recurring_yearly,
				       d.reminder_days_before, d.notes
				FROM important_dates d
				LEFT JOIN family_members fm ON fm.id = d.family_member_id
				WHERE (
				    (d.recurring_yearly = false AND d.event_date BETWEEN CURRENT_DATE AND CURRENT_DATE + $1 * interval '1 day')
				    OR
				    (d.recurring_yearly = true AND
				        (make_date(EXTRACT(YEAR FROM CURRENT_DATE)::int,
				                   EXTRACT(MONTH FROM d.event_date)::int,
				                   EXTRACT(DAY FROM d.event_date)::int)
				         BETWEEN CURRENT_DATE AND CURRENT_DATE + $1 * interval '1 day'))
				)
				ORDER BY CASE
				    WHEN d.recurring_yearly THEN
				        make_date(EXTRACT(YEAR FROM CURRENT_DATE)::int,
				                  EXTRACT(MONTH FROM d.event_date)::int,
				                  EXTRACT(DAY FROM d.event_date)::int)
				    ELSE d.event_date
				END ASC`, days)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var d importantDate
				if err := rows.Scan(&d.ID, &d.Title, &d.MemberName, &d.EventDate,
					&d.RecurringYearly, &d.ReminderDays, &d.Notes); err != nil {
					return err
				}
				dates = append(dates, d)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(dates) == 0 {
			return brain.TextResult(fmt.Sprintf("No important dates in the next %d days.", days)), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d upcoming date(s):\n\n", len(dates))
		now := time.Now()
		for _, d := range dates {
			targetDate := d.EventDate
			if d.RecurringYearly {
				targetDate = time.Date(now.Year(), d.EventDate.Month(), d.EventDate.Day(), 0, 0, 0, 0, time.UTC)
			}
			daysUntil := int(targetDate.Sub(now).Hours()/24) + 1

			who := ""
			if d.MemberName != nil {
				who = fmt.Sprintf(" [%s]", *d.MemberName)
			}
			fmt.Fprintf(&sb, "• %s%s — %s (%d days away)\n", d.Title, who, targetDate.Format("2006-01-02"), daysUntil)
			if d.RecurringYearly {
				fmt.Fprintf(&sb, "  Recurs yearly\n")
			}
			if d.Notes != nil {
				fmt.Fprintf(&sb, "  Notes: %s\n", *d.Notes)
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

func safeStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
