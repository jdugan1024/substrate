// ABOUTME: Job Hunt Pipeline extension — complete job search management.
// ABOUTME: Adds tools for tracking companies, postings, applications, interviews, and CRM bridging.

package jobhunt

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

// Register adds job hunt pipeline tools to the MCP server.
func Register(s *server.MCPServer, a *brain.App) {
	s.AddTool(mcp.NewTool("add_company",
		mcp.WithDescription("Add a company you're tracking in your job search"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Company name")),
		mcp.WithString("industry", mcp.Description("Industry")),
		mcp.WithString("website", mcp.Description("Company website")),
		mcp.WithString("size", mcp.Description("Company size: startup, small, medium, large, enterprise")),
		mcp.WithString("location", mcp.Description("Location")),
		mcp.WithString("remote_policy", mcp.Description("Remote policy: remote, hybrid, onsite")),
		mcp.WithString("notes", mcp.Description("Notes about the company")),
		mcp.WithNumber("glassdoor_rating", mcp.Description("Glassdoor rating (1.0–5.0)")),
	), addCompany(a))

	s.AddTool(mcp.NewTool("add_job_posting",
		mcp.WithDescription("Add a job posting at a tracked company"),
		mcp.WithString("company_id", mcp.Required(), mcp.Description("Company ID")),
		mcp.WithString("title", mcp.Required(), mcp.Description("Job title")),
		mcp.WithString("url", mcp.Description("Posting URL")),
		mcp.WithNumber("salary_min", mcp.Description("Minimum salary")),
		mcp.WithNumber("salary_max", mcp.Description("Maximum salary")),
		mcp.WithString("requirements", mcp.Description("Job requirements")),
		mcp.WithString("nice_to_haves", mcp.Description("Nice-to-have qualifications")),
		mcp.WithString("source", mcp.Description("Source: linkedin, indeed, referral, company_site, other")),
		mcp.WithString("posted_date", mcp.Description("Date posted (YYYY-MM-DD)")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), addPosting(a))

	s.AddTool(mcp.NewTool("submit_application",
		mcp.WithDescription("Record a submitted job application"),
		mcp.WithString("job_posting_id", mcp.Required(), mcp.Description("Job posting ID")),
		mcp.WithString("status", mcp.Description("Status: applied, screening, interviewing, offer, accepted, rejected, withdrawn (default: applied)")),
		mcp.WithString("applied_date", mcp.Description("Date applied (YYYY-MM-DD, defaults to today)")),
		mcp.WithString("resume_version", mcp.Description("Resume version used")),
		mcp.WithString("cover_letter_notes", mcp.Description("Notes about cover letter")),
		mcp.WithString("referral_contact", mcp.Description("Referral contact name")),
		mcp.WithString("notes", mcp.Description("Additional notes")),
	), submitApplication(a))

	s.AddTool(mcp.NewTool("schedule_interview",
		mcp.WithDescription("Schedule an interview for an application"),
		mcp.WithString("application_id", mcp.Required(), mcp.Description("Application ID")),
		mcp.WithString("interview_type", mcp.Required(), mcp.Description("Type: phone_screen, technical, behavioral, panel, onsite, final")),
		mcp.WithString("scheduled_at", mcp.Required(), mcp.Description("Date/time (ISO 8601, e.g. 2026-03-20T14:00:00Z)")),
		mcp.WithNumber("duration_minutes", mcp.Description("Duration in minutes (default 60)")),
		mcp.WithString("interviewer_name", mcp.Description("Interviewer's name")),
		mcp.WithString("interviewer_title", mcp.Description("Interviewer's title")),
		mcp.WithString("notes", mcp.Description("Preparation notes")),
	), scheduleInterview(a))

	s.AddTool(mcp.NewTool("log_interview_notes",
		mcp.WithDescription("Add feedback and rating after an interview"),
		mcp.WithString("interview_id", mcp.Required(), mcp.Description("Interview ID")),
		mcp.WithString("feedback", mcp.Required(), mcp.Description("Interview feedback and notes")),
		mcp.WithNumber("rating", mcp.Description("Self-rating 1–5 (how well it went)")),
	), logInterviewNotes(a))

	s.AddTool(mcp.NewTool("get_pipeline_overview",
		mcp.WithDescription("Dashboard summary of your job search pipeline"),
		mcp.WithNumber("days", mcp.Description("Days ahead for upcoming interviews (default 7)")),
	), getPipelineOverview(a))

	s.AddTool(mcp.NewTool("get_upcoming_interviews",
		mcp.WithDescription("List upcoming interviews with full context"),
		mcp.WithNumber("days", mcp.Description("Days ahead to look (default 7)")),
	), getUpcomingInterviews(a))

	s.AddTool(mcp.NewTool("link_contact_to_professional_crm",
		mcp.WithDescription("Bridge a job search contact to your Professional CRM (Extension 5)"),
		mcp.WithString("job_contact_id", mcp.Required(), mcp.Description("Job contact ID to link")),
	), linkToCRM(a))
}

func addCompany(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, _ := req.GetArguments()["name"].(string)
		if name == "" {
			return brain.ToolError("name is required"), nil
		}
		industry, _ := req.GetArguments()["industry"].(string)
		website, _ := req.GetArguments()["website"].(string)
		size, _ := req.GetArguments()["size"].(string)
		location, _ := req.GetArguments()["location"].(string)
		remotePolicy, _ := req.GetArguments()["remote_policy"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		var glassdoorRating *float64
		if v, ok := req.GetArguments()["glassdoor_rating"].(float64); ok && v >= 1 && v <= 5 {
			glassdoorRating = &v
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO job_companies (user_id, name, industry, website, size, location, remote_policy, notes, glassdoor_rating)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5, $6, $7, $8)
				RETURNING id::text`,
				name, nullOrStr(industry), nullOrStr(website), nullOrStr(size),
				nullOrStr(location), nullOrStr(remotePolicy), nullOrStr(notes), glassdoorRating,
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add company: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added company: %s (id: %s)", name, id)}
		if industry != "" {
			parts = append(parts, "Industry: "+industry)
		}
		if remotePolicy != "" {
			parts = append(parts, "Remote: "+remotePolicy)
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func addPosting(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		companyID, _ := req.GetArguments()["company_id"].(string)
		title, _ := req.GetArguments()["title"].(string)
		if companyID == "" || title == "" {
			return brain.ToolError("company_id and title are required"), nil
		}
		url, _ := req.GetArguments()["url"].(string)
		requirements, _ := req.GetArguments()["requirements"].(string)
		niceToHaves, _ := req.GetArguments()["nice_to_haves"].(string)
		source, _ := req.GetArguments()["source"].(string)
		postedDate, _ := req.GetArguments()["posted_date"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		var salaryMin, salaryMax *int
		if v, ok := req.GetArguments()["salary_min"].(float64); ok {
			i := int(v)
			salaryMin = &i
		}
		if v, ok := req.GetArguments()["salary_max"].(float64); ok {
			i := int(v)
			salaryMax = &i
		}

		var id, companyName string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Get company name.
			err := tx.QueryRow(ctx, "SELECT name FROM job_companies WHERE id = $1", companyID).Scan(&companyName)
			if err != nil {
				return fmt.Errorf("company not found: %w", err)
			}

			return tx.QueryRow(ctx, `
				INSERT INTO job_postings (user_id, company_id, title, url, salary_min, salary_max, requirements, nice_to_haves, source, posted_date, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5, $6, $7, $8, $9::date, $10)
				RETURNING id::text`,
				companyID, title, nullOrStr(url), salaryMin, salaryMax,
				nullOrStr(requirements), nullOrStr(niceToHaves), nullOrStr(source),
				nullOrStr(postedDate), nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to add posting: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Added posting: %s at %s (id: %s)", title, companyName, id)}
		if salaryMin != nil && salaryMax != nil {
			parts = append(parts, fmt.Sprintf("Salary: $%dk–$%dk", *salaryMin/1000, *salaryMax/1000))
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func submitApplication(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		postingID, _ := req.GetArguments()["job_posting_id"].(string)
		if postingID == "" {
			return brain.ToolError("job_posting_id is required"), nil
		}
		status, _ := req.GetArguments()["status"].(string)
		if status == "" {
			status = "applied"
		}
		appliedDate, _ := req.GetArguments()["applied_date"].(string)
		if appliedDate == "" {
			appliedDate = time.Now().Format("2006-01-02")
		}
		resumeVersion, _ := req.GetArguments()["resume_version"].(string)
		coverLetterNotes, _ := req.GetArguments()["cover_letter_notes"].(string)
		referralContact, _ := req.GetArguments()["referral_contact"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		var id, jobTitle, companyName string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Get posting + company info.
			err := tx.QueryRow(ctx, `
				SELECT jp.title, jc.name
				FROM job_postings jp
				JOIN job_companies jc ON jc.id = jp.company_id
				WHERE jp.id = $1`, postingID,
			).Scan(&jobTitle, &companyName)
			if err != nil {
				return fmt.Errorf("posting not found: %w", err)
			}

			return tx.QueryRow(ctx, `
				INSERT INTO job_applications (user_id, job_posting_id, status, applied_date, resume_version, cover_letter_notes, referral_contact, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3::date, $4, $5, $6, $7)
				RETURNING id::text`,
				postingID, status, appliedDate, nullOrStr(resumeVersion),
				nullOrStr(coverLetterNotes), nullOrStr(referralContact), nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to submit application: " + err.Error()), nil
		}

		return brain.TextResult(fmt.Sprintf("Application submitted: %s at %s (id: %s)\nStatus: %s\nApplied: %s",
			jobTitle, companyName, id, status, appliedDate)), nil
	}
}

func scheduleInterview(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		applicationID, _ := req.GetArguments()["application_id"].(string)
		interviewType, _ := req.GetArguments()["interview_type"].(string)
		scheduledAt, _ := req.GetArguments()["scheduled_at"].(string)
		if applicationID == "" || interviewType == "" || scheduledAt == "" {
			return brain.ToolError("application_id, interview_type, and scheduled_at are required"), nil
		}

		interviewerName, _ := req.GetArguments()["interviewer_name"].(string)
		interviewerTitle, _ := req.GetArguments()["interviewer_title"].(string)
		notes, _ := req.GetArguments()["notes"].(string)

		duration := 60
		if v, ok := req.GetArguments()["duration_minutes"].(float64); ok && v > 0 {
			duration = int(v)
		}

		var id string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO job_interviews (user_id, application_id, interview_type, scheduled_at, duration_minutes, interviewer_name, interviewer_title, notes)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3::timestamptz, $4, $5, $6, $7)
				RETURNING id::text`,
				applicationID, interviewType, scheduledAt, duration,
				nullOrStr(interviewerName), nullOrStr(interviewerTitle), nullOrStr(notes),
			).Scan(&id)
		})
		if err != nil {
			return brain.ToolError("Failed to schedule interview: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Scheduled %s interview (id: %s)", interviewType, id)}
		parts = append(parts, "When: "+scheduledAt)
		parts = append(parts, fmt.Sprintf("Duration: %d min", duration))
		if interviewerName != "" {
			parts = append(parts, "Interviewer: "+interviewerName)
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func logInterviewNotes(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		interviewID, _ := req.GetArguments()["interview_id"].(string)
		feedback, _ := req.GetArguments()["feedback"].(string)
		if interviewID == "" || feedback == "" {
			return brain.ToolError("interview_id and feedback are required"), nil
		}

		var rating *int
		if v, ok := req.GetArguments()["rating"].(float64); ok && v >= 1 && v <= 5 {
			r := int(v)
			rating = &r
		}

		var interviewType string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				UPDATE job_interviews
				SET feedback = $1, rating = $2, status = 'completed'
				WHERE id = $3
				RETURNING interview_type`,
				feedback, rating, interviewID,
			).Scan(&interviewType)
		})
		if err != nil {
			return brain.ToolError("Failed to log notes: " + err.Error()), nil
		}

		parts := []string{fmt.Sprintf("Logged notes for %s interview", interviewType)}
		if rating != nil {
			parts = append(parts, fmt.Sprintf("Rating: %d/5", *rating))
		}
		return brain.TextResult(strings.Join(parts, "\n")), nil
	}
}

func getPipelineOverview(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		days := 7
		if v, ok := req.GetArguments()["days"].(float64); ok && v > 0 {
			days = int(v)
		}

		type statusCount struct {
			Status string
			Count  int
		}

		var statusCounts []statusCount
		var upcomingCount int
		var recentApps []struct {
			Title       string
			Company     string
			Status      string
			AppliedDate time.Time
		}

		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Application counts by status.
			rows, err := tx.Query(ctx, `
				SELECT status, COUNT(*) FROM job_applications GROUP BY status ORDER BY COUNT(*) DESC`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var sc statusCount
				if err := rows.Scan(&sc.Status, &sc.Count); err != nil {
					return err
				}
				statusCounts = append(statusCounts, sc)
			}
			if err := rows.Err(); err != nil {
				return err
			}

			// Upcoming interviews count.
			err = tx.QueryRow(ctx, `
				SELECT COUNT(*) FROM job_interviews
				WHERE status = 'scheduled' AND scheduled_at BETWEEN now() AND now() + $1 * interval '1 day'`,
				days).Scan(&upcomingCount)
			if err != nil {
				return err
			}

			// Recent applications.
			rows2, err := tx.Query(ctx, `
				SELECT jp.title, jc.name, ja.status, ja.applied_date
				FROM job_applications ja
				JOIN job_postings jp ON jp.id = ja.job_posting_id
				JOIN job_companies jc ON jc.id = jp.company_id
				ORDER BY ja.applied_date DESC
				LIMIT 5`)
			if err != nil {
				return err
			}
			defer rows2.Close()
			for rows2.Next() {
				var app struct {
					Title       string
					Company     string
					Status      string
					AppliedDate time.Time
				}
				if err := rows2.Scan(&app.Title, &app.Company, &app.Status, &app.AppliedDate); err != nil {
					return err
				}
				recentApps = append(recentApps, app)
			}
			return rows2.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		var sb strings.Builder
		sb.WriteString("=== Pipeline Overview ===\n\n")

		if len(statusCounts) == 0 {
			sb.WriteString("No applications yet.\n")
		} else {
			sb.WriteString("Applications by status:\n")
			total := 0
			for _, sc := range statusCounts {
				fmt.Fprintf(&sb, "  %s: %d\n", sc.Status, sc.Count)
				total += sc.Count
			}
			fmt.Fprintf(&sb, "  Total: %d\n", total)
		}

		fmt.Fprintf(&sb, "\nUpcoming interviews (next %d days): %d\n", days, upcomingCount)

		if len(recentApps) > 0 {
			sb.WriteString("\nRecent applications:\n")
			for _, app := range recentApps {
				fmt.Fprintf(&sb, "  • %s at %s [%s] — %s\n",
					app.Title, app.Company, app.Status, app.AppliedDate.Format("2006-01-02"))
			}
		}

		return brain.TextResult(sb.String()), nil
	}
}

func getUpcomingInterviews(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		days := 7
		if v, ok := req.GetArguments()["days"].(float64); ok && v > 0 {
			days = int(v)
		}

		type interview struct {
			ID               string
			InterviewType    string
			ScheduledAt      time.Time
			Duration         int
			InterviewerName  *string
			InterviewerTitle *string
			Notes            *string
			JobTitle         string
			CompanyName      string
		}

		var interviews []interview
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT ji.id::text, ji.interview_type, ji.scheduled_at, ji.duration_minutes,
				       ji.interviewer_name, ji.interviewer_title, ji.notes,
				       jp.title, jc.name
				FROM job_interviews ji
				JOIN job_applications ja ON ja.id = ji.application_id
				JOIN job_postings jp ON jp.id = ja.job_posting_id
				JOIN job_companies jc ON jc.id = jp.company_id
				WHERE ji.status = 'scheduled'
				  AND ji.scheduled_at BETWEEN now() AND now() + $1 * interval '1 day'
				ORDER BY ji.scheduled_at ASC`, days)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var i interview
				if err := rows.Scan(&i.ID, &i.InterviewType, &i.ScheduledAt, &i.Duration,
					&i.InterviewerName, &i.InterviewerTitle, &i.Notes,
					&i.JobTitle, &i.CompanyName); err != nil {
					return err
				}
				interviews = append(interviews, i)
			}
			return rows.Err()
		})
		if err != nil {
			return brain.ToolError("Error: " + err.Error()), nil
		}

		if len(interviews) == 0 {
			return brain.TextResult(fmt.Sprintf("No interviews scheduled in the next %d days.", days)), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d interview(s) in the next %d days:\n\n", len(interviews), days)
		for _, i := range interviews {
			fmt.Fprintf(&sb, "• %s — %s at %s\n", i.ScheduledAt.Format("2006-01-02 15:04"), i.InterviewType, i.CompanyName)
			fmt.Fprintf(&sb, "  Role: %s\n", i.JobTitle)
			fmt.Fprintf(&sb, "  Duration: %d min\n", i.Duration)
			if i.InterviewerName != nil {
				interviewer := *i.InterviewerName
				if i.InterviewerTitle != nil {
					interviewer += " (" + *i.InterviewerTitle + ")"
				}
				fmt.Fprintf(&sb, "  Interviewer: %s\n", interviewer)
			}
			if i.Notes != nil {
				fmt.Fprintf(&sb, "  Notes: %s\n", *i.Notes)
			}
			fmt.Fprintln(&sb)
		}
		return brain.TextResult(sb.String()), nil
	}
}

