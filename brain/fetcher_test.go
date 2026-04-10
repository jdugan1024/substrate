package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
