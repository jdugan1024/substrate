// ABOUTME: Web UI handlers for substrate.
// ABOUTME: Serves capture and browse UIs, and the GET /entries API endpoint.

package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"substrate/brain"
	"substrate/brain/service"
	"substrate/core"
)

//go:embed web/index.html
var webUI string

//go:embed web/browse.html
var browseUI string

//go:embed web/tokens.html
var tokensUI string

//go:embed web/shared.css
var sharedCSS string

// Pages, fully assembled once at startup: shared CSS and the active-mode nav
// are injected inline so each page is self-contained (no extra request, no CDN).
var (
	capturePage string
	browsePage  string
)

// navHTML returns the top app bar with the Capture/Browse mode switch, marking
// the given mode ("capture" or "browse") active.
func navHTML(active string) string {
	cap, brw := "", ""
	if active == "capture" {
		cap = " active"
	} else {
		brw = " active"
	}
	return `<header class="appbar">` +
		`<span class="wordmark">Substrate</span>` +
		`<nav class="modeswitch">` +
		`<a class="mode` + cap + `" href="/">Capture</a>` +
		`<a class="mode` + brw + `" href="/browse">Browse</a>` +
		`</nav></header>`
}

// buildPage inlines the shared CSS and active-mode nav into a page template by
// replacing the /*__SHARED_CSS__*/ and <!--__NAV__--> placeholders.
func buildPage(tmpl, activeMode string) string {
	p := strings.Replace(tmpl, "/*__SHARED_CSS__*/", sharedCSS, 1)
	return strings.Replace(p, "<!--__NAV__-->", navHTML(activeMode), 1)
}

func init() {
	capturePage = buildPage(webUI, "capture")
	browsePage = buildPage(browseUI, "browse")
}

// RegisterWebHandlers adds the web UI and capture endpoint to the mux.
func RegisterWebHandlers(mux *http.ServeMux, a *brain.App, es *service.EntryService, sessions *WebSessionStore) {
	mux.HandleFunc("/", serveWebUI())
	mux.Handle("POST /capture", webAuthMiddleware(sessions, http.HandlerFunc(webCaptureHandler(a, es))))
	mux.HandleFunc("GET /browse", serveBrowseUI())
	mux.Handle("GET /entries", webAuthMiddleware(sessions, http.HandlerFunc(listEntriesHandler(a))))
	mux.Handle("GET /entries/{id}", webAuthMiddleware(sessions, http.HandlerFunc(getEntryHandler(a))))
	mux.HandleFunc("GET /tokens.html", serveTokensUI())
	mux.Handle("POST /tokens", webAuthMiddleware(sessions, http.HandlerFunc(createTokenHandler(a))))
	mux.Handle("GET /tokens", webAuthMiddleware(sessions, http.HandlerFunc(listTokensHandler(a))))
	mux.Handle("DELETE /tokens/{id}", webAuthMiddleware(sessions, http.HandlerFunc(revokeTokenHandler(a))))
}

func serveWebUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, capturePage)
	}
}

func serveBrowseUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, browsePage)
	}
}

func serveTokensUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, tokensUI)
	}
}

type captureRequest struct {
	Text string `json:"text"`
}

type captureResponse struct {
	Tool    string `json:"tool"`
	Message string `json:"message"`
	// ID and LowConfidence let the web UI render friendly copy and a
	// "View in Browse" deep link without parsing the human Message string
	// (which is shared with the agent/MCP capture path).
	ID            string `json:"id"`
	LowConfidence bool   `json:"low_confidence"`
}

type entryItem struct {
	ID             string `json:"id"`
	RecordType     string `json:"record_type"`
	Title          string `json:"title"`
	ContentText    string `json:"content_text"`
	PayloadSummary string `json:"payload_summary"`
	URL            string `json:"url,omitempty"`
	CreatedAt      string `json:"created_at"`
	// MatchFields lists which fields matched the search query (title, summary,
	// topics, body), in display order. Omitted when there is no active query.
	MatchFields []string `json:"match_fields,omitempty"`
}

