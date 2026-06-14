// ABOUTME: Repository functions for the captured_sessions live-capture tracking table.
// ABOUTME: RLS-scoped; all functions run inside a WithUserTx transaction.

package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CapturedSession is a row from captured_sessions.
type CapturedSession struct {
	Tool             string
	SessionID        string
	SummaryEntryID   *string
	ChunkedMsgCount  int
	MessageCount     int
	LastSummarizedAt *time.Time
}

// UpsertCapturedSessionParams holds the fields written on each ingest.
type UpsertCapturedSessionParams struct {
	Tool             string
	SessionID        string
	SummaryEntryID   *string
	ChunkedMsgCount  int
	MessageCount     int
	LastSummarizedAt *time.Time
	SessionEnded     bool
}

// GetCapturedSession returns the tracking row for (tool, session_id), or
// (nil, nil) if none exists. Must run inside a WithUserTx (RLS).
func GetCapturedSession(ctx context.Context, tx pgx.Tx, tool, sessionID string) (*CapturedSession, error) {
	var c CapturedSession
	err := tx.QueryRow(ctx, `
		SELECT tool, session_id, summary_entry_id::text, chunked_msg_count, message_count, last_summarized_at
		FROM captured_sessions
		WHERE tool = $1 AND session_id = $2
	`, tool, sessionID).Scan(&c.Tool, &c.SessionID, &c.SummaryEntryID, &c.ChunkedMsgCount, &c.MessageCount, &c.LastSummarizedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get captured session: %w", err)
	}
	return &c, nil
}

// UpsertCapturedSession inserts or updates the tracking row. Sets
// session_started_at on first insert and session_ended_at when SessionEnded.
// Must run inside a WithUserTx (RLS).
func UpsertCapturedSession(ctx context.Context, tx pgx.Tx, p UpsertCapturedSessionParams) error {
	var endedAt *time.Time
	if p.SessionEnded {
		now := time.Now()
		endedAt = &now
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO captured_sessions (
			user_id, tool, session_id, summary_entry_id,
			chunked_msg_count, message_count, last_summarized_at,
			session_started_at, session_ended_at, last_ingested_at
		) VALUES (
			current_setting('app.current_user_id')::uuid, $1, $2, $3::uuid,
			$4, $5, $6,
			now(), $7, now()
		)
		ON CONFLICT (user_id, tool, session_id) DO UPDATE SET
			summary_entry_id   = COALESCE(EXCLUDED.summary_entry_id, captured_sessions.summary_entry_id),
			chunked_msg_count  = EXCLUDED.chunked_msg_count,
			message_count      = EXCLUDED.message_count,
			last_summarized_at = COALESCE(EXCLUDED.last_summarized_at, captured_sessions.last_summarized_at),
			session_ended_at   = COALESCE(EXCLUDED.session_ended_at, captured_sessions.session_ended_at),
			last_ingested_at   = now()
	`, p.Tool, p.SessionID, p.SummaryEntryID,
		p.ChunkedMsgCount, p.MessageCount, p.LastSummarizedAt,
		endedAt)
	if err != nil {
		return fmt.Errorf("upsert captured session: %w", err)
	}
	return nil
}
