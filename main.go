// ABOUTME: Open Brain MCP server entry point.
// ABOUTME: Wires shared infrastructure, core tools, and extensions into a single HTTP server.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"open-brain-go/brain"
	"open-brain-go/brain/service"
	"open-brain-go/core"
	"open-brain-go/extensions/calendar"
	"open-brain-go/extensions/household"
	"open-brain-go/extensions/meals"
)

// pendingAuth tracks the original redirect_uri from an MCP client so the
// OAuth callback can forward the auth code to the correct localhost listener.
type pendingAuth struct {
	redirectURI string
	created     time.Time
}

var (
	pendingAuths   = map[string]pendingAuth{}
	pendingAuthsMu sync.Mutex
)

const callbackURL = "https://engram.x1024.net/oauth/callback"

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	openRouterKey := os.Getenv("OPENROUTER_API_KEY")
	if openRouterKey == "" {
		log.Fatal("OPENROUTER_API_KEY is required")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx := context.Background()

	app, err := brain.New(ctx, dbURL, openRouterKey)
	if err != nil {
		log.Fatalf("init app: %v", err)
	}
	defer app.Pool.Close()
	log.Println("Connected to database")

	issuerURL := os.Getenv("AUTHELIA_ISSUER_URL")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	if issuerURL == "" || clientID == "" {
		log.Fatal("AUTHELIA_ISSUER_URL and OIDC_CLIENT_ID are required")
	}
	oidcVerifier, err := brain.NewOIDCVerifier(ctx, issuerURL, clientID)
	if err != nil {
		log.Fatalf("init oidc: %v", err)
	}
	app.OIDC = oidcVerifier
	log.Println("OIDC verifier initialized")

	es := service.NewEntryService(app)

	// Start background enrichment worker — retries failed link fetches and
	// extracts full-text for richer semantic embeddings.
	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	go brain.NewEnrichmentWorker(app).Run(workerCtx)

	sessionStore := NewWebSessionStore()
	sessionStore.StartCleanup(workerCtx)

	s := server.NewMCPServer("open-brain", "1.0.0")

	core.Register(s, app)
	core.RegisterAddItem(s, app, es)
	core.RegisterSearch(s, app)
	household.Register(s, app)
	calendar.Register(s, app)
	meals.Register(s, app)

	mcpHandler := server.NewStreamableHTTPServer(s)

	mux := http.NewServeMux()
	mux.Handle("/mcp", authMiddleware(app, mcpHandler))
	mux.Handle("/mcp/", authMiddleware(app, mcpHandler))
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", oauthMetadataHandler(issuerURL, clientID))
	mux.HandleFunc("POST /oauth/register", clientRegistrationHandler(clientID))
	mux.HandleFunc("GET /oauth/authorize", oauthAuthorizeHandler(issuerURL, clientID))
	mux.HandleFunc("GET /oauth/callback", oauthCallbackHandler())
	mux.HandleFunc("POST /oauth/token", oauthTokenHandler(issuerURL, clientID))
	mux.HandleFunc("GET /web/login", webLoginHandler(issuerURL, clientID, sessionStore))
	mux.HandleFunc("GET /web/callback", webCallbackHandler(app, issuerURL, clientID, sessionStore))
	mux.HandleFunc("GET /web/logout", webLogoutHandler(sessionStore))
	mux.HandleFunc("GET /web/check", webCheckHandler(sessionStore))
	RegisterWebHandlers(mux, app, es, sessionStore)
	RegisterPWAHandlers(mux)

	log.Printf("Open Brain MCP server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// authMiddleware validates an OIDC Bearer token and resolves the caller's user ID
// for RLS-scoped DB transactions.
func authMiddleware(a *brain.App, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, `{"error":"missing bearer token"}`, http.StatusUnauthorized)
			return
		}
		rawToken := strings.TrimPrefix(auth, "Bearer ")

		subject, err := a.OIDC.Verify(r.Context(), rawToken)
		if err != nil {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}

		var userID string
		err = a.Pool.QueryRow(r.Context(),
			"SELECT id::text FROM mcp_users WHERE oidc_subject = $1", subject,
		).Scan(&userID)
		if err != nil {
			http.Error(w, `{"error":"unknown user"}`, http.StatusForbidden)
			return
		}

		ctx := context.WithValue(r.Context(), brain.CtxUserID, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// oauthMetadataHandler serves RFC 8414 OAuth authorization server metadata.
// All OAuth endpoints point to engram, which proxies to Authelia. This lets
// engram handle redirect_uri translation for localhost MCP clients.
func oauthMetadataHandler(issuerURL, clientID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           "https://engram.x1024.net",
			"authorization_endpoint":           "https://engram.x1024.net/oauth/authorize",
			"token_endpoint":                   "https://engram.x1024.net/oauth/token",
			"registration_endpoint":            "https://engram.x1024.net/oauth/register",
			"scopes_supported":                 []string{"openid", "profile", "email", "offline_access"},
			"response_types_supported":         []string{"code"},
			"grant_types_supported":            []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	}
}

// clientRegistrationHandler implements RFC 7591 dynamic client registration
// by returning the pre-configured OIDC client credentials.
func clientRegistrationHandler(clientID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
			ClientName   string   `json:"client_name"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		redirectURIs := req.RedirectURIs
		if len(redirectURIs) == 0 {
			redirectURIs = []string{callbackURL}
		}

		log.Printf("OAuth client registration: redirect_uris=%v", redirectURIs)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"client_id":                  clientID,
			"client_name":                "Engram MCP Server",
			"redirect_uris":              redirectURIs,
			"grant_types":                []string{"authorization_code", "refresh_token"},
			"response_types":             []string{"code"},
			"token_endpoint_auth_method": "none",
		})
	}
}

// oauthAuthorizeHandler proxies the authorization request to Authelia.
// It saves the MCP client's original redirect_uri (e.g. http://localhost:PORT/callback)
// and substitutes engram's own callback URL, which Authelia has pre-registered.
func oauthAuthorizeHandler(issuerURL, clientID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		originalRedirect := r.URL.Query().Get("redirect_uri")
		if originalRedirect == "" {
			http.Error(w, `{"error":"redirect_uri required"}`, http.StatusBadRequest)
			return
		}

		// Generate a mapping key and store the original redirect_uri.
		// We embed this in Authelia's state parameter so we can recover it on callback.
		b := make([]byte, 16)
		rand.Read(b)
		mappingKey := hex.EncodeToString(b)

		pendingAuthsMu.Lock()
		pendingAuths[mappingKey] = pendingAuth{
			redirectURI: originalRedirect,
			created:     time.Now(),
		}
		pendingAuthsMu.Unlock()

		// Wrap the client's original state with our mapping key.
		clientState := r.URL.Query().Get("state")
		wrappedState := mappingKey + ":" + clientState

		// Build the Authelia authorization URL with engram's fixed callback.
		params := url.Values{}
		params.Set("response_type", "code")
		params.Set("client_id", clientID)
		params.Set("redirect_uri", callbackURL)
		params.Set("state", wrappedState)
		if scope := r.URL.Query().Get("scope"); scope != "" {
			params.Set("scope", scope)
		}
		if challenge := r.URL.Query().Get("code_challenge"); challenge != "" {
			params.Set("code_challenge", challenge)
			params.Set("code_challenge_method", r.URL.Query().Get("code_challenge_method"))
		}

		target := issuerURL + "/api/oidc/authorization?" + params.Encode()
		log.Printf("OAuth authorize: redirecting to Authelia, mapping_key=%s original_redirect=%s", mappingKey, originalRedirect)
		http.Redirect(w, r, target, http.StatusFound)
	}
}

// oauthCallbackHandler receives the authorization code from Authelia and
// forwards it to the MCP client's original localhost redirect_uri.
func oauthCallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Unwrap the state to recover mapping key and original client state.
		wrappedState := r.URL.Query().Get("state")
		parts := strings.SplitN(wrappedState, ":", 2)
		if len(parts) != 2 {
			http.Error(w, `{"error":"invalid state"}`, http.StatusBadRequest)
			return
		}
		mappingKey := parts[0]
		clientState := parts[1]

		pendingAuthsMu.Lock()
		pending, ok := pendingAuths[mappingKey]
		delete(pendingAuths, mappingKey)
		pendingAuthsMu.Unlock()

		if !ok || time.Since(pending.created) > 10*time.Minute {
			http.Error(w, `{"error":"expired or unknown auth session"}`, http.StatusBadRequest)
			return
		}

		// Forward the code (and any error) to the MCP client's original redirect.
		target, err := url.Parse(pending.redirectURI)
		if err != nil {
			http.Error(w, `{"error":"bad stored redirect_uri"}`, http.StatusInternalServerError)
			return
		}
		q := target.Query()
		if code := r.URL.Query().Get("code"); code != "" {
			q.Set("code", code)
		}
		q.Set("state", clientState)
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			q.Set("error", errParam)
			q.Set("error_description", r.URL.Query().Get("error_description"))
		}
		target.RawQuery = q.Encode()

		log.Printf("OAuth callback: forwarding code to %s", target.String())
		http.Redirect(w, r, target.String(), http.StatusFound)
	}
}

// oauthTokenHandler proxies token requests to Authelia, replacing the
// MCP client's localhost redirect_uri with engram's registered callback URL.
func oauthTokenHandler(issuerURL, clientID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}

		// Build the upstream request with engram's registered redirect_uri.
		form := url.Values{}
		for k, vs := range r.Form {
			for _, v := range vs {
				if k == "redirect_uri" {
					// Swap localhost redirect to engram's registered callback.
					form.Set(k, callbackURL)
				} else {
					form.Add(k, v)
				}
			}
		}
		// Ensure client_id is set (public client, no secret).
		if form.Get("client_id") == "" {
			form.Set("client_id", clientID)
		}

		upstream := issuerURL + "/api/oidc/token"
		resp, err := http.Post(upstream, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		if err != nil {
			log.Printf("OAuth token proxy error: %v", err)
			http.Error(w, `{"error":"upstream error"}`, http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Forward the response as-is.
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}
