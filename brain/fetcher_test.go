package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode/utf8"
)

func TestFetchFullText_StripsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body><p>Hello</p>  <p>World</p></body></html>`))
	}))
	defer srv.Close()

	text, err := fetchFullText(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Hello World"; text != want {
		t.Errorf("got text %q, want %q", text, want)
	}
}

// A PDF (or any non-text content type) must not be treated as extractable
// text: its raw bytes are not valid UTF-8 and poison the content_text column.
func TestFetchFullText_RejectsNonTextContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte{'%', 'P', 'D', 'F', '-', '1', '.', '4', 0xd0, 0xd4, 0x00})
	}))
	defer srv.Close()

	_, err := fetchFullText(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for non-text content type, got nil")
	}
}

// Even a text/html response can carry bytes that are not valid UTF-8 (e.g. a
// page served in a single-byte encoding). The extracted text must be scrubbed
// to valid UTF-8 so Postgres accepts it.
func TestFetchFullText_SanitizesInvalidUTF8(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte{'<', 'p', '>', 'a', 0xd0, 0xd4, 'b', '<', '/', 'p', '>'})
	}))
	defer srv.Close()

	text, err := fetchFullText(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !utf8.ValidString(text) {
		t.Errorf("returned text is not valid UTF-8: %q", text)
	}
}

func TestFetchLinkMeta_OGTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head>
			<meta property="og:title" content="OG Title">
			<meta property="og:description" content="OG Description">
		</head><body>hello</body></html>`))
	}))
	defer srv.Close()

	title, desc, err := FetchLinkMeta(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "OG Title" {
		t.Errorf("got title %q, want %q", title, "OG Title")
	}
	if desc != "OG Description" {
		t.Errorf("got desc %q, want %q", desc, "OG Description")
	}
}

func TestFetchLinkMeta_FallbackTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Fallback Title</title></head><body/></html>`))
	}))
	defer srv.Close()

	title, desc, err := FetchLinkMeta(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Fallback Title" {
		t.Errorf("got title %q, want %q", title, "Fallback Title")
	}
	if desc != "" {
		t.Errorf("expected empty desc, got %q", desc)
	}
}

func TestFetchLinkMeta_MetaDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head>
			<title>Page Title</title>
			<meta name="description" content="Meta Desc">
		</head></html>`))
	}))
	defer srv.Close()

	title, desc, err := FetchLinkMeta(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Page Title" {
		t.Errorf("got title %q", title)
	}
	if desc != "Meta Desc" {
		t.Errorf("got desc %q", desc)
	}
}

func TestFetchLinkMeta_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := FetchLinkMeta(context.Background(), srv.URL)
	if err == nil {
		t.Error("expected error for 404 response")
	}
}

func TestParseLinkText_URLOnly(t *testing.T) {
	url, notes := ParseLinkText("https://example.com/article")
	if url != "https://example.com/article" {
		t.Errorf("got url %q", url)
	}
	if notes != "" {
		t.Errorf("expected empty notes, got %q", notes)
	}
}

func TestParseLinkText_URLWithNotes(t *testing.T) {
	url, notes := ParseLinkText("https://example.com/article  great piece on distributed systems")
	if url != "https://example.com/article" {
		t.Errorf("got url %q", url)
	}
	if notes != "great piece on distributed systems" {
		t.Errorf("got notes %q", notes)
	}
}

func TestParseLinkText_LeadingSpace(t *testing.T) {
	url, notes := ParseLinkText("  https://example.com  my note  ")
	if url != "https://example.com" {
		t.Errorf("got url %q", url)
	}
	if notes != "my note" {
		t.Errorf("got notes %q", notes)
	}
}

func TestBuildLinkPayload_FetchedWithBoth(t *testing.T) {
	payload, contentText := BuildLinkPayload("https://example.com", "My Title", "My Desc", "my notes", nil)
	if contentText != "My Title — My Desc (https://example.com)" {
		t.Errorf("got contentText %q", contentText)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["fetch_status"] != "fetched" {
		t.Errorf("expected fetch_status=fetched, got %v", m["fetch_status"])
	}
	if m["extract_status"] != "pending" {
		t.Errorf("expected extract_status=pending, got %v", m["extract_status"])
	}
	if m["notes"] != "my notes" {
		t.Errorf("expected notes='my notes', got %v", m["notes"])
	}
}

func TestBuildLinkPayload_FetchedTitleOnly(t *testing.T) {
	_, contentText := BuildLinkPayload("https://example.com", "My Title", "", "", nil)
	if contentText != "My Title (https://example.com)" {
		t.Errorf("got contentText %q", contentText)
	}
}

func TestBuildLinkPayload_FetchError(t *testing.T) {
	payload, contentText := BuildLinkPayload("https://example.com", "", "", "", fmt.Errorf("timeout"))
	if contentText != "https://example.com" {
		t.Errorf("got contentText %q", contentText)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["fetch_status"] != "pending" {
		t.Errorf("expected fetch_status=pending, got %v", m["fetch_status"])
	}
	if _, ok := m["extract_status"]; ok {
		t.Error("extract_status should not be set when fetch failed")
	}
	if m["fetch_error"] == nil {
		t.Error("fetch_error should be set on fetch failure")
	}
}

func TestBuildLinkPayload_NoNotes(t *testing.T) {
	payload, _ := BuildLinkPayload("https://example.com", "T", "D", "", nil)
	var m map[string]any
	json.Unmarshal(payload, &m)
	if _, ok := m["notes"]; ok {
		t.Error("notes should be omitted when empty")
	}
}

func TestBuildNoteLinkEnvelope_WithLinkPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Test Page</title></head><body/></html>`))
	}))
	defer srv.Close()

	env := BuildNoteLinkEnvelope(context.Background(), "link:"+srv.URL+"  my notes")

	if env.RecordType != "note.link" {
		t.Errorf("got RecordType %q, want %q", env.RecordType, "note.link")
	}
	if env.SchemaVersion != "1.0.0" {
		t.Errorf("got SchemaVersion %q, want %q", env.SchemaVersion, "1.0.0")
	}
	if env.Confidence != 1.0 {
		t.Errorf("got Confidence %v, want 1.0", env.Confidence)
	}
	if env.ContentText == "" {
		t.Error("ContentText should not be empty")
	}
	var m map[string]any
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if m["notes"] != "my notes" {
		t.Errorf("got notes %v, want %q", m["notes"], "my notes")
	}
}

func TestBuildNoteLinkEnvelope_WithoutPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>No Prefix</title></head><body/></html>`))
	}))
	defer srv.Close()

	env := BuildNoteLinkEnvelope(context.Background(), srv.URL)

	if env.RecordType != "note.link" {
		t.Errorf("got RecordType %q, want %q", env.RecordType, "note.link")
	}
	if env.Confidence != 1.0 {
		t.Errorf("got Confidence %v, want 1.0", env.Confidence)
	}
	var m map[string]any
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if m["url"] != srv.URL {
		t.Errorf("got url %v, want %q", m["url"], srv.URL)
	}
	if _, ok := m["notes"]; ok {
		t.Error("notes should be omitted when empty")
	}
}
