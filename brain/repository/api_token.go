// ABOUTME: Repository functions for the api_tokens table (personal access tokens).
// ABOUTME: api_tokens has no RLS, so user-scoped queries filter by user_id explicitly.

package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// APIToken is a row from api_tokens (never exposes the plaintext token).
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// InsertAPIToken records a new token hash for a user and returns its id.
func InsertAPIToken(ctx context.Context, pool *pgxpool.Pool, userID, name, tokenHash string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO api_tokens (user_id, name, token_hash)
		VALUES ($1::uuid, $2, $3)
		RETURNING id::text
	`, userID, name, tokenHash).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert api token: %w", err)
	}
	return id, nil
}

// GetUserIDByTokenHash returns the owning user's id for a live (non-revoked)
// token hash. Returns ("", nil) if no matching live token exists.
func GetUserIDByTokenHash(ctx context.Context, pool *pgxpool.Pool, tokenHash string) (string, error) {
	var userID string
	err := pool.QueryRow(ctx, `
		SELECT user_id::text FROM api_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash).Scan(&userID)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get user by token hash: %w", err)
	}
	return userID, nil
}

// TouchAPIToken updates last_used_at to now for a token hash (best-effort).
// The write is conditional: it is skipped when last_used_at was set within the
// last 5 minutes, so the hot auth path does not write on every request.
func TouchAPIToken(ctx context.Context, pool *pgxpool.Pool, tokenHash string) error {
	_, err := pool.Exec(ctx, `
		UPDATE api_tokens SET last_used_at = now()
		WHERE token_hash = $1
		  AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes')
	`, tokenHash)
	if err != nil {
		return fmt.Errorf("touch api token: %w", err)
	}
	return nil
}

// ListAPITokens returns a user's live (non-revoked) tokens, newest first.
func ListAPITokens(ctx context.Context, pool *pgxpool.Pool, userID string) ([]APIToken, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, name, created_at, last_used_at
		FROM api_tokens
		WHERE user_id = $1::uuid AND revoked_at IS NULL
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	var tokens []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan api token: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// RevokeAPIToken marks a user's token revoked. Filters by user_id so a user
// cannot revoke another user's token.
func RevokeAPIToken(ctx context.Context, pool *pgxpool.Pool, userID, id string) error {
	_, err := pool.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		WHERE id = $1::uuid AND user_id = $2::uuid AND revoked_at IS NULL
	`, id, userID)
	if err != nil {
		return fmt.Errorf("revoke api token: %w", err)
	}
	return nil
}
