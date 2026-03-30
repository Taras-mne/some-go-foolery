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
	"github.com/claudy-app/claudy-core/pkg/tunnel"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

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
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON")
		return
	}
	if err := store.Register(body.Username, body.Password); err != nil {
		errJSON(w, http.StatusConflict, "REGISTER_FAILED", err.Error())
		return
	}
	log.Printf("[auth] registered: %s", body.Username)
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
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
	trimmed := strings.TrimPrefix(r.URL.Path, "/dav/")
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

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/register", handleRegister)
	mux.HandleFunc("/auth/login", handleLogin)
	mux.HandleFunc("/tunnel", handleTunnel)
	mux.HandleFunc("/dav/", handleDAV)
	mux.HandleFunc("/health", handleHealth)

	// CORS wrapper for browser-based file manager.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PROPFIND, MKCOL, MOVE, COPY, OPTIONS, LOCK, UNLOCK, PROPPATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Depth, Destination, Overwrite")
		w.Header().Set("Access-Control-Expose-Headers", "DAV, Content-Type, Allow")
		if r.Method == http.MethodOptions && r.URL.Path != "/tunnel" {
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
