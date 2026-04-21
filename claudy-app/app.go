package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/net/webdav"

	"github.com/gorilla/websocket"
)

// -----------------------------------------------------------------------
// App struct
// -----------------------------------------------------------------------

type App struct {
	ctx    context.Context
	config *Config

	// daemon state
	mu        sync.RWMutex
	connected bool
	running   bool

	// quit channel — closed when user quits
	quit chan struct{}

	// tray status callbacks (set by main.go)
	OnStatus func(connected bool)
}

// -----------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------

type Config struct {
	RelayURL string `json:"relay_url"`
	Username string `json:"username"`
	Password string `json:"-"`
	ShareDir string `json:"share_dir"`
}

// DaemonStatus returned to frontend
type DaemonStatus struct {
	Running   bool   `json:"running"`
	Connected bool   `json:"connected"`
	Username  string `json:"username"`
	ShareDir  string `json:"share_dir"`
	RelayURL  string `json:"relay_url"`
}

func NewApp() *App {
	return &App{
		quit: make(chan struct{}),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// Kill any other daemon holding the lock (e.g. a stray claudy-core/daemon)
	acquireLock()

	a.config = loadConfig()

	// Start daemon goroutine if config is ready
	if a.config.Username != "" && a.config.Password != "" && a.config.ShareDir != "" {
		go a.runDaemon()
	}
}

func (a *App) shutdown(ctx context.Context) {
	select {
	case <-a.quit:
	default:
		close(a.quit)
	}
	releaseLock()
}

// -----------------------------------------------------------------------
// Config helpers
// -----------------------------------------------------------------------

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claudy", "config.json")
}

func loadConfig() *Config {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return &Config{RelayURL: "http://23.172.217.149"}
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return &Config{RelayURL: "http://23.172.217.149"}
	}

	// Migrate: old configs stored the password as "password" in JSON.
	// If present, move it into the keyring and rewrite the file without it.
	var legacy struct {
		Password string `json:"password"`
	}
	_ = json.Unmarshal(data, &legacy)
	if legacy.Password != "" && c.Username != "" {
		if err := savePassword(c.Username, legacy.Password); err == nil {
			c.Password = legacy.Password
			clean, _ := json.MarshalIndent(&c, "", "  ")
			_ = os.WriteFile(configPath(), clean, 0600)
		} else {
			c.Password = legacy.Password
		}
	} else {
		c.Password = loadPassword(c.Username)
	}

	return &c
}

func (a *App) saveConfig() error {
	path := configPath()
	os.MkdirAll(filepath.Dir(path), 0700)
	data, _ := json.MarshalIndent(a.config, "", "  ")
	return os.WriteFile(path, data, 0600)
}

// -----------------------------------------------------------------------
// Daemon — WebDAV + WebSocket tunnel (runs as goroutine inside the app)
// -----------------------------------------------------------------------