// titleField maps record types whose primary headline lives in a payload field
// other than "title" (e.g. a contact's name, not their job title).
var titleField = map[string]string{
	"crm.contact":      "name",
	"crm.interaction":  "person_name",
	"maintenance.task": "name",
}

// deriveTitle picks a human title for an entry: a type-specific headline field,
// then an explicit title in entities or payload, otherwise the first non-empty
// line of content_text.
func deriveTitle(recordType string, entities, payload json.RawMessage, contentText string) string {
	if field := titleField[recordType]; field != "" {
		var m map[string]any
		if json.Unmarshal(payload, &m) == nil {
			if v, ok := m[field].(string); ok && v != "" {
				return v
			}
		}
	}
	for _, raw := range []json.RawMessage{entities, payload} {
		var m struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(raw, &m) == nil && m.Title != "" {
			return m.Title
		}
	}
	for _, line := range strings.Split(contentText, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

type entriesResponse struct {
	Entries []entryItem `json:"entries"`
	HasMore bool        `json:"has_more"`
	// Counts is per-record_type result counts for the active query, ignoring the
	// selected type filter (so every filter chip can show a count). Only sent on
	// the first page (offset 0); nil on subsequent pages.
	Counts map[string]int `json:"counts,omitempty"`
}

// SQL expressions that approximate the displayed title/summary/topics for an
// entry, sourced directly from columns. These drive full-text ranking and
// per-field match attribution only — the strings shown to the user still come
// from deriveTitle / FormatPayloadSummary. jsonb array access is type-guarded
// so non-array payload fields don't error.
const (
	ftsTitleExpr = `coalesce(payload->>'title','')||' '||coalesce(payload->>'name','')||' '||` +
		`coalesce(payload->>'person_name','')||' '||coalesce(payload->>'company_name','')||' '||` +
		`split_part(content_text, E'\n', 1)`
	ftsSummaryExpr = `coalesce(payload->>'summary','')||' '||coalesce(payload->>'description','')||' '||` +
		`coalesce(payload->>'notes','')`
	ftsTopicsExpr = `coalesce((SELECT string_agg(t,' ') FROM jsonb_array_elements_text(` +
		`CASE WHEN jsonb_typeof(payload->'topics')='array' THEN payload->'topics' ELSE '[]'::jsonb END) t),'')||' '||` +
		`coalesce((SELECT string_agg(t,' ') FROM jsonb_array_elements_text(` +
		`CASE WHEN jsonb_typeof(tags)='array' THEN tags ELSE '[]'::jsonb END) t),'')`
)

// ftsFieldsCTE is the shared "m" CTE that computes the four per-field tsvectors
// for every visible entry. $1 must bind the websearch_to_tsquery text.
const ftsFieldsCTE = `WITH tsq AS (SELECT websearch_to_tsquery('english', $1) AS q),
m AS (
  SELECT id, record_type, content_text, payload, entities, created_at,
    to_tsvector('english', ` + ftsTitleExpr + `) AS tv_title,
    to_tsvector('english', ` + ftsSummaryExpr + `) AS tv_summary,
    to_tsvector('english', ` + ftsTopicsExpr + `) AS tv_topics,
    to_tsvector('english', content_text) AS tv_body
  FROM entries
  WHERE deleted_at IS NULL AND record_type <> 'conversation.chunk'
)`

func listEntriesHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		recordType := r.URL.Query().Get("type")
		limit := 50
		offset := 0
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
			limit = v
		}
		if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
			offset = v
		}

		fetchLimit := limit + 1
		var items []entryItem
		var counts map[string]int

		err := a.WithUserTx(r.Context(), func(tx pgx.Tx) error {
			listSQL, args := buildEntriesQuery(q, recordType, fetchLimit, offset)
			rows, err := tx.Query(r.Context(), listSQL, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item entryItem
				var payload, entities json.RawMessage
				var createdAt time.Time
				var mTitle, mSummary, mTopics, mBody bool
				if err := rows.Scan(&item.ID, &item.RecordType, &item.ContentText, &payload, &entities, &createdAt,
					&mTitle, &mSummary, &mTopics, &mBody); err != nil {
					return err
				}
				item.CreatedAt = createdAt.UTC().Format(time.RFC3339)
				item.Title = deriveTitle(item.RecordType, entities, payload, item.ContentText)
				item.PayloadSummary = core.FormatPayloadSummary(item.RecordType, payload)
				item.MatchFields = matchFields(mTitle, mSummary, mTopics, mBody)
				if item.RecordType == "note.link" {
					var p struct {
						URL string `json:"url"`
					}
					if json.Unmarshal(payload, &p) == nil {
						item.URL = p.URL
					}
				}
				items = append(items, item)
			}
			if err := rows.Err(); err != nil {
				return err
			}

			// Counts (per type, across the whole query) only on the first page.
			if offset == 0 {
				counts = map[string]int{}
				countSQL, countArgs := buildCountsQuery(q)
				crows, err := tx.Query(r.Context(), countSQL, countArgs...)
				if err != nil {
					return err
				}
				defer crows.Close()
				for crows.Next() {
					var rt string
					var c int
					if err := crows.Scan(&rt, &c); err != nil {
						return err
					}
					counts[rt] = c
				}
				return crows.Err()
			}
			return nil
		})
		if err != nil {
			log.Printf("list entries error: %v", err)
			http.Error(w, `{"error":"failed to fetch entries"}`, http.StatusInternalServerError)
			return
		}

		hasMore := len(items) > limit
		if hasMore {
			items = items[:limit]
		}
		if items == nil {
			items = []entryItem{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entriesResponse{Entries: items, HasMore: hasMore, Counts: counts})
	}
}