func linkToCRM(a *brain.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		jobContactID, _ := req.GetArguments()["job_contact_id"].(string)
		if jobContactID == "" {
			return brain.ToolError("job_contact_id is required"), nil
		}

		var crmContactID, contactName string
		err := a.WithUserTx(ctx, func(tx pgx.Tx) error {
			// Get the job contact details.
			var name string
			var title, email, phone, notes *string
			var companyID *string
			err := tx.QueryRow(ctx, `
				SELECT name, title, email, phone, notes, company_id::text
				FROM job_contacts WHERE id = $1`, jobContactID,
			).Scan(&name, &title, &email, &phone, &notes, &companyID)
			if err != nil {
				return fmt.Errorf("job contact not found: %w", err)
			}
			contactName = name

			// Get company name if available.
			var companyName *string
			if companyID != nil {
				var cn string
				err := tx.QueryRow(ctx, "SELECT name FROM job_companies WHERE id = $1", *companyID).Scan(&cn)
				if err == nil {
					companyName = &cn
				}
			}

			// Create CRM contact.
			howWeMet := "Job search"
			err = tx.QueryRow(ctx, `
				INSERT INTO professional_contacts (user_id, name, company, title, email, phone, how_we_met, notes, tags)
				VALUES (current_setting('app.current_user_id')::uuid, $1, $2, $3, $4, $5, $6, $7, ARRAY['job-search'])
				RETURNING id::text`,
				name, companyName, title, email, phone, howWeMet, notes,
			).Scan(&crmContactID)
			if err != nil {
				return err
			}

			// Link back.
			_, err = tx.Exec(ctx,
				"UPDATE job_contacts SET professional_crm_contact_id = $1 WHERE id = $2",
				crmContactID, jobContactID)
			return err
		})
		if err != nil {
			return brain.ToolError("Failed to link contact: " + err.Error()), nil
		}

		return brain.TextResult(fmt.Sprintf("Linked %s to Professional CRM (crm id: %s)", contactName, crmContactID)), nil
	}
}

func nullOrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
