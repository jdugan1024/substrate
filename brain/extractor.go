// ABOUTME: Entry extractor — routes free-form text to the right record type.
// ABOUTME: Tries deterministic pattern rules first; falls back to one-shot LLM extraction.
// ABOUTME: Replaces brain/dispatch.go:DispatchCapture for the web capture path.

package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// MinConfidence returns the configured confidence threshold (default 0.7).
func MinConfidence() float64 {
	if v := os.Getenv("ENGRAM_EXTRACTION_MIN_CONFIDENCE"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil && f > 0 && f <= 1 {
			return f
		}
	}
	return 0.7
}

// --- Deterministic pattern rules ---

var (
	reEmail = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	rePhone = regexp.MustCompile(`(?i)(\+?1[\s\-.]?)?\(?\d{3}\)?[\s\-.]?\d{3}[\s\-.]?\d{4}`)
	reURL   = regexp.MustCompile(`(?i)https?://\S+`)

	reMaintenanceKeywords = regexp.MustCompile(
		`(?i)\b(replace|repair|fix|service|inspect|clean|check|install|every\s+\d+\s+(day|week|month|year))\b`)
	reJobKeywords = regexp.MustCompile(
		`(?i)\b(applied|applying|interviewing|interview|offer|rejected|job\s+posting|position\s+at|hiring|opening\s+at)\b`)
)

// patternMatch attempts to classify text using deterministic rules.
// Returns (recordType, confidence) or ("", 0) if no strong signal.
func patternMatch(text string) (recordType string, confidence float64) {
	// Explicit link: prefix — highest priority, no ambiguity.
	if trimmed := strings.TrimSpace(text); len(trimmed) >= 5 && strings.EqualFold(trimmed[:5], "link:") {
		return "note.link", 1.0
	}

	hasEmail := reEmail.MatchString(text)
	hasPhone := rePhone.MatchString(text)
	hasURL := reURL.MatchString(text)
	hasMaintenance := reMaintenanceKeywords.MatchString(text)
	hasJob := reJobKeywords.MatchString(text)

	switch {
	case (hasEmail || hasPhone) && !hasMaintenance && !hasJob:
		// Contact information present
		return "crm.contact", 0.8
	case hasMaintenance && !hasEmail && !hasJob:
		// Maintenance-specific vocabulary
		return "maintenance.task", 0.8
	case hasURL && hasJob:
		// URL + job keywords → job application
		return "jobhunt.application", 0.8
	case hasJob && !hasURL && !hasEmail:
		// Job keywords without URL (may need LLM for full details)
		return "jobhunt.application", 0.72
	case hasURL && !hasJob && !hasEmail && !hasMaintenance:
		// URL only → general thought/reference
		return "note.thought", 0.75
	}
	return "", 0
}

// --- LLM one-shot extraction ---

var extractorSystemPrompt = `You are a personal knowledge extractor. Given text from the user, determine the best record type and extract a structured payload. Respond with ONLY a valid JSON object — no markdown, no explanation.

Today's date: %s

RECORD TYPES (choose exactly one):

note.thought — general notes, ideas, observations, reminders
  payload: {"content":"<text>","topics":["..."],"people":["..."],"action_items":["..."],"thought_type":"observation|task|idea|reference|person_note"}

crm.contact — adding a person to professional contacts
  Trigger: met someone, new contact, person's name + company/title/email
  payload: {"name":"...","company":"...","title":"...","email":"...","phone":"...","how_we_met":"...","notes":"..."}

crm.interaction — logging an interaction with a known person
  Trigger: met with, talked to, call with, coffee with a named person
  payload: {"person_name":"...","interaction_type":"meeting|call|coffee|email|conference|linkedin|other","summary":"...","follow_up_needed":true|false,"follow_up_notes":"...","interaction_date":"YYYY-MM-DD"}

maintenance.task — home repair, upkeep, or maintenance task
  Trigger: fix, replace, service, HVAC, plumbing, filter, paint, clean (home context), every N days
  payload: {"name":"...","category":"...","location":"...","frequency_days":90,"next_due":"YYYY-MM-DD","notes":"..."}

jobhunt.application — a job to track
  Trigger: applied, applying, job posting, position at, opening at
  payload: {"company_name":"...","title":"...","posting_url":"...","status":"applied|screening|interviewing|offer|accepted|rejected|withdrawn","applied_date":"YYYY-MM-DD","notes":"..."}

Output format:
{
  "record_type": "<type>",
  "schema_version": "1.0.0",
  "payload": { ... },
  "content_text": "<one sentence summary of the item>",
  "tags": ["tag1"],
  "entities": {"people":["..."],"orgs":["..."],"dates":["..."]},
  "confidence": 0.0-1.0
}

When in doubt, use note.thought.`

