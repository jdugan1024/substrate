package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPatternMatch_LinkPrefix(t *testing.T) {
	cases := []string{
		"link:https://example.com",
		"LINK:https://example.com",
		"Link:https://example.com/article extra notes",
		"link: https://example.com",
	}
	for _, text := range cases {
		rt, conf := patternMatch(text)
		if rt != "note.link" {
			t.Errorf("patternMatch(%q): got record_type %q, want note.link", text, rt)
		}
		if conf != 1.0 {
			t.Errorf("patternMatch(%q): got confidence %v, want 1.0", text, conf)
		}
	}
}

func TestPatternMatch_LinkDoesNotMatchPlainURL(t *testing.T) {
	// A plain URL without the link: prefix should route to note.thought, not note.link
	rt, _ := patternMatch("https://example.com check this out")
	if rt == "note.link" {
		t.Error("plain URL without link: prefix should not match note.link")
	}
}

func TestBuildDeterministicEnvelope_NoteLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>Test Page</title></head><body/></html>`))
	}))
	defer srv.Close()

	a := &App{OpenRouterKey: "test"}
	env, err := a.buildDeterministicEnvelope(context.Background(), "link:"+srv.URL+"  my notes", "note.link")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.RecordType != "note.link" {
		t.Errorf("got record_type %q", env.RecordType)
	}
	if env.Confidence != 1.0 {
		t.Errorf("got confidence %v, want 1.0", env.Confidence)
	}
	if env.ContentText == "" {
		t.Error("content_text should not be empty")
	}

	var m map[string]any
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if m["url"] == nil {
		t.Error("payload missing url field")
	}
	if m["notes"] != "my notes" {
		t.Errorf("expected notes='my notes', got %v", m["notes"])
	}
}

func TestBuildDeterministicEnvelope_NoteLinkNoPrefix(t *testing.T) {
	// When called from CaptureTyped, text may not have the link: prefix
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>Bare URL Page</title></head><body/></html>`))
	}))
	defer srv.Close()

	a := &App{OpenRouterKey: "test"}
	env, err := a.buildDeterministicEnvelope(context.Background(), srv.URL, "note.link")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.RecordType != "note.link" {
		t.Errorf("got record_type %q", env.RecordType)
	}
}
