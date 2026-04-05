// Relay server for Claudy.
//
// Endpoints:
//   POST /auth/register            — create account
//   POST /auth/login               → {"token":"..."}
//   GET  /tunnel?token=<jwt>       — daemon WebSocket (authenticated)
//   ANY  /dav/<username>/…         — WebDAV proxy (Basic Auth: username + password)
//   GET  /health                   — health check
//
// Environment variables:
//   PORT      — listen port (default 8080)
//   DATA_DIR  — directory for users.json and jwt_secret (default /var/lib/claudy)
package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/claudy-app/claudy-core/pkg/auth"
	"github.com/claudy-app/claudy-core/pkg/captcha"
	"github.com/claudy-app/claudy-core/pkg/email"
	"github.com/claudy-app/claudy-core/pkg/tunnel"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

//go:embed static/drive.html
var driveHTML []byte

// ---------------------------------------------------------------------------
// Tunnel registry
// ---------------------------------------------------------------------------

type conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	pending sync.Map // id → chan *tunnel.Response
}

func (c *conn) send(req *tunnel.Request) (<-chan *tunnel.Response, error) {
	ch := make(chan *tunnel.Response, 1)
	c.pending.Store(req.ID, ch)

	data, err := json.Marshal(req)
	if err != nil {
		c.pending.Delete(req.ID)
		return nil, err
	}
	c.writeMu.Lock()
	err = c.ws.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if err != nil {
		c.pending.Delete(req.ID)
		return nil, err
	}
	return ch, nil
}

func (c *conn) readLoop() {
	for {
		_, msg, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var resp tunnel.Response
		if json.Unmarshal(msg, &resp) != nil {
			continue
		}
		if v, ok := c.pending.LoadAndDelete(resp.ID); ok {
			v.(chan *tunnel.Response) <- &resp
		}
	}
}

var (
	registry   = map[string]*conn{}
	registryMu sync.RWMutex
	upgrader   = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	store      *auth.Store
	emailCfg   email.Config
	captchaCfg captcha.Config
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nfcRequestURI normalises the percent-encoded path component of a URL to
// NFC Unicode.  macOS sends paths in NFD (decomposed), Windows filesystems
// use NFC (precomposed) — without this Windows daemons return 404 for files
// whose names contain Cyrillic / accented characters.
func nfcRequestURI(r *http.Request) string {
	raw := r.URL.RequestURI() // e.g. /dav/user/%D0%B9%CC%86file.txt?q=1
	// Split off any query string.
	path, query, _ := strings.Cut(raw, "?")
	// Decode → NFC-normalise → re-encode only the non-ASCII bytes.
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return raw // fallback: use as-is
	}
	nfc := norm.NFC.String(decoded)
	encoded := strings.NewReplacer(" ", "%20").Replace(url.PathEscape(nfc))
	// url.PathEscape over-encodes slashes; restore them.
	encoded = strings.ReplaceAll(encoded, "%2F", "/")
	if query != "" {
		encoded += "?" + query
	}
	return encoded
}