// Extract classifies and extracts structured data from free-form text.
// It runs deterministic pattern rules first; if confidence is sufficient it
// skips the LLM call. Otherwise it calls the LLM for one-shot extraction.
// The returned Envelope may have Valid=false in its ValidationResult if it
// needs to be stored as note.unstructured — the caller should check.
func (a *App) Extract(ctx context.Context, text string) (*Envelope, error) {
	minConf := MinConfidence()

	// 1. Deterministic rules — cheap path.
	if rt, conf := patternMatch(text); rt != "" && conf >= minConf {
		env, err := a.buildDeterministicEnvelope(ctx, text, rt)
		if err == nil {
			env.Confidence = conf
			return env, nil
		}
		// If deterministic build fails, fall through to LLM.
	}

	// 2. LLM one-shot extraction.
	return a.llmExtract(ctx, text)
}

// buildDeterministicEnvelope constructs an envelope for cases where the record
// type is already known from pattern matching. Uses the LLM only for field
// extraction (a narrower, cheaper prompt).
func (a *App) buildDeterministicEnvelope(ctx context.Context, text, recordType string) (*Envelope, error) {
	// note.link: strip optional link: prefix, parse url+notes, sync fetch metadata.
	if recordType == "note.link" {
		return BuildNoteLinkEnvelope(ctx, text), nil
	}

	// For note.thought, we can skip LLM entirely — just wrap the content.
	if recordType == "note.thought" {
		payload, _ := json.Marshal(map[string]any{"content": text})
		return &Envelope{
			RecordType:    "note.thought",
			SchemaVersion: "1.0.0",
			Payload:       payload,
			ContentText:   text,
			Confidence:    0.9,
		}, nil
	}

	// For typed records with a known type, use a constrained extraction prompt.
	se, err := SchemaFor(recordType, "1.0.0")
	if err != nil {
		return nil, err
	}

	schemaBytes, _ := json.MarshalIndent(se.Schema, "", "  ")
	prompt := fmt.Sprintf(
		`Extract structured fields from this text for record type %q.\nJSON Schema: %s\n\nReturn ONLY a JSON object matching the schema. Today: %s`,
		recordType, string(schemaBytes), time.Now().Format("2006-01-02"),
	)

	payload, err := a.LLMExtractPayload(ctx, prompt, text)
	if err != nil {
		return nil, err
	}

	return &Envelope{
		RecordType:    recordType,
		SchemaVersion: "1.0.0",
		Payload:       payload,
		ContentText:   text,
		Confidence:    0.85,
	}, nil
}

// llmExtract makes a single LLM call to classify and extract the full envelope.
func (a *App) llmExtract(ctx context.Context, text string) (*Envelope, error) {
	prompt := fmt.Sprintf(extractorSystemPrompt, time.Now().Format("2006-01-02"))

	body, _ := json.Marshal(map[string]any{
		"model":           "openai/gpt-4o-mini",
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": prompt},
			{"role": "user", "content": text},
		},
	})

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.OpenRouterKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return envelopeFallback(text, "llm request failed: "+err.Error()), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.ReadAll(resp.Body)
		return envelopeFallback(text, fmt.Sprintf("llm status %d", resp.StatusCode)), nil
	}

	var result struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Choices) == 0 {
		return envelopeFallback(text, "llm response decode failed"), nil
	}

	var env Envelope
	if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &env); err != nil {
		return envelopeFallback(text, "llm output not valid envelope JSON: "+err.Error()), nil
	}

	// Ensure content_text is populated.
	if env.ContentText == "" {
		env.ContentText = text
	}

	return &env, nil
}

// LLMExtractPayload calls the LLM for constrained field extraction (type is already known).
func (a *App) LLMExtractPayload(ctx context.Context, systemPrompt, text string) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{
		"model":           "openai/gpt-4o-mini",
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
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

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("llm status %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Choices) == 0 {
		return nil, fmt.Errorf("llm response decode failed")
	}

	return json.RawMessage(result.Choices[0].Message.Content), nil
}

// envelopeFallback returns a note.thought envelope when LLM extraction fails entirely.
func envelopeFallback(text, reason string) *Envelope {
	payload, _ := json.Marshal(map[string]any{"content": text})
	return &Envelope{
		RecordType:    "note.thought",
		SchemaVersion: "1.0.0",
		Payload:       payload,
		ContentText:   text,
		Confidence:    0.5, // below threshold → will be stored as note.unstructured
	}
}
