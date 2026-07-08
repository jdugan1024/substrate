# Web Session Auth Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the client-side OAuth PKCE flow (tokens in localStorage) with server-side session cookies, using a dedicated web auth path that bypasses the MCP proxy entirely.

**Architecture:** A new `web_auth.go` file implements `WebSessionStore` (in-memory session map with 30-day TTL), `webAuthMiddleware` (validates `engram_session` httpOnly cookie), and web-specific OAuth handlers (`/web/login` → Authelia directly, `/web/callback` → creates session + sets cookie, `/web/logout`). The web pages drop all client-side token management and rely on cookies sent automatically with every request.

**Tech Stack:** Go stdlib (`net/http`, `crypto/sha256`, `crypto/rand`), existing `brain.App` + `brain.OIDCVerifier`, JavaScript `fetch` (no changes to auth logic — cookies are transparent).

---

## File Map

| Action | File | Responsibility |
|--------|------|----------------|
| Create | `web_auth.go` | Session store, PKCE helpers, web login/callback/logout/check handlers, webAuthMiddleware |
| Create | `web_auth_test.go` | Tests for session store and all web auth handlers |
| Modify | `main.go` | Instantiate `WebSessionStore`, add `/web/login`, `/web/callback`, `/web/logout` routes |
| Modify | `web.go` | Update `RegisterWebHandlers` signature, swap `authMiddleware` → `webAuthMiddleware`, add `/web/check` |
| Modify | `web/index.html` | Remove ~120 lines of client-side OAuth code; update login button and submit/init functions |
| Modify | `web/browse.html` | Remove ~30 lines of token helpers; update `fetchEntries` and `init` |

---

## Task 1: WebSessionStore and PKCE helpers

**Files:**
- Create: `web_auth.go`

- [ ] **Step 1: Create `web_auth.go` with session store and PKCE helpers**

```go
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
	"net/http"
	"net/url"
	"sync"
	"time"

	"open-brain-go/brain"
)

const (
	webCallbackURL = "https://engram.x1024.net/web/callback"
	sessionTTL     = 30 * 24 * time.Hour
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
		if time.Since(v.created) > 10*time.Minute {
			delete(s.pending, k)
		}
	}
	if !ok || time.Since(p.created) > 10*time.Minute {
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
			http.Error(w, fmt.Sprintf("auth error: %s", errParam), http.StatusBadRequest)
			return
		}

		state := r.URL.Query().Get("state")
		code := r.URL.Query().Get("code")

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

		resp, err := http.PostForm(issuerURL+"/api/oidc/token", form)
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
		MaxAge:   -1,
	})
}

func randomBase64url(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha256Base64url(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd /home/jdugan/engram && go build ./...
```

Expected: no output (clean build).

- [ ] **Step 3: Commit**

```bash
cd /home/jdugan/engram
git add web_auth.go
git commit -m "feat: add WebSessionStore and web auth handlers"
```

---

## Task 2: Tests for session store and web auth handlers

**Files:**
- Create: `web_auth_test.go`

