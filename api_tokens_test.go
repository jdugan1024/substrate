package main

import (
	"strings"
	"testing"
)

func TestGenerateAPIToken_Format(t *testing.T) {
	plaintext, hash := generateAPIToken()
	if !strings.HasPrefix(plaintext, tokenPrefix) {
		t.Fatalf("plaintext missing prefix %q: %q", tokenPrefix, plaintext)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if hash == plaintext {
		t.Fatal("hash must differ from plaintext")
	}
}

func TestGenerateAPIToken_Unique(t *testing.T) {
	p1, h1 := generateAPIToken()
	p2, h2 := generateAPIToken()
	if p1 == p2 || h1 == h2 {
		t.Fatal("expected distinct tokens and hashes")
	}
}

func TestHashAPIToken_MatchesGenerated(t *testing.T) {
	plaintext, hash := generateAPIToken()
	if got := hashAPIToken(plaintext); got != hash {
		t.Fatalf("hashAPIToken(plaintext)=%q, want %q", got, hash)
	}
}

func TestHashAPIToken_Deterministic(t *testing.T) {
	if hashAPIToken("substrate_pat_abc") != hashAPIToken("substrate_pat_abc") {
		t.Fatal("hash should be deterministic")
	}
}
