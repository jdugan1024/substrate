// ABOUTME: Repository functions for the canonical entries table.
// ABOUTME: All write and read operations against entries go through these typed functions.

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"
)

// Entry represents a row in the canonical entries table.
type Entry struct {
	ID            string
	UserID        string
	RecordType    string
	SchemaVersion string
	Source        string
	Confidence    *float64
	FailureMode   *string
	ContentText   string
	Payload       json.RawMessage
	Tags          []string
	Entities      json.RawMessage
	Embedding     *pgvector.Vector
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// InsertEntryParams holds the fields needed to write a new entry.
type InsertEntryParams struct {
	RecordType    string
	SchemaVersion string
	Source        string
	Confidence    *float64
	FailureMode   *string
	ContentText   string
	Payload       json.RawMessage
	Tags          []string
	Entities      json.RawMessage
	Embedding     *pgvector.Vector
}

// InsertEntry writes a new entry row inside an existing transaction.
// RLS is enforced via app.current_user_id set on the transaction.
func InsertEntry(ctx context.Context, tx pgx.Tx, p InsertEntryParams) (string, error) {
	if p.Tags == nil {
		p.Tags = []string{}
	}
	if p.Entities == nil {
		p.Entities = json.RawMessage("{}")
	}
	if p.Payload == nil {
		p.Payload = json.RawMessage("{}")
	}

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO entries (
			user_id, record_type, schema_version, source,
			confidence, failure_mode, content_text, payload,
			tags, entities, embedding
		) VALUES (
			current_setting('app.current_user_id')::uuid, $1, $2, $3,
			$4, $5, $6, $7,
			$8, $9, $10
		) RETURNING id::text`,
		p.RecordType, p.SchemaVersion, p.Source,
		p.Confidence, p.FailureMode, p.ContentText, p.Payload,
		p.Tags, p.Entities, p.Embedding,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert entry: %w", err)
	}
	return id, nil
}

// UpdateEntryContentParams holds fields for re-writing an existing entry's
// content (used to upsert a conversation summary as it is regenerated).
type UpdateEntryContentParams struct {
	EntryID     string
	ContentText string
	Payload     json.RawMessage
	Tags        []string
	Entities    json.RawMessage
	Embedding   *pgvector.Vector
}

// UpdateEntryContent rewrites an existing entry's content, payload, tags,
// entities, and embedding inside a transaction. RLS scopes the row to the
// current user. The entries_updated_at trigger refreshes updated_at.
func UpdateEntryContent(ctx context.Context, tx pgx.Tx, p UpdateEntryContentParams) error {
	if p.Tags == nil {
		p.Tags = []string{}
	}
	if p.Entities == nil {
		p.Entities = json.RawMessage("{}")
	}
	if p.Payload == nil {
		p.Payload = json.RawMessage("{}")
	}
	_, err := tx.Exec(ctx, `
		UPDATE entries
		SET content_text = $2, payload = $3, tags = $4, entities = $5, embedding = $6
		WHERE id = $1::uuid
	`, p.EntryID, p.ContentText, p.Payload, p.Tags, p.Entities, p.Embedding)
	if err != nil {
		return fmt.Errorf("update entry content: %w", err)
	}
	return nil
}

// UpdateSessionChunkTitles backfills the title onto all conversation.chunk
// entries for one captured session. Chunks are written with a cheap
// first-prompt title at capture time; once the summary produces a better
// title we propagate it so every entry for the session stays consistent.
// For chunks, payload mirrors entities, so both are updated. RLS scopes the
// rows to the current user. Returns the number of chunks updated.
func UpdateSessionChunkTitles(ctx context.Context, tx pgx.Tx, source, sessionID, title string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE entries
		SET entities = jsonb_set(entities, '{title}', to_jsonb($3::text)),
		    payload  = jsonb_set(payload,  '{title}', to_jsonb($3::text))
		WHERE record_type = 'conversation.chunk'
		  AND source = $1
		  AND entities->>'session_id' = $2
		  AND entities->>'title' IS DISTINCT FROM $3
	`, source, sessionID, title)
	if err != nil {
		return 0, fmt.Errorf("update session chunk titles: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SearchEntriesParams holds filters for cross-domain semantic search.
type SearchEntriesParams struct {
	Embedding   pgvector.Vector
	Threshold   float64
	Limit       int
	RecordTypes []string // empty = all types
}

// SearchResult is one result row from SearchEntries.
type SearchResult struct {
	ID          string
	RecordType  string
	ContentText string
	Payload     json.RawMessage
	Similarity  float64
	CreatedAt   time.Time
}

// SearchEntries performs a semantic search over entries using cosine similarity.
// Optionally filtered by record_type. Excludes soft-deleted rows.
func SearchEntries(ctx context.Context, tx pgx.Tx, p SearchEntriesParams) ([]SearchResult, error) {
	if p.Limit <= 0 {
		p.Limit = 10
	}
	if p.Threshold <= 0 {
		p.Threshold = 0.5
	}

	var (
		sql  string
		args []any
		n    = 1
	)

	sql = `
		SELECT id::text, record_type, content_text, payload,
		       1 - (embedding <=> $1) AS similarity, created_at
		FROM entries
		WHERE deleted_at IS NULL
		  AND 1 - (embedding <=> $1) > $2`
	args = append(args, p.Embedding, p.Threshold)
	n = 3

	if len(p.RecordTypes) > 0 {
		sql += fmt.Sprintf(" AND record_type = ANY($%d)", n)
		args = append(args, p.RecordTypes)
		n++
	}

	sql += fmt.Sprintf(" ORDER BY embedding <=> $1 LIMIT $%d", n)
	args = append(args, p.Limit)

	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search entries: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ID, &r.RecordType, &r.ContentText, &r.Payload, &r.Similarity, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}
