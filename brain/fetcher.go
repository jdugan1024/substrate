// ABOUTME: Link metadata fetcher — HTML title/description extraction and background enrichment worker.

package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── HTML meta extraction ──────────────────────────────────────────────────────

var (
	reOGTitle     = regexp.MustCompile(`(?i)<meta[^>]+property=["']og:title["'][^>]+content=["']([^"'<>]+)["']`)
	reOGTitleAlt  = regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"'<>]+)["'][^>]+property=["']og:title["']`)
	reOGDesc      = regexp.MustCompile(`(?i)<meta[^>]+property=["']og:description["'][^>]+content=["']([^"'<>]+)["']`)
	reOGDescAlt   = regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"'<>]+)["'][^>]+property=["']og:description["']`)
	reMetaDesc    = regexp.MustCompile(`(?i)<meta[^>]+name=["']description["'][^>]+content=["']([^"'<>]+)["']`)
	reMetaDescAlt = regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"'<>]+)["'][^>]+name=["']description["']`)
	reTitleTag    = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)</title>`)
	reHTMLTag     = regexp.MustCompile(`<[^>]+>`)
	reSpaces      = regexp.MustCompile(`\s+`)
)

// FetchLinkMeta fetches the given URL and extracts title and description from
// HTML meta tags. Priority: og:title > <title>, og:description > <meta name="description">.
// Returns a non-nil error if the HTTP request fails or the response is not 2xx.
func FetchLinkMeta(ctx context.Context, rawURL string) (title, description string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Engram/1.0; +https://engram.x1024.net)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 512 KB is enough to capture <head> without loading large bodies.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", "", fmt.Errorf("read body: %w", err)
	}
	s := string(body)

	title = firstMatch(s, reOGTitle, reOGTitleAlt, reTitleTag)
	description = firstMatch(s, reOGDesc, reOGDescAlt, reMetaDesc, reMetaDescAlt)
	return title, description, nil
}

// firstMatch tries each regex in order, returning the first non-empty capture group.
func firstMatch(s string, patterns ...*regexp.Regexp) string {
	for _, re := range patterns {
		if m := re.FindStringSubmatch(s); len(m) > 1 {
			if v := strings.TrimSpace(m[1]); v != "" {
				return v
			}
		}
	}
	return ""
}

// ParseLinkText splits "url [optional notes]" into its two components.
// The URL is the first whitespace-delimited token; notes is everything after it.
func ParseLinkText(text string) (rawURL, notes string) {
	text = strings.TrimSpace(text)
	parts := strings.SplitN(text, " ", 2)
	rawURL = parts[0]
	if len(parts) > 1 {
		notes = strings.TrimSpace(parts[1])
	}
	return
}

// BuildLinkPayload constructs the JSON payload and content_text for a note.link entry.
//   - On success (fetchErr == nil): fetch_status="fetched", extract_status="pending",
//     content_text = "{title} — {description} ({url})" (falls back to just title or url).
//   - On failure (fetchErr != nil): fetch_status="pending", fetch_error=<reason>,
//     content_text = url (searchable immediately by URL).
func BuildLinkPayload(rawURL, title, description, notes string, fetchErr error) (payload json.RawMessage, contentText string) {
	m := map[string]any{"url": rawURL}

	if fetchErr != nil {
		m["fetch_status"] = "pending"
		m["fetch_error"] = fetchErr.Error()
		contentText = rawURL
	} else {
		m["fetch_status"] = "fetched"
		m["extract_status"] = "pending"
		if title != "" {
			m["title"] = title
		}
		if description != "" {
			m["description"] = description
		}
		switch {
		case title != "" && description != "":
			contentText = fmt.Sprintf("%s — %s (%s)", title, description, rawURL)
		case title != "":
			contentText = fmt.Sprintf("%s (%s)", title, rawURL)
		default:
			contentText = rawURL
		}
	}

	if notes != "" {
		m["notes"] = notes
	}

	b, _ := json.Marshal(m)
	return b, contentText
}

// ── Enrichment worker ─────────────────────────────────────────────────────────

// EnrichmentWorker retries pending link fetches and extracts full-text content
// for richer semantic embeddings. Start it via Run in a goroutine.
type EnrichmentWorker struct {
	app *App
}

// NewEnrichmentWorker creates an EnrichmentWorker backed by the given App.
func NewEnrichmentWorker(app *App) *EnrichmentWorker {
	return &EnrichmentWorker{app: app}
}

// Run starts the enrichment loop. It runs until ctx is cancelled.
// The interval is controlled by ENGRAM_ENRICHMENT_INTERVAL (default 10m).
func (w *EnrichmentWorker) Run(ctx context.Context) {
	interval := 10 * time.Minute
	if v := os.Getenv("ENGRAM_ENRICHMENT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	log.Printf("enrichment worker: starting (interval=%s)", interval)
	for {
		w.fetchPendingLinks(ctx)
		w.extractPendingLinks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

type linkEntry struct {
	id      string
	url     string
	notes   string
	payload map[string]any
}

// fetchPendingLinks retries note.link entries where fetch_status="pending".
func (w *EnrichmentWorker) fetchPendingLinks(ctx context.Context) {
	var entries []linkEntry
	err := w.app.WithAdminTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, payload
			FROM entries
			WHERE record_type = 'note.link'
			  AND payload->>'fetch_status' = 'pending'
			  AND deleted_at IS NULL
			LIMIT 50
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e linkEntry
			var raw []byte
			if err := rows.Scan(&e.id, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &e.payload); err != nil {
				log.Printf("enrichment: skip entry %s: unmarshal payload: %v", e.id, err)
				continue
			}
			e.url, _ = e.payload["url"].(string)
			e.notes, _ = e.payload["notes"].(string)
			if e.url != "" {
				entries = append(entries, e)
			}
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("enrichment: query pending links: %v", err)
		return
	}

	for _, e := range entries {
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		title, desc, fetchErr := FetchLinkMeta(fetchCtx, e.url)
		cancel()

		newPayload, contentText := BuildLinkPayload(e.url, title, desc, e.notes, fetchErr)

		// On persistent 4xx errors, mark as failed (no further retries).
		if fetchErr != nil && strings.HasPrefix(fetchErr.Error(), "HTTP 4") {
			var m map[string]any
			json.Unmarshal(newPayload, &m)
			m["fetch_status"] = "failed"
			newPayload, _ = json.Marshal(m)
		}

		emb, embErr := w.app.GetEmbedding(ctx, contentText)
		if embErr != nil {
			log.Printf("enrichment: embedding for %s: %v", e.id, embErr)
			continue
		}

		if err := w.app.WithAdminTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE entries
				SET payload = $2, content_text = $3, embedding = $4, updated_at = NOW()
				WHERE id = $1
			`, e.id, newPayload, contentText, &emb)
			return err
		}); err != nil {
			log.Printf("enrichment: update entry %s: %v", e.id, err)
		} else {
			log.Printf("enrichment: fetched link %s", e.id)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// extractPendingLinks fetches full-text for entries where fetch_status="fetched"
// and extract_status="pending", then regenerates their embeddings.
func (w *EnrichmentWorker) extractPendingLinks(ctx context.Context) {
	var entries []linkEntry
	err := w.app.WithAdminTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, payload->>'url'
			FROM entries
			WHERE record_type = 'note.link'
			  AND payload->>'fetch_status' = 'fetched'
			  AND payload->>'extract_status' = 'pending'
			  AND deleted_at IS NULL
			LIMIT 20
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e linkEntry
			if err := rows.Scan(&e.id, &e.url); err != nil {
				return err
			}
			if e.url != "" {
				entries = append(entries, e)
			}
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("enrichment: query extract pending: %v", err)
		return
	}

	for _, e := range entries {
		extractCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		text, extractErr := fetchFullText(extractCtx, e.url)
		cancel()

		var extractStatus, extractErrStr, embeddingText string
		if extractErr != nil || strings.TrimSpace(text) == "" {
			extractStatus = "failed"
			if extractErr != nil {
				extractErrStr = extractErr.Error()
			} else {
				extractErrStr = "no usable text extracted"
			}
			embeddingText = e.url
		} else {
			extractStatus = "extracted"
			if len(text) > 2000 {
				text = text[:2000]
			}
			embeddingText = text
		}

		emb, embErr := w.app.GetEmbedding(ctx, embeddingText)
		if embErr != nil {
			log.Printf("enrichment: extract embedding for %s: %v", e.id, embErr)
			continue
		}

		statusJSON, _ := json.Marshal(extractStatus)
		errJSON, _ := json.Marshal(extractErrStr)

		if err := w.app.WithAdminTx(ctx, func(tx pgx.Tx) error {
			if extractErrStr != "" {
				_, err := tx.Exec(ctx, `
					UPDATE entries
					SET payload = jsonb_set(jsonb_set(payload, '{extract_status}', $2), '{extract_error}', $3),
					    embedding = $4, updated_at = NOW()
					WHERE id = $1
				`, e.id, statusJSON, errJSON, &emb)
				return err
			}
			_, err := tx.Exec(ctx, `
				UPDATE entries
				SET payload = jsonb_set(payload, '{extract_status}', $2),
				    content_text = $3,
				    embedding = $4, updated_at = NOW()
				WHERE id = $1
			`, e.id, statusJSON, embeddingText, &emb)
			return err
		}); err != nil {
			log.Printf("enrichment: update extract %s: %v", e.id, err)
		} else {
			log.Printf("enrichment: extracted text for link %s (status=%s)", e.id, extractStatus)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// fetchFullText fetches a URL and returns its body with HTML tags stripped.
func fetchFullText(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Engram/1.0; +https://engram.x1024.net)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", err
	}

	text := reHTMLTag.ReplaceAllString(string(body), " ")
	text = reSpaces.ReplaceAllString(text, " ")
	return strings.TrimSpace(text), nil
}
