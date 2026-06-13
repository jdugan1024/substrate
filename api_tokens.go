// ABOUTME: Personal access token (PAT) helpers and web management handlers.
// ABOUTME: Tokens authenticate headless clients (e.g. the capture daemon) to /ingest.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"

	"open-brain-go/brain"
	"open-brain-go/brain/repository"
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

type createTokenRequest struct {
	Name string `json:"name"`
}

type createTokenResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"` // plaintext — shown exactly once
}

// createTokenHandler issues a new PAT for the authenticated user and returns
// the plaintext token once. Runs behind webAuthMiddleware (userID in context).
func createTokenHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(brain.CtxUserID).(string)
		if userID == "" {
			http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
			return
		}
		var req createTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
			return
		}

		plaintext, hash := generateAPIToken()
		id, err := repository.InsertAPIToken(r.Context(), a.Pool, userID, req.Name, hash)
		if err != nil {
			log.Printf("create token error: %v", err)
			http.Error(w, `{"error":"failed to create token"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createTokenResponse{ID: id, Name: req.Name, Token: plaintext})
	}
}

// listTokensHandler returns the user's live tokens (no hashes, no plaintext).
func listTokensHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(brain.CtxUserID).(string)
		tokens, err := repository.ListAPITokens(r.Context(), a.Pool, userID)
		if err != nil {
			log.Printf("list tokens error: %v", err)
			http.Error(w, `{"error":"failed to list tokens"}`, http.StatusInternalServerError)
			return
		}
		if tokens == nil {
			tokens = []repository.APIToken{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tokens": tokens})
	}
}

// revokeTokenHandler revokes one of the user's tokens by id (path value "id").
func revokeTokenHandler(a *brain.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(brain.CtxUserID).(string)
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, `{"error":"id is required"}`, http.StatusBadRequest)
			return
		}
		if err := repository.RevokeAPIToken(r.Context(), a.Pool, userID, id); err != nil {
			log.Printf("revoke token error: %v", err)
			http.Error(w, `{"error":"failed to revoke token"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
