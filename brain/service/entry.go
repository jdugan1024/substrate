// ABOUTME: EntryService — extract, validate, and persist entries to the canonical store.
// ABOUTME: Used by both the add_item MCP tool and the web capture handler.

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"open-brain-go/brain"
	"open-brain-go/brain/repository"
)

// EntryService orchestrates extraction, validation, embedding, and persistence
// for the canonical entries table.
type EntryService struct {
	app *brain.App
}

// NewEntryService creates an EntryService backed by the given App.
func NewEntryService(app *brain.App) *EntryService {
	return &EntryService{app: app}
}

// CaptureResult is returned by Capture after a successful write.
type CaptureResult struct {
	EntryID    string
	RecordType string
	Message    string
	// Fallback is true when the entry could not be confidently classified and
	// was stored as note.unstructured. The web UI uses this to show friendly
	// low-confidence guidance instead of the raw failure_mode.
	Fallback bool
}

// Capture extracts, validates, and persists an entry from free-form text.
// source is the capture path ("web" or "mcp").
//
// On low confidence or validation failure, the entry is stored as
// note.unstructured with the failure_mode field populated — Capture always
// returns a non-nil result on success (even for fallback entries).
func (s *EntryService) Capture(ctx context.Context, text, source string) (*CaptureResult, error) {
	minConf := brain.MinConfidence()

	// 1. Extract envelope (pattern rules first, then LLM).
	env, err := s.app.Extract(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}

	// 2. Validate envelope + payload against JSON Schema.
	vr := brain.ValidateEnvelope(env, minConf)

	// 3. Build insert params — either the validated payload or a fallback.
	var params repository.InsertEntryParams
	params.Source = source

	if vr.Valid {
		params.RecordType = env.RecordType
		params.SchemaVersion = env.SchemaVersion
		params.ContentText = env.ContentText
		params.Payload = env.Payload
		params.Confidence = &env.Confidence
		params.Tags = tagsFromEnv(env)
		params.Entities = entitiesFromEnv(env)

		log.Printf("entry: record_type=%s confidence=%.2f source=%s", env.RecordType, env.Confidence, source)
	} else {
		// Fallback: store as note.unstructured.
		fallbackPayload, err := brain.FallbackPayload(text, env, vr)
		if err != nil {
			return nil, fmt.Errorf("build fallback payload: %w", err)
		}

		params.RecordType = "note.unstructured"
		params.SchemaVersion = "1.0.0"
		params.ContentText = text
		params.Payload = fallbackPayload
		params.FailureMode = &vr.FailureMode

		log.Printf("entry: fallback failure_mode=%s reason=%s source=%s", vr.FailureMode, vr.ErrorMessage, source)
	}

	// 4. Generate embedding for content_text.
	emb, err := s.app.GetEmbedding(ctx, params.ContentText)
	if err != nil {
		return nil, fmt.Errorf("embedding: %w", err)
	}
	params.Embedding = &emb

	// 5. Persist.
	var entryID string
	if err := s.app.WithUserTx(ctx, func(tx pgx.Tx) error {
		id, err := repository.InsertEntry(ctx, tx, params)
		entryID = id
		return err
	}); err != nil {
		return nil, fmt.Errorf("persist entry: %w", err)
	}

	msg := fmt.Sprintf("Saved as %s (id: %s)", params.RecordType, entryID)
	if !vr.Valid {
		msg = fmt.Sprintf("Saved as note [%s] (id: %s)", vr.FailureMode, entryID)
	}

	return &CaptureResult{
		EntryID:    entryID,
		RecordType: params.RecordType,
		Message:    msg,
		Fallback:   !vr.Valid,
	}, nil
}

