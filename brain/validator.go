// ABOUTME: Entry envelope validation — validates LLM-extracted payloads against registered JSON Schemas.
// ABOUTME: Server-side validation is mandatory; no LLM output is trusted without it.

package brain

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaCompilerCache caches compiled schemas to avoid recompiling on every validation.
var schemaCompilerCache map[string]*jsonschema.Schema

func init() {
	schemaCompilerCache = make(map[string]*jsonschema.Schema)
	compiler := jsonschema.NewCompiler()

	// Pre-compile all registered schemas at startup.
	for key, se := range schemaRegistry {
		// Encode key for use as a URL path component (@ is not valid in URLs).
		urlSafe := strings.ReplaceAll(key, "@", "-")
		url := "mem://schemas/" + urlSafe + ".json"

		var doc any
		if err := json.Unmarshal(se.Raw, &doc); err != nil {
			panic(fmt.Sprintf("compile schema %q: unmarshal: %v", key, err))
		}
		if err := compiler.AddResource(url, doc); err != nil {
			panic(fmt.Sprintf("compile schema %q: add resource: %v", key, err))
		}
		compiled, err := compiler.Compile(url)
		if err != nil {
			panic(fmt.Sprintf("compile schema %q: compile: %v", key, err))
		}
		schemaCompilerCache[key] = compiled
	}
}

// Envelope is the expected JSON structure returned by the LLM extractor.
type Envelope struct {
	RecordType    string           `json:"record_type"`
	SchemaVersion string           `json:"schema_version"`
	Payload       json.RawMessage  `json:"payload"`
	ContentText   string           `json:"content_text"`
	Tags          []string         `json:"tags"`
	Entities      EnvelopeEntities `json:"entities"`
	Confidence    float64          `json:"confidence"`
}

// EnvelopeEntities holds extracted named entities from the LLM.
type EnvelopeEntities struct {
	People []string `json:"people"`
	Orgs   []string `json:"orgs"`
	Dates  []string `json:"dates"`
}

// ValidationResult is the outcome of ValidateEnvelope.
type ValidationResult struct {
	Valid        bool
	FailureMode  string // "low_confidence" | "validation_failure" | ""
	ErrorMessage string
}

// ValidateEnvelope checks an LLM-returned envelope for structural validity,
// known record_type, schema version existence, payload conformance, and
// confidence threshold. Returns a ValidationResult indicating pass or the
// specific failure mode.
func ValidateEnvelope(env *Envelope, minConfidence float64) ValidationResult {
	// 1. Required envelope fields.
	if env.RecordType == "" {
		return ValidationResult{FailureMode: "validation_failure", ErrorMessage: "missing record_type"}
	}
	if env.SchemaVersion == "" {
		return ValidationResult{FailureMode: "validation_failure", ErrorMessage: "missing schema_version"}
	}
	if env.ContentText == "" {
		return ValidationResult{FailureMode: "validation_failure", ErrorMessage: "missing content_text"}
	}

	// 2. Confidence threshold check (before schema lookup to avoid unnecessary work).
	if env.Confidence < minConfidence {
		return ValidationResult{
			FailureMode:  "low_confidence",
			ErrorMessage: fmt.Sprintf("confidence %.2f below threshold %.2f", env.Confidence, minConfidence),
		}
	}

	// 3. Schema existence check.
	key := env.RecordType + "@" + env.SchemaVersion
	compiled, ok := schemaCompilerCache[key]
	if !ok {
		return ValidationResult{
			FailureMode:  "validation_failure",
			ErrorMessage: fmt.Sprintf("no schema registered for %q at version %q", env.RecordType, env.SchemaVersion),
		}
	}

	// 4. Payload JSON Schema validation.
	var payloadVal any
	if err := json.Unmarshal(env.Payload, &payloadVal); err != nil {
		return ValidationResult{
			FailureMode:  "validation_failure",
			ErrorMessage: "payload is not valid JSON: " + err.Error(),
		}
	}
	if err := compiled.Validate(payloadVal); err != nil {
		return ValidationResult{
			FailureMode:  "validation_failure",
			ErrorMessage: "payload schema violation: " + err.Error(),
		}
	}

	return ValidationResult{Valid: true}
}

// FallbackPayload builds a note.unstructured payload from a failed envelope attempt.
// rawInput is the original user text; env may be nil if the LLM returned garbage.
func FallbackPayload(rawInput string, env *Envelope, result ValidationResult) (json.RawMessage, error) {
	m := map[string]any{
		"content":   rawInput,
		"raw_input": rawInput,
	}
	if env != nil && env.RecordType != "" {
		m["attempted_record_type"] = env.RecordType
	}
	if result.FailureMode != "" {
		m["failure_mode"] = result.FailureMode
	}
	if result.ErrorMessage != "" {
		m["rejection_reason"] = result.ErrorMessage
	}
	return json.Marshal(m)
}
