// ABOUTME: Shared infrastructure for the Open Brain MCP server.
// ABOUTME: Provides the App struct, DB transaction helper, OpenRouter clients, and MCP response helpers.

package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	pgvector "github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

// CtxUserID is the context key for the authenticated user's ID.
type contextKey string

const CtxUserID contextKey = "userID"

// ThoughtMetadata is the structured metadata extracted from a captured thought.
type ThoughtMetadata struct {
	People      []string `json:"people"`
	ActionItems []string `json:"action_items"`
	Dates       []string `json:"dates_mentioned"`
	Topics      []string `json:"topics"`
	Type        string   `json:"type"`
	Source      string   `json:"source,omitempty"`
}

// App holds shared dependencies available to all extensions.
type App struct {
	Pool          *pgxpool.Pool
	AdminPool     *pgxpool.Pool
	OpenRouterKey string
	OIDC          *OIDCVerifier
}

// adminPool returns the pool used for RLS-bypassing system transactions.
// It prefers a dedicated BYPASSRLS-capable pool (AdminPool) and falls back to
// the main Pool when none is configured, preserving prior behavior.
func (a *App) adminPool() *pgxpool.Pool {
	if a.AdminPool != nil {
		return a.AdminPool
	}
	return a.Pool
}

// New creates an App, connecting to Postgres with pgvector type registration.
func New(ctx context.Context, dbURL, openRouterKey string) (*App, error) {
	pool, err := newPool(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	return &App{Pool: pool, OpenRouterKey: openRouterKey}, nil
}

// ConnectAdminPool connects the dedicated, RLS-bypassing pool used by
// WithAdminTx (e.g. the EnrichmentWorker). The role behind adminURL must have
// the BYPASSRLS attribute. When unset, WithAdminTx falls back to the main pool.
func (a *App) ConnectAdminPool(ctx context.Context, adminURL string) error {
	pool, err := newPool(ctx, adminURL)
	if err != nil {
		return err
	}
	a.AdminPool = pool
	return nil
}

// newPool builds a pgx pool with pgvector type registration and verifies it.
func newPool(ctx context.Context, dbURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parse db config: %w", err)
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect to db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

// WithUserTx begins a transaction scoped to the authenticated user via SET LOCAL,
// which activates PostgreSQL RLS policies keyed on app.current_user_id.
func (a *App) WithUserTx(ctx context.Context, fn func(pgx.Tx) error) error {
	userID, ok := ctx.Value(CtxUserID).(string)
	if !ok || userID == "" {
		return fmt.Errorf("no authenticated user in request context")
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.current_user_id = '%s'", userID)); err != nil {
		return fmt.Errorf("set user context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithAdminTx begins a transaction that bypasses row-level security.
// Requires the database role to have the BYPASSRLS attribute or be the
// table owner. Used exclusively by the EnrichmentWorker.
func (a *App) WithAdminTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := a.adminPool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL row_security = off"); err != nil {
		return fmt.Errorf("disable row_security: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetEmbedding returns a vector embedding for text via OpenRouter.
func (a *App) GetEmbedding(ctx context.Context, text string) (pgvector.Vector, error) {
	body, _ := json.Marshal(map[string]string{
		"model": "openai/text-embedding-3-small",
		"input": text,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/embeddings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.OpenRouterKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pgvector.Vector{}, err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return pgvector.Vector{}, fmt.Errorf("embedding API error %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return pgvector.Vector{}, fmt.Errorf("embedding decode: %w (body: %s)", err, string(b))
	}
	if len(result.Data) == 0 {
		return pgvector.Vector{}, fmt.Errorf("no embeddings returned (body: %s)", string(b))
	}
	return pgvector.NewVector(result.Data[0].Embedding), nil
}

// ExtractMetadata uses an LLM to pull structured metadata from a thought.
func (a *App) ExtractMetadata(ctx context.Context, text string) (*ThoughtMetadata, error) {
	body, _ := json.Marshal(map[string]any{
		"model":           "openai/gpt-4o-mini",
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{
				"role": "system",
				"content": `Extract metadata from the user's captured thought. Return JSON with:
- "people": array of people mentioned (empty if none)
- "action_items": array of implied to-dos (empty if none)
- "dates_mentioned": array of dates YYYY-MM-DD (empty if none)
- "topics": array of 1-3 short topic tags (always at least one)
- "type": one of "observation", "task", "idea", "reference", "person_note"
Only extract what's explicitly there.`,
			},
			{"role": "user", "content": text},
		},
	})

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.OpenRouterKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Choices) == 0 {
		return &ThoughtMetadata{Topics: []string{"uncategorized"}, Type: "observation"}, nil
	}

	var meta ThoughtMetadata
	if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &meta); err != nil {
		return &ThoughtMetadata{Topics: []string{"uncategorized"}, Type: "observation"}, nil
	}
	return &meta, nil
}

// ToolError returns an MCP error result.
func ToolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.NewTextContent(msg)},
		IsError: true,
	}
}

// TextResult returns a successful MCP text result.
func TextResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.NewTextContent(msg)},
	}
}