// matchFields returns the field keys that matched the query, in display order.
// Returns nil when nothing matched (e.g. no query), so the JSON field is omitted.
func matchFields(title, summary, topics, body bool) []string {
	var f []string
	if title {
		f = append(f, "title")
	}
	if summary {
		f = append(f, "summary")
	}
	if topics {
		f = append(f, "topics")
	}
	if body {
		f = append(f, "body")
	}
	return f
}

// buildEntriesQuery returns the list query and its args. With a query it runs
// full-text search (ranked by weighted relevance, then recency) and reports
// which fields matched; without one it falls back to recency order.
func buildEntriesQuery(q, recordType string, fetchLimit, offset int) (string, []any) {
	if q == "" {
		sql := `SELECT id::text, record_type, content_text, payload, entities, created_at,
			false, false, false, false
			FROM entries
			WHERE deleted_at IS NULL AND record_type <> 'conversation.chunk'`
		args := []any{}
		if recordType != "" {
			sql += " AND record_type = $1"
			args = append(args, recordType)
		}
		sql += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		args = append(args, fetchLimit, offset)
		return sql, args
	}

	args := []any{q}
	typeFilter := ""
	if recordType != "" {
		args = append(args, recordType)
		typeFilter = fmt.Sprintf(" AND record_type = $%d", len(args))
	}
	limitParam := len(args) + 1
	offsetParam := len(args) + 2
	args = append(args, fetchLimit, offset)

	sql := ftsFieldsCTE + `
		SELECT id::text, record_type, content_text, payload, entities, created_at,
		  tv_title   @@ (SELECT q FROM tsq),
		  tv_summary @@ (SELECT q FROM tsq),
		  tv_topics  @@ (SELECT q FROM tsq),
		  tv_body    @@ (SELECT q FROM tsq)
		FROM m
		WHERE (tv_title || tv_summary || tv_topics || tv_body) @@ (SELECT q FROM tsq)` + typeFilter + `
		ORDER BY ts_rank(
		    setweight(tv_title,'A') || setweight(tv_summary,'B') ||
		    setweight(tv_topics,'C') || setweight(tv_body,'D'),
		    (SELECT q FROM tsq)) DESC, created_at DESC
		LIMIT $` + strconv.Itoa(limitParam) + ` OFFSET $` + strconv.Itoa(offsetParam)
	return sql, args
}

