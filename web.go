// ABOUTME: Web UI handlers for engram.
// ABOUTME: Serves capture and browse UIs, and the GET /entries API endpoint.

package main

import (
	"encoding/json"
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"open-brain-go/brain"
	"open-brain-go/brain/service"
	"open-brain-go/core"
)

//go:embed web/index.html
var webUI string

//go:embed web/browse.html
var browseUI string

// RegisterWebHandlers adds the web UI and capture endpoint to the mux.
func RegisterWebHandlers(mux *http.ServeMux, a *brain.App, es *service.EntryService, sessions *WebSessionStore) {
	mux.HandleFunc("/", serveWebUI())
	mux.Handle("POST /capture", webAuthMiddleware(sessions, http.HandlerFunc(webCaptureHandler(a, es))))
	mux.HandleFunc("GET /browse", serveBrowseUI())
	mux.Handle("GET /entries", webAuthMiddleware(sessions, http.HandlerFunc(listEntriesHandler(a))))
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
		fmt.Fprint(w, webUI)
	}
}

func serveBrowseUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, browseUI)
	}
}

type captureRequest struct {
	Text string `json:"text"`
}

type captureResponse struct {
	Tool    string `json:"tool"`
	Message string `json:"message"`
}

type entryItem struct {
	ID             string `json:"id"`
	RecordType     string `json:"record_type"`
	ContentText    string `json:"content_text"`
	PayloadSummary string `json:"payload_summary"`
	URL            string `json:"url,omitempty"`
	CreatedAt      string `json:"created_at"`
}

type entriesResponse struct {
	Entries []entryItem `json:"entries"`
	HasMore bool        `json:"has_more"`
}

func listEntriesHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
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

		err := a.WithUserTx(r.Context(), func(tx pgx.Tx) error {
			qSQL := `SELECT id::text, record_type, content_text, payload, created_at
				FROM entries
				WHERE deleted_at IS NULL`
			args := []any{}
			n := 1

			if q != "" {
				qSQL += fmt.Sprintf(" AND content_text ILIKE $%d", n)
				args = append(args, "%"+q+"%")
				n++
			}
			if recordType != "" {
				qSQL += fmt.Sprintf(" AND record_type = $%d", n)
				args = append(args, recordType)
				n++
			}
			qSQL += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", n, n+1)
			args = append(args, fetchLimit, offset)

			rows, err := tx.Query(r.Context(), qSQL, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item entryItem
				var payload json.RawMessage
				var createdAt time.Time
				if err := rows.Scan(&item.ID, &item.RecordType, &item.ContentText, &payload, &createdAt); err != nil {
					return err
				}
				item.CreatedAt = createdAt.UTC().Format(time.RFC3339)
				item.PayloadSummary = core.FormatPayloadSummary(item.RecordType, payload)
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
			return rows.Err()
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
		json.NewEncoder(w).Encode(entriesResponse{Entries: items, HasMore: hasMore})
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
		json.NewEncoder(w).Encode(captureResponse{Tool: cr.RecordType, Message: cr.Message})
	}
}
