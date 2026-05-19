// ABOUTME: Server-side session management for the web UI.
// ABOUTME: Direct OIDC flow to Authelia + httpOnly session cookies, bypassing the MCP proxy.

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"open-brain-go/brain"
)

const (
	webCallbackURL = "https://engram.x1024.net/web/callback"
	sessionTTL     = 30 * 24 * time.Hour
	pendingLoginTTL = 10 * time.Minute
)

type webSession struct {
	userID    string
	expiresAt time.Time
}

type pendingWebLogin struct {
	verifier string
	created  time.Time
}

// WebSessionStore manages server-side sessions for the web UI.
type WebSessionStore struct {
	mu       sync.Mutex
	sessions map[string]webSession
	pending  map[string]pendingWebLogin
}

func NewWebSessionStore() *WebSessionStore {
	return &WebSessionStore{
		sessions: make(map[string]webSession),
		pending:  make(map[string]pendingWebLogin),
	}
}

// StorePending records a PKCE verifier for the given OAuth state parameter.
func (s *WebSessionStore) StorePending(state, verifier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[state] = pendingWebLogin{verifier: verifier, created: time.Now()}
}

// PopPending retrieves and removes a pending login entry by state, returning the
// code_verifier. Returns false if state is unknown or older than 10 minutes.
// Also cleans up any other stale pending entries while the lock is held.
func (s *WebSessionStore) PopPending(state string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[state]
	delete(s.pending, state)
	for k, v := range s.pending {
		if time.Since(v.created) > pendingLoginTTL {
			delete(s.pending, k)
		}
	}
	if !ok || time.Since(p.created) > pendingLoginTTL {
		return "", false
	}
	return p.verifier, true
}

// Create adds a new session for userID and returns a random session ID.
func (s *WebSessionStore) Create(userID string) string {
	id := randomHex(32)
	s.mu.Lock()
	s.sessions[id] = webSession{userID: userID, expiresAt: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return id
}

// Get returns the userID for a valid, unexpired session.
func (s *WebSessionStore) Get(sessionID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok || time.Now().After(sess.expiresAt) {
		delete(s.sessions, sessionID)
		return "", false
	}
	return sess.userID, true
}

// Delete removes a session.
func (s *WebSessionStore) Delete(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

// StartCleanup runs a background goroutine that removes expired sessions hourly.
func (s *WebSessionStore) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				for id, sess := range s.sessions {
					if time.Now().After(sess.expiresAt) {
						delete(s.sessions, id)
					}
				}
				s.mu.Unlock()
			}
		}
	}()
}

// webAuthMiddleware validates the engram_session cookie and injects userID into context.
func webAuthMiddleware(sessions *WebSessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("engram_session")
		if err != nil {
			http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
			return
		}
		userID, ok := sessions.Get(cookie.Value)
		if !ok {
			clearSessionCookie(w)
			http.Error(w, `{"error":"session expired"}`, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), brain.CtxUserID, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// webCheckHandler returns 200 if the request has a valid session, 401 otherwise.
// Used by the web UI on init to decide whether to show auth or content.
func webCheckHandler(sessions *WebSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("engram_session")
		if err != nil {
			http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
			return
		}
		if _, ok := sessions.Get(cookie.Value); !ok {
			clearSessionCookie(w)
			http.Error(w, `{"error":"session expired"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// webLoginHandler initiates the OIDC authorization flow directly with Authelia,
// bypassing the MCP proxy. It stores a PKCE verifier keyed on the state parameter.
func webLoginHandler(issuerURL, clientID string, sessions *WebSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		verifier := randomBase64url(32)
		challenge := sha256Base64url(verifier)
		state := randomHex(16)

		sessions.StorePending(state, verifier)

		params := url.Values{}
		params.Set("response_type", "code")
		params.Set("client_id", clientID)
		params.Set("redirect_uri", webCallbackURL)
		params.Set("scope", "openid offline_access")
		params.Set("state", state)
		params.Set("code_challenge", challenge)
		params.Set("code_challenge_method", "S256")

		http.Redirect(w, r, issuerURL+"/api/oidc/authorization?"+params.Encode(), http.StatusFound)
	}
}

// webCallbackHandler receives the authorization code from Authelia, exchanges it
// for tokens directly (no proxy), verifies the token, looks up the user, creates
// a session, and sets an httpOnly cookie.
func webCallbackHandler(a *brain.App, issuerURL, clientID string, sessions *WebSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			log.Printf("webCallback: IdP error %q: %s", errParam, r.URL.Query().Get("error_description"))
			http.Error(w, fmt.Sprintf("auth error: %s", errParam), http.StatusBadRequest)
			return
		}

		state := r.URL.Query().Get("state")
		code := r.URL.Query().Get("code")
		if state == "" || code == "" {
			http.Error(w, "missing state or code", http.StatusBadRequest)
			return
		}

		verifier, ok := sessions.PopPending(state)
		if !ok {
			http.Error(w, "invalid or expired auth session", http.StatusBadRequest)
			return
		}

		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("code", code)
		form.Set("redirect_uri", webCallbackURL)
		form.Set("client_id", clientID)
		form.Set("code_verifier", verifier)

		tokenClient := &http.Client{Timeout: 10 * time.Second}
		resp, err := tokenClient.PostForm(issuerURL+"/api/oidc/token", form)
		if err != nil {
			http.Error(w, "token exchange failed", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, "token exchange failed", http.StatusBadGateway)
			return
		}

		var tokens struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil || tokens.AccessToken == "" {
			http.Error(w, "invalid token response", http.StatusBadGateway)
			return
		}

		subject, err := a.OIDC.Verify(r.Context(), tokens.AccessToken)
		if err != nil {
			http.Error(w, "token verification failed", http.StatusUnauthorized)
			return
		}

		var userID string
		if err := a.Pool.QueryRow(r.Context(),
			"SELECT id::text FROM mcp_users WHERE oidc_subject = $1", subject,
		).Scan(&userID); err != nil {
			http.Error(w, "unknown user", http.StatusForbidden)
			return
		}

		sessionID := sessions.Create(userID)
		http.SetCookie(w, &http.Cookie{
			Name:     "engram_session",
			Value:    sessionID,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL.Seconds()),
		})
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

// webLogoutHandler clears the session cookie and deletes the server-side session.
func webLogoutHandler(sessions *WebSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("engram_session"); err == nil {
			sessions.Delete(cookie.Value)
		}
		clearSessionCookie(w)
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "engram_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func randomBase64url(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand.Read: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha256Base64url(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand.Read: %v", err))
	}
	return hex.EncodeToString(b)
}
