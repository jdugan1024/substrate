// ABOUTME: Personal access token (PAT) helpers and web management handlers.
// ABOUTME: Tokens authenticate headless clients (e.g. the capture daemon) to /ingest.

package main

import (
	"crypto/sha256"
	"encoding/hex"
)

// tokenPrefix marks a bearer token as a PAT so authMiddleware can route it to
// the PAT lookup instead of OIDC verification.
const tokenPrefix = "engram_pat_"

// generateAPIToken returns a new opaque token (to show the user once) and its
// SHA-256 hex hash (to store).
func generateAPIToken() (plaintext, hash string) {
	plaintext = tokenPrefix + randomHex(32) // randomHex is defined in web_auth.go
	return plaintext, hashAPIToken(plaintext)
}

// hashAPIToken returns the SHA-256 hex hash of a token's plaintext.
func hashAPIToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