func isDAVMethod(m string) bool {
	switch m {
	// OPTIONS is handled upstream (CORS wrapper + default 200); exclude it here
	// so Windows WebClient can discover DAV capabilities without credentials.
	case "PROPFIND", "PROPPATCH", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK",
		http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead:
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// ---------------------------------------------------------------------------
// Auth handlers
// ---------------------------------------------------------------------------

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Username     string `json:"username"`
		Password     string `json:"password"`
		Email        string `json:"email"`
		CaptchaToken string `json:"captcha_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON")
		return
	}

	// Verify CAPTCHA before doing any work.
	remoteIP := r.RemoteAddr
	if idx := strings.LastIndex(remoteIP, ":"); idx >= 0 {
		remoteIP = remoteIP[:idx]
	}
	if err := captcha.Verify(captchaCfg, body.CaptchaToken, remoteIP); err != nil {
		errJSON(w, http.StatusBadRequest, "CAPTCHA_FAILED", err.Error())
		return
	}

	token, err := store.Register(body.Username, body.Password, body.Email)
	if err != nil {
		errJSON(w, http.StatusConflict, "REGISTER_FAILED", err.Error())
		return
	}
	log.Printf("[auth] registered: %s <%s> — verify token: %s", body.Username, body.Email, token)

	if emailCfg.Enabled() {
		if err := email.SendVerification(emailCfg, body.Email, token); err != nil {
			log.Printf("[auth] WARNING: failed to send verification email to %s: %v", body.Email, err)
		} else {
			log.Printf("[auth] verification email sent to %s", body.Email)
		}
	} else {
		log.Printf("[auth] SMTP not configured — verification token printed above")
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"status":  "ok",
		"message": "check your email to verify your account",
	})
}

func handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"captcha_site_key": captchaCfg.SiteKey,
		"captcha_provider": string(captchaCfg.Provider),
	})
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		errJSON(w, http.StatusBadRequest, "BAD_REQUEST", "missing token")
		return
	}
	username, err := store.VerifyEmail(token)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "VERIFY_FAILED", err.Error())
		return
	}
	log.Printf("[auth] email verified: %s", username)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "email verified — you can now log in",
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON")
		return
	}
	token, err := store.Login(body.Username, body.Password)
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", err.Error())
		return
	}
	log.Printf("[auth] login: %s", body.Username)
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// ---------------------------------------------------------------------------
// Tunnel handler (daemon → relay)
// ---------------------------------------------------------------------------

func handleTunnel(w http.ResponseWriter, r *http.Request) {
	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		http.Error(w, `{"error":"missing token","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
		return
	}
	username, err := store.ValidateToken(tokenStr)
	if err != nil {
		http.Error(w, `{"error":"invalid token","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[tunnel] upgrade failed for %s: %v", username, err)
		return
	}

	c := &conn{ws: ws}
	registryMu.Lock()
	if old, ok := registry[username]; ok {
		old.ws.Close()
	}
	registry[username] = c
	registryMu.Unlock()

	log.Printf("[tunnel] daemon connected: user=%s remote=%s", username, r.RemoteAddr)
	c.readLoop()

	registryMu.Lock()
	if registry[username] == c {
		delete(registry, username)
	}
	registryMu.Unlock()
	ws.Close()
	log.Printf("[tunnel] daemon disconnected: user=%s", username)
}

// ---------------------------------------------------------------------------
// WebDAV proxy handler (client devices → relay → daemon)
// ---------------------------------------------------------------------------

func handleDAV(w http.ResponseWriter, r *http.Request) {
	// Windows WebClient probes /dav/ (no username) with PROPFIND — return collection listing.
	trimmed := strings.TrimPrefix(r.URL.Path, "/dav/")
	if trimmed == "" || trimmed == "/" {
		if r.Method == "PROPFIND" {
			registryMu.RLock()
			var entries string
			for u := range registry {
				entries += `<D:response><D:href>/dav/` + u + `/</D:href>` +
					`<D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype>` +
					`<D:displayname>` + u + `</D:displayname></D:prop>` +
					`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
			}
			registryMu.RUnlock()
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(207)
			w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>` +
				`<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/</D:href>` +
				`<D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop>` +
				`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
				entries + `</D:multistatus>`))
			return
		}
		http.NotFound(w, r)
		return
	}

	// macOS Finder requires Basic Auth — issue a challenge if missing.
	username, password, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Claudy"`)
		errJSON(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	// Validate credentials. Accept password as account password OR a valid JWT.
	authed := store.ValidatePassword(username, password)
	if !authed {
		// Also accept JWT as the password (useful for scripts / non-Finder clients).
		if u, err := store.ValidateToken(password); err == nil && u == username {
			authed = true
		}
	}
	if !authed {
		w.Header().Set("WWW-Authenticate", `Basic realm="Claudy"`)
		errJSON(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid username or password")
		return
	}

	// Path: /dav/<username>/rest…  — enforce that the authed user matches the path.
	slash := strings.IndexByte(trimmed, '/')
	var pathUser string
	if slash < 0 {
		pathUser = trimmed
	} else {
		pathUser = trimmed[:slash]
	}
	if pathUser != username {
		errJSON(w, http.StatusForbidden, "FORBIDDEN", "access denied")
		return
	}

	registryMu.RLock()
	c, online := registry[username]
	registryMu.RUnlock()
	if !online {
		errJSON(w, http.StatusServiceUnavailable, "SOURCE_OFFLINE", "your source device is offline")
		return
	}

	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}

	req := &tunnel.Request{
		ID:      uuid.NewString(),
		Method:  r.Method,
		Path:    nfcRequestURI(r), // NFC-normalised + URL-encoded path
		Headers: r.Header.Clone(),
		Body:    body,
	}

	ch, err := c.send(req)
	if err != nil {
		errJSON(w, http.StatusBadGateway, "TUNNEL_ERROR", "failed to reach source device")
		return
	}

	select {
	case resp := <-ch:
		log.Printf("[dav] %s %s → %d (body=%d bytes) user=%s",
			req.Method, req.Path, resp.Status, len(resp.Body), username)
		for k, vals := range resp.Headers {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		// golang.org/x/net/webdav does not emit DAV: header; add it so
		// macOS Finder recognises the endpoint as a WebDAV server.
		if r.Method == http.MethodOptions {
			w.Header().Set("DAV", "1, 2")
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
	case <-time.After(60 * time.Second):
		c.pending.Delete(req.ID)
		log.Printf("[dav] TIMEOUT %s %s user=%s", req.Method, req.Path, username)
		errJSON(w, http.StatusGatewayTimeout, "TIMEOUT", "source device did not respond in time")
	}
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	registryMu.RLock()
	n := len(registry)
	registryMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tunnels": n})
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/var/lib/claudy"
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatalf("[relay] cannot create data dir: %v", err)
	}

	// Load or generate JWT secret.
	secretPath := filepath.Join(dataDir, "jwt_secret")
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Fatalf("[relay] cannot generate JWT secret: %v", err)
		}
		if err := os.WriteFile(secretPath, secret, 0600); err != nil {
			log.Fatalf("[relay] cannot save JWT secret: %v", err)
		}
		log.Println("[relay] generated new JWT secret")
	}

	// Load user store.
	usersPath := filepath.Join(dataDir, "users.json")
	store, err = auth.NewStore(usersPath, secret)
	if err != nil {
		log.Fatalf("[relay] cannot load user store: %v", err)
	}
	log.Printf("[relay] data dir: %s", dataDir)

	// Load email config from environment.
	emailCfg = email.Config{
		Host:    os.Getenv("SMTP_HOST"),
		Port:    os.Getenv("SMTP_PORT"),
		User:    os.Getenv("SMTP_USER"),
		Pass:    os.Getenv("SMTP_PASS"),
		From:    os.Getenv("SMTP_FROM"),
		BaseURL: os.Getenv("BASE_URL"),
	}
	if emailCfg.Port == "" {
		emailCfg.Port = "587"
	}
	if emailCfg.BaseURL == "" {
		emailCfg.BaseURL = "http://localhost:" + port
	}
	if emailCfg.Enabled() {
		log.Printf("[relay] SMTP configured: %s:%s user=%s", emailCfg.Host, emailCfg.Port, emailCfg.User)
	} else {
		log.Printf("[relay] SMTP not configured — verify tokens will be logged to stdout")
	}

	// Load CAPTCHA config from environment.
	captchaProvider := captcha.Provider(os.Getenv("CAPTCHA_PROVIDER"))
	if captchaProvider == "" {
		captchaProvider = captcha.ProviderTurnstile
	}
	captchaCfg = captcha.Config{
		Provider:  captchaProvider,
		SiteKey:   os.Getenv("CAPTCHA_SITE_KEY"),
		SecretKey: os.Getenv("CAPTCHA_SECRET"),
	}
	if captchaCfg.Enabled() {
		log.Printf("[relay] CAPTCHA enabled: provider=%s", captchaCfg.Provider)
	} else {
		log.Printf("[relay] CAPTCHA disabled — set CAPTCHA_SITE_KEY and CAPTCHA_SECRET to enable")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/auth/register", handleRegister)
	mux.HandleFunc("/auth/login", handleLogin)
	mux.HandleFunc("/auth/verify", handleVerify)
	mux.HandleFunc("/tunnel", handleTunnel)
	mux.HandleFunc("/dav/", handleDAV)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// All WebDAV methods (including PROPFIND): authenticate and proxy to
		// /dav/<username>/ so Windows WebClient sees the user's files at the root.
		// If no credentials are provided, return a stub 207 for PROPFIND so that
		// Windows WebClient can probe capabilities before sending credentials.
		// Browser GET on root → serve the web UI.
		if r.Method == http.MethodGet && r.URL.Path == "/" &&
			!strings.Contains(r.Header.Get("User-Agent"), "WebDAV") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(driveHTML)
			return
		}

		if r.Method == "PROPFIND" || isDAVMethod(r.Method) {
			// Windows WebDAV MiniRedir does not send Basic credentials for localhost
			// even after a 401 challenge. When the request is from loopback (local machine
			// only), skip auth and proxy to the first online daemon. This is safe because
			// only local processes can reach 127.0.0.1 / ::1.
			// Remote clients (Mac, phone) use /dav/<username>/ with full auth instead.
			remoteHost := r.RemoteAddr
			if idx := strings.LastIndex(remoteHost, ":"); idx >= 0 {
				remoteHost = remoteHost[:idx]
			}
			remoteHost = strings.Trim(remoteHost, "[]")
			isLoopback := remoteHost == "127.0.0.1" || remoteHost == "::1"

			var proxyUsername string
			if isLoopback {
				// Pick the first online user.
				registryMu.RLock()
				for u := range registry {
					proxyUsername = u
					break
				}
				registryMu.RUnlock()
			} else {
				// Remote client: require auth.
				username, password, ok := r.BasicAuth()
				if !ok {
					w.Header().Set("WWW-Authenticate", `Basic realm="Claudy"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				authed := store.ValidatePassword(username, password)
				if !authed {
					if u, err := store.ValidateToken(password); err == nil && u == username {
						authed = true
					}
				}
				if !authed {
					w.Header().Set("WWW-Authenticate", `Basic realm="Claudy"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				proxyUsername = username
			}

			if proxyUsername == "" {
				http.Error(w, "no daemon online", http.StatusServiceUnavailable)
				return
			}

			// Rewrite path: /foo -> /dav/<username>/foo
			rest := strings.TrimPrefix(r.URL.Path, "/")
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/dav/" + proxyUsername + "/" + rest
			r2.RequestURI = r2.URL.RequestURI()
			// Inject a JWT so handleDAV passes its auth check without a password.
			if tok, err := store.IssueToken(proxyUsername); err == nil {
				r2.SetBasicAuth(proxyUsername, tok)
			}
			handleDAV(w, r2)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(driveHTML)
	})

	// CORS wrapper for browser-based file manager.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PROPFIND, MKCOL, MOVE, COPY, OPTIONS, LOCK, UNLOCK, PROPPATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Depth, Destination, Overwrite")
		w.Header().Set("Access-Control-Expose-Headers", "DAV, Content-Type, Allow")
		// CORS preflight (browser sends Access-Control-Request-Method, no auth).
		// Must return 204 immediately — never hit auth checks.
		// Plain WebDAV OPTIONS from Finder/clients (no ACRM header) goes to the handler
		// so the daemon can reply with DAV: 1,2 and Allow headers.
		// Always advertise DAV capability so Windows WebClient doesn't bail on error 67.
		w.Header().Set("DAV", "1, 2")
		w.Header().Set("MS-Author-Via", "DAV")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})

	log.Printf("[relay] starting on :%s", port)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("[relay] %v", err)
	}

	fmt.Println() // suppress unused import warning
}
