package main

import (
	"strings"
	"testing"
)

func TestMatchFields(t *testing.T) {
	if got := matchFields(false, false, false, false); got != nil {
		t.Errorf("no matches: want nil, got %v", got)
	}
	got := matchFields(true, false, true, true)
	want := []string{"title", "topics", "body"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestBuildEntriesQueryNoQuery(t *testing.T) {
	sql, args := buildEntriesQuery("", "", 51, 0)
	if strings.Contains(sql, "websearch_to_tsquery") {
		t.Error("empty query should not use FTS")
	}
	// limit + offset only
	if len(args) != 2 || args[0] != 51 || args[1] != 0 {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(sql, "ORDER BY created_at DESC") {
		t.Error("expected recency order")
	}
}

func TestBuildEntriesQueryWithQueryAndType(t *testing.T) {
	sql, args := buildEntriesQuery("engram notes", "note.thought", 51, 100)
	if !strings.Contains(sql, "websearch_to_tsquery") || !strings.Contains(sql, "ts_rank") {
		t.Error("expected FTS query with ranking")
	}
	// $1=q, $2=type, $3=limit, $4=offset
	if len(args) != 4 {
		t.Fatalf("want 4 args, got %d: %v", len(args), args)
	}
	if args[0] != "engram notes" || args[1] != "note.thought" || args[2] != 51 || args[3] != 100 {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(sql, "LIMIT $3 OFFSET $4") {
		t.Errorf("param numbering wrong: %s", sql)
	}
	if !strings.Contains(sql, "AND record_type = $2") {
		t.Error("expected type filter on $2")
	}
}

func TestBuildEntriesQueryWithQueryNoType(t *testing.T) {
	sql, args := buildEntriesQuery("engram", "", 51, 0)
	if len(args) != 3 || args[0] != "engram" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(sql, "LIMIT $2 OFFSET $3") {
		t.Errorf("param numbering wrong: %s", sql)
	}
}

func TestBuildCountsQuery(t *testing.T) {
	sql, args := buildCountsQuery("")
	if strings.Contains(sql, "websearch_to_tsquery") || args != nil {
		t.Error("empty query counts should be a plain group-by")
	}
	sql, args = buildCountsQuery("engram")
	if !strings.Contains(sql, "GROUP BY record_type") || len(args) != 1 || args[0] != "engram" {
		t.Errorf("fts counts wrong: args=%v", args)
	}
	// Counts must NOT filter by record_type (every chip needs a count).
	if strings.Contains(sql, "record_type =") {
		t.Error("counts query should not filter by type")
	}
}