// CaptureTyped extracts and persists an entry when the record_type is already
// known (e.g. from an add_item <type> <content> MCP call). Skips LLM
// classification; only performs constrained field extraction.
func (s *EntryService) CaptureTyped(ctx context.Context, recordType, text, source string) (*CaptureResult, error) {
	minConf := brain.MinConfidence()

	se, err := brain.SchemaFor(recordType, "1.0.0")
	if err != nil {
		return nil, fmt.Errorf("unknown record type %q: %w", recordType, err)
	}

	schemaBytes, _ := json.MarshalIndent(se.Schema, "", "  ")
	prompt := fmt.Sprintf(
		`Extract structured fields from this text for record type %q.\nJSON Schema: %s\n\nReturn ONLY a JSON object matching the schema. Do not include any fields not in the schema. Today: %s`,
		recordType, string(schemaBytes), currentDate(),
	)

	env := &brain.Envelope{
		RecordType:    recordType,
		SchemaVersion: "1.0.0",
		ContentText:   text,
		Confidence:    0.9, // type is known — high base confidence
	}

	// For note.link, parse url+notes and sync-fetch metadata; no LLM involved.
	if recordType == "note.link" {
		built := brain.BuildNoteLinkEnvelope(ctx, text)
		env.Payload, env.ContentText = built.Payload, built.ContentText
	} else if recordType == "note.thought" {
		env.Payload, _ = json.Marshal(map[string]any{"content": text})
	} else {
		payload, err := s.app.LLMExtractPayload(ctx, prompt, text)
		if err != nil {
			// Fall through to fallback on LLM failure.
			vr := brain.ValidationResult{FailureMode: "validation_failure", ErrorMessage: "payload extraction failed: " + err.Error()}
			return s.persistFallback(ctx, text, source, env, vr)
		}
		env.Payload = payload
	}

	vr := brain.ValidateEnvelope(env, minConf)
	if !vr.Valid {
		return s.persistFallback(ctx, text, source, env, vr)
	}

	return s.persistValid(ctx, env, source)
}

func (s *EntryService) persistValid(ctx context.Context, env *brain.Envelope, source string) (*CaptureResult, error) {
	emb, err := s.app.GetEmbedding(ctx, env.ContentText)
	if err != nil {
		return nil, fmt.Errorf("embedding: %w", err)
	}

	params := repository.InsertEntryParams{
		RecordType:    env.RecordType,
		SchemaVersion: env.SchemaVersion,
		Source:        source,
		Confidence:    &env.Confidence,
		ContentText:   env.ContentText,
		Payload:       env.Payload,
		Tags:          tagsFromEnv(env),
		Entities:      entitiesFromEnv(env),
		Embedding:     &emb,
	}

	var entryID string
	if err := s.app.WithUserTx(ctx, func(tx pgx.Tx) error {
		id, err := repository.InsertEntry(ctx, tx, params)
		entryID = id
		return err
	}); err != nil {
		return nil, fmt.Errorf("persist entry: %w", err)
	}

	log.Printf("entry: record_type=%s confidence=%.2f source=%s id=%s", env.RecordType, env.Confidence, source, entryID)
	return &CaptureResult{
		EntryID:    entryID,
		RecordType: env.RecordType,
		Message:    fmt.Sprintf("Saved as %s (id: %s)", env.RecordType, entryID),
	}, nil
}

func (s *EntryService) persistFallback(ctx context.Context, text, source string, env *brain.Envelope, vr brain.ValidationResult) (*CaptureResult, error) {
	fallbackPayload, err := brain.FallbackPayload(text, env, vr)
	if err != nil {
		return nil, fmt.Errorf("build fallback payload: %w", err)
	}

	emb, err := s.app.GetEmbedding(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("embedding: %w", err)
	}

	params := repository.InsertEntryParams{
		RecordType:    "note.unstructured",
		SchemaVersion: "1.0.0",
		Source:        source,
		FailureMode:   &vr.FailureMode,
		ContentText:   text,
		Payload:       fallbackPayload,
		Embedding:     &emb,
	}

	var entryID string
	if err := s.app.WithUserTx(ctx, func(tx pgx.Tx) error {
		id, err := repository.InsertEntry(ctx, tx, params)
		entryID = id
		return err
	}); err != nil {
		return nil, fmt.Errorf("persist fallback: %w", err)
	}

	log.Printf("entry: fallback failure_mode=%s reason=%s source=%s id=%s", vr.FailureMode, vr.ErrorMessage, source, entryID)
	return &CaptureResult{
		EntryID:    entryID,
		RecordType: "note.unstructured",
		Message:    fmt.Sprintf("Saved as note [%s] (id: %s)", vr.FailureMode, entryID),
		Fallback:   true,
	}, nil
}

func tagsFromEnv(env *brain.Envelope) []string {
	if env.Tags != nil {
		return env.Tags
	}
	return []string{}
}

func entitiesFromEnv(env *brain.Envelope) json.RawMessage {
	b, err := json.Marshal(env.Entities)
	if err != nil || string(b) == "null" {
		return json.RawMessage("{}")
	}
	return b
}

func currentDate() string {
	return time.Now().Format("2006-01-02")
}