func (a *App) runDaemon() {
	for {
		a.setStatus(false, false)

		// Login
		token, err := apiLogin(a.config.RelayURL, a.config.Username, a.config.Password)
		if err != nil {
			select {
			case <-a.quit:
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		wsURL, err := buildWSURL(a.config.RelayURL, token)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		davPrefix := "/dav/" + a.config.Username
		dav := &dynamicDAV{}
		dav.setDir(davPrefix, a.config.ShareDir)

		a.setStatus(true, false)
		a.connectLoop(wsURL, dav)
	}
}

func (a *App) setStatus(running, connected bool) {
	a.mu.Lock()
	a.running = running
	a.connected = connected
	a.mu.Unlock()
	if a.OnStatus != nil {
		a.OnStatus(connected)
	}
}

func (a *App) connectLoop(wsURL string, dav http.Handler) {
	backoff := time.Second
	for {
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			a.setStatus(true, false)
			select {
			case <-time.After(backoff):
			case <-a.quit:
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		a.setStatus(true, true)
		handleConnection(ws, dav)
		a.setStatus(true, false)
		select {
		case <-time.After(time.Second):
		case <-a.quit:
			return
		}
	}
}

// restartDaemon — called after login / folder change
func (a *App) restartDaemon() {
	// signal quit to old loop
	select {
	case <-a.quit:
	default:
		close(a.quit)
	}
	a.quit = make(chan struct{})
	go a.runDaemon()
}

// -----------------------------------------------------------------------
// WebDAV helpers
// -----------------------------------------------------------------------

type dynamicDAV struct {
	mu sync.RWMutex
	h  *webdav.Handler
}

func (d *dynamicDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	h := d.h
	d.mu.RUnlock()
	h.ServeHTTP(w, r)
}

func (d *dynamicDAV) setDir(prefix, dir string) {
	d.mu.Lock()
	d.h = &webdav.Handler{
		Prefix:     prefix,
		FileSystem: webdav.Dir(dir),
		LockSystem: webdav.NewMemLS(),
	}
	d.mu.Unlock()
}

func handleConnection(ws *websocket.Conn, dav http.Handler) {
	type Request struct {
		ID      string              `json:"id"`
		Method  string              `json:"method"`
		Path    string              `json:"path"`
		Headers map[string][]string `json:"headers"`
		Body    []byte              `json:"body"`
	}
	type Response struct {
		ID      string              `json:"id"`
		Status  int                 `json:"status"`
		Headers map[string][]string `json:"headers"`
		Body    []byte              `json:"body"`
	}
	var writeMu sync.Mutex
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var req Request
		if json.Unmarshal(msg, &req) != nil {
			continue
		}
		go func() {
			httpReq := httptest.NewRequest(req.Method, req.Path, bytes.NewReader(req.Body))
			for k, vals := range req.Headers {
				for _, v := range vals {
					httpReq.Header.Add(k, v)
				}
			}
			rec := httptest.NewRecorder()
			dav.ServeHTTP(rec, httpReq)
			result := rec.Result()
			body, _ := io.ReadAll(result.Body)
			resp := Response{ID: req.ID, Status: result.StatusCode, Headers: result.Header, Body: body}
			data, _ := json.Marshal(resp)
			writeMu.Lock()
			_ = ws.WriteMessage(websocket.TextMessage, data)
			writeMu.Unlock()
		}()
	}
}

// -----------------------------------------------------------------------
// REST helpers
// -----------------------------------------------------------------------

func apiLogin(relayURL, username, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := http.Post(relayURL+"/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return "", fmt.Errorf("%s", e["error"])
	}
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	return result["token"], nil
}

func buildWSURL(relayURL, token string) (string, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = "/tunnel"
	q := url.Values{}
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// -----------------------------------------------------------------------
// Exported methods (callable from JS)
// -----------------------------------------------------------------------

func (a *App) GetConfig() *Config {
	safe := *a.config
	safe.Password = ""
	return &safe
}

func (a *App) GetStatus() DaemonStatus {
	a.mu.RLock()
	running := a.running
	connected := a.connected
	a.mu.RUnlock()
	return DaemonStatus{
		Running:   running,
		Connected: connected,
		Username:  a.config.Username,
		ShareDir:  a.config.ShareDir,
		RelayURL:  a.config.RelayURL,
	}
}

func (a *App) Login(relayURL, username, password string) (string, error) {
	relayURL = strings.TrimRight(relayURL, "/")
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := http.Post(relayURL+"/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("cannot reach server: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return "", fmt.Errorf("%s", e["error"])
	}
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)

	a.config.RelayURL = relayURL
	a.config.Username = username
	a.config.Password = password
	a.saveConfig()
	if err := savePassword(username, password); err != nil {
		return "", fmt.Errorf("cannot save credentials to keyring: %v", err)
	}
	a.restartDaemon()
	return result["token"], nil
}

func (a *App) Register(relayURL, username, password string) error {
	relayURL = strings.TrimRight(relayURL, "/")
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := http.Post(relayURL+"/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cannot reach server: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s", e["error"])
	}
	return nil
}

func (a *App) PickFolder() string {
	dir, err := wailsruntime.OpenDirectoryDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Choose a folder to share with Claudy",
	})
	if err != nil || dir == "" {
		return ""
	}
	a.config.ShareDir = dir
	a.saveConfig()
	a.restartDaemon()
	return dir
}

func (a *App) SetShareDir(dir string) error {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("invalid directory: %s", dir)
	}
	a.config.ShareDir = dir
	a.saveConfig()
	a.restartDaemon()
	return nil
}

func (a *App) OpenWebUI() {
	u := a.config.RelayURL
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", u).Start()
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		exec.Command("xdg-open", u).Start()
	}
}

func (a *App) OpenFolder() {
	dir := a.config.ShareDir
	if dir == "" {
		return
	}
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", dir).Start()
	case "windows":
		exec.Command("explorer", dir).Start()
	default:
		exec.Command("xdg-open", dir).Start()
	}
}

func (a *App) Logout() {
	deletePassword(a.config.Username)
	a.config.Username = ""
	a.config.Password = ""
	a.saveConfig()
	select {
	case <-a.quit:
	default:
		close(a.quit)
	}
	a.quit = make(chan struct{})
	a.setStatus(false, false)
}

func (a *App) ShowWindow() {
	wailsruntime.WindowShow(a.ctx)
}

func (a *App) GetAutostart() bool {
	return isAutostartEnabled()
}

func (a *App) SetAutostart(enable bool) error {
	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	if enable {
		return enableAutostart(execPath)
	}
	return disableAutostart()
}