- [ ] **Step 1: Create `web_auth_test.go`**

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"open-brain-go/brain"
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
	req.AddCookie(&http.Cookie{Name: "engram_session", Value: sessionID})
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
	req.AddCookie(&http.Cookie{Name: "engram_session", Value: "bogus-id"})
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
	req.AddCookie(&http.Cookie{Name: "engram_session", Value: sessionID})
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
	req.AddCookie(&http.Cookie{Name: "engram_session", Value: sessionID})
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
		if c.Name == "engram_session" && c.MaxAge < 0 {
			cookieCleared = true
		}
	}
	if !cookieCleared {
		t.Fatal("expected engram_session cookie to be cleared")
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
```

- [ ] **Step 2: Run the tests**

```bash
cd /home/jdugan/engram && go test ./... -run "TestWebSession|TestWebAuth|TestWebCheck|TestWebLogin|TestWebLogout|TestWebCallback|TestPKCE" -v
```

Expected: all 16 tests PASS.

- [ ] **Step 3: Commit**

```bash
cd /home/jdugan/engram
git add web_auth_test.go
git commit -m "test: add web session auth tests"
```

---

## Task 3: Wire up routes in `main.go` and `web.go`

**Files:**
- Modify: `main.go`
- Modify: `web.go`

- [ ] **Step 1: Update `main.go` to instantiate the session store and add web auth routes**

In `main.go`, add after the `workerCancel` / `workerCtx` block (around line 84) and before the MCP server setup:

```go
	sessionStore := NewWebSessionStore()
	sessionStore.StartCleanup(workerCtx)
```

Add these three routes in the `mux.HandleFunc` block (after line 105, before `RegisterWebHandlers`):

```go
	mux.HandleFunc("GET /web/login", webLoginHandler(issuerURL, clientID, sessionStore))
	mux.HandleFunc("GET /web/callback", webCallbackHandler(app, issuerURL, clientID, sessionStore))
	mux.HandleFunc("GET /web/logout", webLogoutHandler(sessionStore))
```

Change the `RegisterWebHandlers` call from:

```go
	RegisterWebHandlers(mux, app, es)
```

To:

```go
	RegisterWebHandlers(mux, app, es, sessionStore)
```

The variable used in `main.go` for the `*brain.App` is `app`. Verify it: `app, err := brain.New(...)` at line 60. Confirmed — use `app`.

- [ ] **Step 2: Update `web.go` to use `webAuthMiddleware` and add `/web/check`**

Change `RegisterWebHandlers` signature and body:

Old:
```go
func RegisterWebHandlers(mux *http.ServeMux, a *brain.App, es *service.EntryService) {
	mux.HandleFunc("/", serveWebUI())
	mux.Handle("POST /capture", authMiddleware(a, http.HandlerFunc(webCaptureHandler(a, es))))
	mux.HandleFunc("GET /browse", serveBrowseUI())
	mux.Handle("GET /entries", authMiddleware(a, http.HandlerFunc(listEntriesHandler(a))))
}
```

New:
```go
func RegisterWebHandlers(mux *http.ServeMux, a *brain.App, es *service.EntryService, sessions *WebSessionStore) {
	mux.HandleFunc("/", serveWebUI())
	mux.Handle("POST /capture", webAuthMiddleware(sessions, http.HandlerFunc(webCaptureHandler(a, es))))
	mux.HandleFunc("GET /browse", serveBrowseUI())
	mux.Handle("GET /entries", webAuthMiddleware(sessions, http.HandlerFunc(listEntriesHandler(a))))
	mux.HandleFunc("GET /web/check", webCheckHandler(sessions))
}
```

Remove the unused `brain` import from `web.go` if it's no longer referenced there — but check first: `brain.App` is still used via the `a *brain.App` parameter, so the import stays.

- [ ] **Step 3: Build to verify no compile errors**

```bash
cd /home/jdugan/engram && go build ./...
```

Expected: no output.

- [ ] **Step 4: Run all tests**

```bash
cd /home/jdugan/engram && go test ./...
```

Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/jdugan/engram
git add main.go web.go
git commit -m "feat: wire web session auth routes and swap middleware"
```

---

## Task 4: Update `web/index.html`

**Files:**
- Modify: `web/index.html`

The script section (lines 227–580) needs to be substantially simplified. The OAuth PKCE code (~120 lines) is replaced by ~5 lines that call `/web/check`.

- [ ] **Step 1: Change the login button (line 210)**

Old:
```html
    <button class="btn-login" id="login-btn" onclick="startLogin()">Log In</button>
```

New:
```html
    <button class="btn-login" id="login-btn" onclick="window.location.href='/web/login'">Log In</button>
```

- [ ] **Step 2: Replace the entire `<script>` block**

Remove everything from `<script>` (line 227) through the closing `</script>` (line 580) and replace with the following. The mic/speech recognition code, submit handler, keydown listener, and share-target handler are unchanged — only the auth-related functions change.

```html
<script>
// ── Auth (server-side session cookie, set by /web/login → /web/callback) ──────

function showAuthSection() {
  document.getElementById('auth-section').style.display = '';
  document.getElementById('capture-section').style.display = 'none';
}

function showCaptureSection() {
  document.getElementById('auth-section').style.display = 'none';
  document.getElementById('capture-section').style.display = '';
  document.getElementById('note-input').focus();
}

function showResult(type, message, tool) {
  const el = document.getElementById('result-area');
  el.className = 'result ' + type;
  el.style.display = 'block';
  if (tool && type === 'success') {
    const badge = document.createElement('span');
    badge.className = 'tool-badge';
    badge.textContent = tool.replace(/_/g, ' ');
    el.textContent = '';
    el.appendChild(badge);
    el.appendChild(document.createTextNode(message));
  } else {
    el.textContent = message;
  }
  if (type === 'success') {
    setTimeout(() => { el.style.display = 'none'; }, 4000);
  }
}

function showError(msg) {
  const el = document.getElementById('result-area');
  if (!el) return;
  el.className = 'result error';
  el.style.display = 'block';
  el.textContent = msg;
}

// ── Speech recognition ────────────────────────────────────────────────────────

function setupMic() {
  const SR = window.SpeechRecognition || window.webkitSpeechRecognition;
  if (!SR) return;

  const micBtn = document.getElementById('mic-btn');
  micBtn.style.display = 'inline-flex';

  let recog = null;
  let listening = false;
  let manualStop = false;
  let startedAt = 0;

  function hideMicBtn(reason) {
    micBtn.style.display = 'none';
    console.info('Mic button hidden:', reason);
  }

  function newRecog() {
    const r = new SR();
    r.continuous = false;
    r.interimResults = false;
    r.lang = 'en-US';

    r.onstart = () => {
      listening = true;
      startedAt = Date.now();
      micBtn.textContent = '⏹';
      micBtn.classList.add('recording');
      micBtn.setAttribute('aria-label', 'Stop recording');
    };

    r.onend = () => {
      listening = false;
      micBtn.textContent = '🎤';
      micBtn.classList.remove('recording');
      micBtn.setAttribute('aria-label', 'Dictate');
    };

    r.onresult = (e) => {
      let transcript = '';
      for (let i = 0; i < e.results.length; i++) {
        if (e.results[i].isFinal) transcript += e.results[i][0].transcript;
      }
      if (!transcript) return;
      const ta = document.getElementById('note-input');
      ta.value = ta.value ? ta.value + ' ' + transcript : transcript;
    };

    r.onerror = (e) => {
      listening = false;
      micBtn.textContent = '🎤';
      micBtn.classList.remove('recording');

      if (manualStop) { manualStop = false; return; }

      if (e.error === 'aborted' && Date.now() - startedAt < 500) {
        hideMicBtn('speech service unavailable');
        showResult('error', 'In-page dictation not supported here — use system dictation instead (macOS: press Fn twice).');
        return;
      }

      const friendly = {
        'not-allowed':         'Microphone permission denied — check browser settings.',
        'audio-capture':       'No microphone found.',
        'network':             'Speech recognition requires a network connection.',
        'no-speech':           'No speech detected — try again.',
        'service-not-allowed': 'Speech recognition not available on this browser.',
      };
      showResult('error', friendly[e.error] || 'Speech error: ' + e.error);
    };

    return r;
  }

  micBtn.addEventListener('click', () => {
    if (listening) {
      manualStop = true;
      recog && recog.stop();
      return;
    }
    recog = newRecog();
    recog.start();
  });
}

// ── Submit ────────────────────────────────────────────────────────────────────

async function submitNote() {
  const textarea = document.getElementById('note-input');
  const text = textarea.value.trim();
  if (!text) return;

  const btn = document.getElementById('submit-btn');
  btn.disabled = true;
  btn.textContent = 'Saving…';

  document.getElementById('result-area').style.display = 'none';

  try {
    const resp = await fetch('/capture', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text }),
    });

    if (resp.status === 401) { showAuthSection(); return; }

    const data = await resp.json();
    if (!resp.ok) {
      showResult('error', data.error || 'Something went wrong.');
      return;
    }

    showResult('success', data.message, data.tool);
    textarea.value = '';
  } catch (e) {
    showResult('error', 'Network error — please try again.');
  } finally {
    btn.disabled = false;
    btn.textContent = 'Save';
  }
}

// Submit on Ctrl/Cmd+Enter
document.addEventListener('keydown', (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
    e.preventDefault();
    submitNote();
  }
});

// ── PWA Share target ──────────────────────────────────────────────────────────

function maybePreFillShare() {
  const params = new URLSearchParams(window.location.search);
  const shareURL = params.get('share_url');
  if (!shareURL) return;

  const shareTitle = params.get('share_title');
  const textarea = document.getElementById('note-input');
  textarea.value = 'link:' + shareURL + (shareTitle ? '  ' + shareTitle : '');
  window.history.replaceState({}, '', '/');
}

// ── Init ──────────────────────────────────────────────────────────────────────

async function init() {
  const resp = await fetch('/web/check');
  if (resp.status === 401) {
    showAuthSection();
    return;
  }
  showCaptureSection();
  maybePreFillShare();
}

document.addEventListener('DOMContentLoaded', () => {
  setupMic();
  init();
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js');
  }
});
</script>
```

- [ ] **Step 3: Build (embeds web/index.html at compile time)**

```bash
cd /home/jdugan/engram && go build ./...
```

Expected: no output.

- [ ] **Step 4: Commit**

```bash
cd /home/jdugan/engram
git add web/index.html
git commit -m "feat: replace client-side OAuth in index.html with server-side session"
```

---

## Task 5: Update `web/browse.html`

**Files:**
- Modify: `web/browse.html`

The token helpers in browse.html (lines 274–301) and the Bearer token headers in `fetchEntries` and `init` need to be removed.

- [ ] **Step 1: Remove token helpers (lines 272–301) and replace with a comment**

Old block (lines 272–301):
```javascript
// ── Token helpers (mirrors capture page logic) ────────────────────────────────

function clearTokens() {
  ['engram_access_token', 'engram_refresh_token', 'engram_token_expires_at']
    .forEach(k => localStorage.removeItem(k));
}

async function refreshAccessToken() {
  const rt = localStorage.getItem('engram_refresh_token');
  if (!rt) { clearTokens(); return null; }
  const resp = await fetch('/oauth/token', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ grant_type: 'refresh_token', refresh_token: rt, client_id: 'engram' }),
  });
  if (!resp.ok) { clearTokens(); return null; }
  const tokens = await resp.json();
  localStorage.setItem('engram_access_token', tokens.access_token);
  if (tokens.refresh_token) localStorage.setItem('engram_refresh_token', tokens.refresh_token);
  localStorage.setItem('engram_token_expires_at', String(Date.now() + (tokens.expires_in || 3600) * 1000));
  return tokens.access_token;
}

async function getValidToken() {
  const token = localStorage.getItem('engram_access_token');
  const exp = parseInt(localStorage.getItem('engram_token_expires_at') || '0');
  if (!token) return null;
  if (Date.now() >= exp - 60_000) return await refreshAccessToken();
  return token;
}
```

Replace with nothing (delete the entire block).

- [ ] **Step 2: Update `fetchEntries` to remove Bearer token auth**

Old (around line 394):
```javascript
async function fetchEntries(isInitial) {
  if (loading) return;
  loading = true;

  const token = await getValidToken();
  if (!token) { loading = false; clearTokens(); window.location.href = '/'; return; }

  const params = new URLSearchParams({ limit: '50', offset: String(offset) });
  if (currentQ) params.set('q', currentQ);
  if (currentType) params.set('type', currentType);

  let resp;
  try {
    resp = await fetch('/entries?' + params, {
      headers: { 'Authorization': 'Bearer ' + token },
    });
  } catch {
    loading = false;
    if (isInitial) showInitialError();
    else showLoadMoreError();
    return;
  }

  if (resp.status === 401) { loading = false; clearTokens(); window.location.href = '/'; return; }
```

New:
```javascript
async function fetchEntries(isInitial) {
  if (loading) return;
  loading = true;

  const params = new URLSearchParams({ limit: '50', offset: String(offset) });
  if (currentQ) params.set('q', currentQ);
  if (currentType) params.set('type', currentType);

  let resp;
  try {
    resp = await fetch('/entries?' + params);
  } catch {
    loading = false;
    if (isInitial) showInitialError();
    else showLoadMoreError();
    return;
  }

  if (resp.status === 401) { loading = false; window.location.href = '/web/login'; return; }
```

- [ ] **Step 3: Update `init` in browse.html**

Old (around line 498):
```javascript
async function init() {
  const token = await getValidToken();
  if (!token) { window.location.href = '/'; return; }
  setupObserver();
  fetchEntries(true);
}
```

New:
```javascript
async function init() {
  const resp = await fetch('/web/check');
  if (resp.status === 401) { window.location.href = '/web/login'; return; }
  setupObserver();
  fetchEntries(true);
}
```

- [ ] **Step 4: Build**

```bash
cd /home/jdugan/engram && go build ./...
```

Expected: no output.

- [ ] **Step 5: Run all tests**

```bash
cd /home/jdugan/engram && go test ./...
```

Expected: all tests PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/jdugan/engram
git add web/browse.html
git commit -m "feat: replace client-side OAuth in browse.html with server-side session"
```

---

## Self-Review

**Spec coverage:**

| Requirement | Covered by |
|-------------|-----------|
| Unexpected logouts — access token expiry in background | Session TTL is 30 days, independent of Authelia token TTL. No logout unless session expires or user logs out. |
| Unexpected logouts — refresh only on API call | Background cleanup goroutine; session TTL doesn't depend on Authelia refresh token at all. |
| Web auth bypasses MCP proxy | `/web/login` goes directly to `issuerURL+"/api/oidc/authorization"`, `/web/callback` calls token endpoint directly. `pendingAuths` map in main.go is untouched for MCP. |
| httpOnly session cookie | Set in `webCallbackHandler` with `HttpOnly: true, Secure: true, SameSite: Lax, MaxAge: 30 days`. |
| MCP endpoints unaffected | `authMiddleware` (Bearer token) still used for `/mcp` and `/mcp/`. `RegisterWebHandlers` swap is isolated. |
| Token removed from localStorage | All `localStorage.setItem('engram_access_token', ...)` and related calls deleted from both HTML files. |

**Placeholder scan:** No TBD, TODO, or "similar to Task N" patterns. All code blocks are complete.

**Type consistency:**
- `WebSessionStore` — defined in Task 1, used in Tasks 3, 4, 5 via `sessions *WebSessionStore`
- `NewWebSessionStore()` — defined in Task 1, called in Task 3 (`main.go`)
- `webAuthMiddleware(sessions, next)` — defined in Task 1, used in Task 3 (`web.go`)
- `webCheckHandler(sessions)` — defined in Task 1, used in Task 3 (`web.go`)
- `webLoginHandler(issuerURL, clientID, sessions)` — defined in Task 1, used in Task 3 (`main.go`)
- `webCallbackHandler(app, issuerURL, clientID, sessions)` — defined in Task 1, parameter name `app` matches `main.go`'s variable name
- `webLogoutHandler(sessions)` — defined in Task 1, used in Task 3 (`main.go`)
- `webCallbackURL` const — defined in Task 1, referenced in test (Task 2) and used inside `webLoginHandler` and `webCallbackHandler`
- `pendingWebLogin` struct — defined in Task 1, accessed directly in tests via `store.pending[...]`

All consistent. ✓
