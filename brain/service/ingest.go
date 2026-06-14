// ABOUTME: IngestService — stores live-captured conversation transcripts as raw
// ABOUTME: chunk entries plus one upserted per-session distilled summary entry.

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"

	"open-brain-go/brain"
	"open-brain-go/brain/repository"
)

// Tunables (configurable later; constants for v1).
const (
	chunkBudgetTokens = 1500
	summaryMinNewMsgs = 6
	summaryMaxAge     = 5 * time.Minute
	openRouterBase    = "https://openrouter.ai/api/v1"

	recordTypeChunk   = "conversation.chunk"
	recordTypeSummary = "conversation.summary"
)

// pgvectorVector aliases the embedding vector type for brevity.
type pgvectorVector = pgvector.Vector

// IngestMessage is one normalized message in a transcript batch.
type IngestMessage struct {
	Role string `json:"role"` // "human" | "assistant"
	Text string `json:"text"`
	// Ts and MsgID are accepted from the wire but not persisted server-side;
	// dedup uses captured_sessions.chunked_msg_count, not message ids. Reserved
	// for the Part 2 daemon and possible future delta-shipping.
	Ts    string `json:"ts"`
	MsgID string `json:"msg_id"`
}

// IngestBatch is the full (trimmed) transcript for one session as sent by the
// capture daemon. Messages SHOULD be the complete transcript in order.
// Project is accepted but not yet persisted server-side (reserved).
type IngestBatch struct {
	Tool         string          `json:"tool"`
	SessionID    string          `json:"session_id"`
	Title        string          `json:"title"`
	Project      string          `json:"project"`
	Messages     []IngestMessage `json:"messages"`
	SessionEnded bool            `json:"session_ended"`
}

// IngestResult summarizes what an ingest produced.
type IngestResult struct {
	ChunksCreated int  `json:"chunks_created"`
	Summarized    bool `json:"summarized"`
	MessageCount  int  `json:"message_count"`
}

// estimateTokens approximates token count as ceil(chars/4).
func estimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}

// packChunks groups messages into chunks whose estimated token totals stay near
// budgetTokens. A trailing partial chunk is returned as remainder (held for a
// later sweep) unless sessionEnded, in which case it is flushed into chunks.
func packChunks(msgs []IngestMessage, budgetTokens int, sessionEnded bool) (chunks [][]IngestMessage, remainder []IngestMessage) {
	var cur []IngestMessage
	curTokens := 0
	for _, m := range msgs {
		t := estimateTokens(m.Text)
		if curTokens+t > budgetTokens && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur = nil
			curTokens = 0
		}
		cur = append(cur, m)
		curTokens += t
	}
	if len(cur) > 0 {
		if sessionEnded {
			chunks = append(chunks, cur)
		} else {
			remainder = cur
		}
	}
	return chunks, remainder
}

// shouldSummarize decides whether to regenerate the per-session summary.
func shouldSummarize(newMsgCount int, lastSummarizedAt *time.Time, now time.Time, sessionEnded bool, minNewMsgs int, maxAge time.Duration) bool {
	if sessionEnded {
		return true
	}
	if newMsgCount == 0 {
		return false
	}
	if lastSummarizedAt == nil {
		return true
	}
	if newMsgCount >= minNewMsgs {
		return true
	}
	return now.Sub(*lastSummarizedAt) >= maxAge
}

// renderTranscript formats messages as "role: text" lines.
func renderTranscript(msgs []IngestMessage) string {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Text)
	}
	return b.String()
}

// ConversationSummary is the structured distillation of a session.
type ConversationSummary struct {
	Summary     string   `json:"summary"`
	Topics      []string `json:"topics"`
	Decisions   []string `json:"decisions"`
	Preferences []string `json:"preferences"`
	OpenThreads []string `json:"open_threads"`
}