// buildCountsQuery returns per-record_type counts for the active query, ignoring
// any selected type filter so every chip can show its count.
func buildCountsQuery(q string) (string, []any) {
	if q == "" {
		return `SELECT record_type, count(*) FROM entries
			WHERE deleted_at IS NULL AND record_type <> 'conversation.chunk'
			GROUP BY record_type`, nil
	}
	return ftsFieldsCTE + `
		SELECT record_type, count(*) FROM m
		WHERE (tv_title || tv_summary || tv_topics || tv_body) @@ (SELECT q FROM tsq)
		GROUP BY record_type`, []any{q}
}

// transcriptChunk is one ordered slice of a captured conversation, rendered as
// "role: text" lines by the ingest pipeline.
type transcriptChunk struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
}

// entryDetail is the full entry returned by GET /entries/{id}, including raw
// payload/entities and, for conversation summaries, the reconstructed transcript.
type entryDetail struct {
	ID          string            `json:"id"`
	RecordType  string            `json:"record_type"`
	Title       string            `json:"title"`
	ContentText string            `json:"content_text"`
	Payload     json.RawMessage   `json:"payload"`
	Entities    json.RawMessage   `json:"entities"`
	Tags        []string          `json:"tags"`
	CreatedAt   string            `json:"created_at"`
	Transcript  []transcriptChunk `json:"transcript,omitempty"`
}

func getEntryHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, `{"error":"id is required"}`, http.StatusBadRequest)
			return
		}

		var detail entryDetail
		var source string
		found := false
		err := a.WithUserTx(r.Context(), func(tx pgx.Tx) error {
			var createdAt time.Time
			var tags []string
			err := tx.QueryRow(r.Context(), `
				SELECT id::text, record_type, source, content_text, payload, entities, tags, created_at
				FROM entries
				WHERE id = $1::uuid AND deleted_at IS NULL`, id).Scan(
				&detail.ID, &detail.RecordType, &source, &detail.ContentText,
				&detail.Payload, &detail.Entities, &tags, &createdAt)
			if err == pgx.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			detail.Tags = tags
			detail.CreatedAt = createdAt.UTC().Format(time.RFC3339)
			detail.Title = deriveTitle(detail.RecordType, detail.Entities, detail.Payload, detail.ContentText)

			// For a conversation summary, attach the session's transcript chunks
			// (excluded from the feed) so the detail view can show the full chat.
			if detail.RecordType == "conversation.summary" {
				var sessionID string
				var ent struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(detail.Entities, &ent) == nil {
					sessionID = ent.SessionID
				}
				if sessionID != "" {
					rows, err := tx.Query(r.Context(), `
						SELECT content_text, COALESCE((entities->>'seq')::int, 0) AS seq
						FROM entries
						WHERE deleted_at IS NULL
						  AND record_type = 'conversation.chunk'
						  AND source = $1
						  AND entities->>'session_id' = $2
						ORDER BY seq`, source, sessionID)
					if err != nil {
						return err
					}
					defer rows.Close()
					for rows.Next() {
						var c transcriptChunk
						if err := rows.Scan(&c.Text, &c.Seq); err != nil {
							return err
						}
						detail.Transcript = append(detail.Transcript, c)
					}
					return rows.Err()
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("get entry error: %v", err)
			http.Error(w, `{"error":"failed to fetch entry"}`, http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(detail)
	}
}

func webCaptureHandler(a *brain.App, es *service.EntryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req captureRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
			http.Error(w, `{"error":"text is required"}`, http.StatusBadRequest)
			return
		}

		cr, err := es.Capture(r.Context(), req.Text, "web")
		if err != nil {
			log.Printf("entry capture error: %v", err)
			http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(captureResponse{
			Tool:          cr.RecordType,
			Message:       cr.Message,
			ID:            cr.EntryID,
			LowConfidence: cr.Fallback,
		})
	}
}
