package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"substrate/brain"
)

func TestWebSessionStore_CreateGetDelete(t *testing.T) {
	store := NewWebSessionStore()

	id := store.Create("user-123")
	if id == "" {
		t.Fatal("expected non-empty session ID")
	}

	userID, ok := store.Get(id)
	if !ok || userID != "user-123" {
		t.Fatalf("expected user-123, got %q (ok=%v)", userID, ok)
	}

	store.Delete(id)
	_, ok = store.Get(id)
	if ok {
		t.Fatal("expected session to be deleted")
	}
}

func TestWebSessionStore_Expiry(t *testing.T) {
	store := NewWebSessionStore()
	id := store.Create("user-456")

	store.mu.Lock()
	sess := store.sessions[id]
	sess.expiresAt = time.Now().Add(-time.Second)
	store.sessions[id] = sess
	store.mu.Unlock()

	_, ok := store.Get(id)
	if ok {
		t.Fatal("expected expired session to be rejected")
	}
}

func TestWebSessionStore_StorePending_PopPending(t *testing.T) {
	store := NewWebSessionStore()
	store.StorePending("state-abc", "verifier-xyz")

	verifier, ok := store.PopPending("state-abc")
	if !ok || verifier != "verifier-xyz" {
		t.Fatalf("expected verifier-xyz, got %q (ok=%v)", verifier, ok)
	}

	// Second pop returns false (already consumed).
	_, ok = store.PopPending("state-abc")
	if ok {
		t.Fatal("expected second pop to return false")
	}
}

func TestWebSessionStore_PopPending_Unknown(t *testing.T) {
	store := NewWebSessionStore()
	_, ok := store.PopPending("nonexistent")
	if ok {
		t.Fatal("expected false for unknown state")
	}
}

func TestWebSessionStore_PopPending_Expired(t *testing.T) {
	store := NewWebSessionStore()
	store.StorePending("old-state", "old-verifier")

	store.mu.Lock()
	store.pending["old-state"] = pendingWebLogin{
		verifier: "old-verifier",
		created:  time.Now().Add(-11 * time.Minute),
	}
	store.mu.Unlock()

	_, ok := store.PopPending("old-state")
	if ok {
		t.Fatal("expected expired pending to be rejected")
	}
}

func TestWebAuthMiddleware_NoCookie(t *testing.T) {
	store := NewWebSessionStore()
	handler := webAuthMiddleware(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/entries", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebAuthMiddleware_ValidSession(t *testing.T) {
	store := NewWebSessionStore()
	sessionID := store.Create("user-789")

	handler := webAuthMiddleware(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(brain.CtxUserID).(string)
		if userID != "user-789" {
			t.Errorf("expected user-789 in context, got %q", userID)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/entries", nil)
	req.AddCookie(&http.Cookie{Name: "substrate_session", Value: sessionID})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestWebAuthMiddleware_InvalidSession(t *testing.T) {
	store := NewWebSessionStore()
	handler := webAuthMiddleware(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/entries", nil)
	req.AddCookie(&http.Cookie{Name: "substrate_session", Value: "bogus-id"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebCheckHandler_Authenticated(t *testing.T) {
	store := NewWebSessionStore()
	sessionID := store.Create("user-abc")

	req := httptest.NewRequest("GET", "/web/check", nil)
	req.AddCookie(&http.Cookie{Name: "substrate_session", Value: sessionID})
	rec := httptest.NewRecorder()
	webCheckHandler(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestWebCheckHandler_Unauthenticated(t *testing.T) {
	store := NewWebSessionStore()
	req := httptest.NewRequest("GET", "/web/check", nil)
	rec := httptest.NewRecorder()
	webCheckHandler(store)(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebLoginHandler_Redirect(t *testing.T) {
	store := NewWebSessionStore()
	handler := webLoginHandler("https://auth.example.com", "myclient", store)

	req := httptest.NewRequest("GET", "/web/login", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("expected Location header")
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("invalid redirect URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("client_id") != "myclient" {
		t.Errorf("expected client_id=myclient, got %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != webCallbackURL {
		t.Errorf("expected redirect_uri=%s, got %q", webCallbackURL, q.Get("redirect_uri"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("expected S256, got %q", q.Get("code_challenge_method"))
	}
	if q.Get("state") == "" {
		t.Error("expected non-empty state")
	}
	if q.Get("code_challenge") == "" {
		t.Error("expected non-empty code_challenge")
	}
}

func TestWebLogoutHandler(t *testing.T) {
	store := NewWebSessionStore()
	sessionID := store.Create("user-def")

	req := httptest.NewRequest("GET", "/web/logout", nil)
	req.AddCookie(&http.Cookie{Name: "substrate_session", Value: sessionID})
	rec := httptest.NewRecorder()
	webLogoutHandler(store)(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if _, ok := store.Get(sessionID); ok {
		t.Fatal("expected session to be deleted after logout")
	}
	var cookieCleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "substrate_session" && c.MaxAge < 0 {
			cookieCleared = true
		}
	}
	if !cookieCleared {
		t.Fatal("expected substrate_session cookie to be cleared")
	}
}

func TestWebCallbackHandler_InvalidState(t *testing.T) {
	store := NewWebSessionStore()
	handler := webCallbackHandler(nil, "https://auth.example.com", "myclient", store)

	req := httptest.NewRequest("GET", "/web/callback?code=abc&state=badstate", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestWebCallbackHandler_AuthError(t *testing.T) {
	store := NewWebSessionStore()
	handler := webCallbackHandler(nil, "https://auth.example.com", "myclient", store)

	req := httptest.NewRequest("GET", "/web/callback?error=access_denied", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestPKCEHelpers(t *testing.T) {
	verifier := randomBase64url(32)
	if verifier == "" {
		t.Fatal("expected non-empty verifier")
	}
	challenge := sha256Base64url(verifier)
	if challenge == "" {
		t.Fatal("expected non-empty challenge")
	}
	if challenge == verifier {
		t.Fatal("challenge must differ from verifier")
	}
}