// generateConversationSummary calls OpenRouter chat completions to distill a
// transcript into a structured summary. baseURL is injectable for tests.
func generateConversationSummary(ctx context.Context, client *http.Client, baseURL, key, fullText string) (ConversationSummary, error) {
	if len(fullText) > 24000 {
		fullText = fullText[:24000]
	}
	body, _ := json.Marshal(map[string]any{
		"model":           "openai/gpt-4o-mini",
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{
				"role": "system",
				"content": `Summarize this LLM coding/chat session. Return JSON with:
- "summary": 2-4 sentence prose summary of what happened
- "topics": array of 1-5 short topic tags
- "decisions": array of decisions or conclusions reached (empty if none)
- "preferences": array of preferences the user expressed (empty if none)
- "open_threads": array of unresolved questions or TODOs (empty if none)`,
			},
			{"role": "user", "content": fullText},
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return ConversationSummary{}, fmt.Errorf("build summary request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return ConversationSummary{}, fmt.Errorf("summary request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return ConversationSummary{}, fmt.Errorf("summary API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return ConversationSummary{}, fmt.Errorf("decode summary response: %w (body: %s)", err, string(respBody))
	}
	if len(result.Choices) == 0 {
		return ConversationSummary{}, fmt.Errorf("empty choices in summary response")
	}

	var cs ConversationSummary
	if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &cs); err != nil {
		return ConversationSummary{}, fmt.Errorf("parse summary json: %w", err)
	}
	return cs, nil
}

// IngestService stores live-captured conversation transcripts.
type IngestService struct {
	app *brain.App
}

// NewIngestService creates an IngestService backed by the given App.
func NewIngestService(app *brain.App) *IngestService {
	return &IngestService{app: app}
}

// Ingest stores a transcript batch: it emits new raw chunk entries and, when
// the throttle allows, upserts the per-session distilled summary entry.
// The batch SHOULD contain the full trimmed transcript for the session.
func (s *IngestService) Ingest(ctx context.Context, batch IngestBatch) (IngestResult, error) {
	if batch.Tool == "" || batch.SessionID == "" {
		return IngestResult{}, fmt.Errorf("tool and session_id are required")
	}

	// 1. Read existing tracking row (RLS tx).
	var existing *repository.CapturedSession
	if err := s.app.WithUserTx(ctx, func(tx pgx.Tx) error {
		c, err := repository.GetCapturedSession(ctx, tx, batch.Tool, batch.SessionID)
		existing = c
		return err
	}); err != nil {
		return IngestResult{}, fmt.Errorf("read captured session: %w", err)
	}

	chunkedCount := 0
	var lastSummarizedAt *time.Time
	var summaryEntryID *string
	if existing != nil {
		chunkedCount = existing.ChunkedMsgCount
		lastSummarizedAt = existing.LastSummarizedAt
		summaryEntryID = existing.SummaryEntryID
	}

	// 2. Determine new (not-yet-chunked) messages and pack them.
	// The batch must be append-only: a transcript shorter than what we have
	// already chunked means the daemon violated the contract (e.g. trimmed from
	// the front), which would misalign the index — reject rather than corrupt.
	if len(batch.Messages) < chunkedCount {
		return IngestResult{}, fmt.Errorf("non-append-only batch for tool=%s session=%s: %d messages < %d already chunked",
			batch.Tool, batch.SessionID, len(batch.Messages), chunkedCount)
	}
	newMsgs := []IngestMessage{}
	if chunkedCount < len(batch.Messages) {
		newMsgs = batch.Messages[chunkedCount:]
	}
	chunks, remainder := packChunks(newMsgs, chunkBudgetTokens, batch.SessionEnded)

	// 3. Embed each chunk (network, outside tx). seq = index of first message.
	type chunkInsert struct {
		text     string
		seq      int
		embed    pgvectorVector
		entities json.RawMessage
	}
	var toInsert []chunkInsert
	seq := chunkedCount
	for _, c := range chunks {
		text := renderTranscript(c)
		emb, err := s.app.GetEmbedding(ctx, text)
		if err != nil {
			return IngestResult{}, fmt.Errorf("embed chunk: %w", err)
		}
		ent, _ := json.Marshal(map[string]any{
			"tool": batch.Tool, "session_id": batch.SessionID, "seq": seq,
		})
		toInsert = append(toInsert, chunkInsert{text: text, seq: seq, embed: emb, entities: ent})
		seq += len(c)
	}
	newChunkedCount := chunkedCount + (len(newMsgs) - len(remainder))

	// 4. Decide on and generate the summary (network, outside tx).
	now := time.Now()
	doSummary := shouldSummarize(len(newMsgs), lastSummarizedAt, now, batch.SessionEnded, summaryMinNewMsgs, summaryMaxAge)
	var summaryText string
	var summaryPayload, summaryEntities json.RawMessage
	var summaryTags []string
	var summaryEmbed pgvectorVector
	if doSummary {
		cs, err := generateConversationSummary(ctx, http.DefaultClient, openRouterBase, s.app.OpenRouterKey, renderTranscript(batch.Messages))
		if err != nil {
			// Best-effort: don't lose raw chunks because the summary failed.
			doSummary = false
		} else {
			summaryText = cs.Summary
			if summaryText == "" {
				summaryText = batch.Title
			}
			if summaryText == "" {
				// Nothing to embed or store — skip the summary this sweep.
				doSummary = false
			} else if summaryEmbed, err = s.app.GetEmbedding(ctx, summaryText); err != nil {
				// Best-effort: never lose raw chunks because the summary embed failed.
				doSummary = false
			} else {
				summaryPayload, _ = json.Marshal(cs)
				summaryEntities, _ = json.Marshal(map[string]any{
					"tool": batch.Tool, "session_id": batch.SessionID, "title": batch.Title,
				})
				summaryTags = cs.Topics
			}
		}
	}

	// 5. Persist everything in one RLS tx.
	newSummaryID := summaryEntryID
	if err := s.app.WithUserTx(ctx, func(tx pgx.Tx) error {
		for _, ci := range toInsert {
			emb := ci.embed
			if _, err := repository.InsertEntry(ctx, tx, repository.InsertEntryParams{
				RecordType:    recordTypeChunk,
				SchemaVersion: "1.0.0",
				Source:        batch.Tool,
				ContentText:   ci.text,
				Payload:       ci.entities, // payload mirrors entities for chunks
				Tags:          []string{},
				Entities:      ci.entities,
				Embedding:     &emb,
			}); err != nil {
				return err
			}
		}

		if doSummary {
			emb := summaryEmbed
			if summaryEntryID != nil {
				if err := repository.UpdateEntryContent(ctx, tx, repository.UpdateEntryContentParams{
					EntryID:     *summaryEntryID,
					ContentText: summaryText,
					Payload:     summaryPayload,
					Tags:        summaryTags,
					Entities:    summaryEntities,
					Embedding:   &emb,
				}); err != nil {
					return err
				}
			} else {
				id, err := repository.InsertEntry(ctx, tx, repository.InsertEntryParams{
					RecordType:    recordTypeSummary,
					SchemaVersion: "1.0.0",
					Source:        batch.Tool,
					ContentText:   summaryText,
					Payload:       summaryPayload,
					Tags:          summaryTags,
					Entities:      summaryEntities,
					Embedding:     &emb,
				})
				if err != nil {
					return err
				}
				newSummaryID = &id
			}
		}

		var summarizedAt *time.Time
		if doSummary {
			summarizedAt = &now
		}
		return repository.UpsertCapturedSession(ctx, tx, repository.UpsertCapturedSessionParams{
			Tool:             batch.Tool,
			SessionID:        batch.SessionID,
			SummaryEntryID:   newSummaryID,
			ChunkedMsgCount:  newChunkedCount,
			MessageCount:     len(batch.Messages),
			LastSummarizedAt: summarizedAt,
			SessionEnded:     batch.SessionEnded,
		})
	}); err != nil {
		return IngestResult{}, fmt.Errorf("persist ingest: %w", err)
	}

	return IngestResult{
		ChunksCreated: len(toInsert),
		Summarized:    doSummary,
		MessageCount:  len(batch.Messages),
	}, nil
}
