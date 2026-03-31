// Claudy daemon — runs on the source device (the "personal cloud" machine).
//
// On first run it walks the user through setup:
//   1. Relay server URL
//   2. Claudy username + password (register or log in)
//   3. Folder to share
//
// Config is saved to ~/.claudy/config.json and reused on subsequent starts.
//
// Environment variables (override config file):
//
//	CLAUDY_RELAY   — relay base URL, e.g. http://23.172.217.149
//	CLAUDY_USER    — username
//	CLAUDY_PASS    — password
//	CLAUDY_DIR     — folder to share
//	CLAUDY_NO_TRAY — set to "1" to disable system tray (headless mode)
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/claudy-app/claudy-core/pkg/autostart"
	"github.com/claudy-app/claudy-core/pkg/tray"
	"github.com/claudy-app/claudy-core/pkg/tunnel"
	"github.com/gorilla/websocket"
	"golang.org/x/net/webdav"
	"golang.org/x/term"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	RelayURL string `json:"relay_url"`
	Username string `json:"username"`
	Password string `json:"password"` // stored locally only
	ShareDir string `json:"share_dir"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claudy", "config.json")
}

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, json.Unmarshal(data, &c)
}

func saveConfig(c *Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// ---------------------------------------------------------------------------
// Setup wizard
// ---------------------------------------------------------------------------

func readLine(prompt string, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	v := strings.TrimSpace(scanner.Text())
	if v == "" {
		return def
	}
	return v
}

func readPassword(prompt string) string {
	fmt.Printf("%s: ", prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err == nil {
			return string(b)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	return strings.TrimSpace(scanner.Text())
}

// pickFolder opens a native OS folder picker and returns the chosen path.
func pickFolder(defaultDir string) string {
	switch runtime.GOOS {
	case "darwin":
		script := `tell application "Finder" to set p to choose folder with prompt "Choose a folder to share with Claudy:"
return POSIX path of p`
		out, err := exec.Command("osascript", "-e", script).Output()
		if err == nil {
			if chosen := strings.TrimRight(strings.TrimSpace(string(out)), "/"); chosen != "" {
				return chosen
			}
		}
	case "linux":
		for _, cmd := range [][]string{
			{"zenity", "--file-selection", "--directory", "--title=Choose a folder to share with Claudy"},
			{"kdialog", "--getexistingdirectory", defaultDir, "--title", "Choose a folder to share with Claudy"},
		} {
			out, err := exec.Command(cmd[0], cmd[1:]...).Output()
			if err == nil {
				if chosen := strings.TrimSpace(string(out)); chosen != "" {
					return chosen
				}
			}
		}
	case "windows":
		ps := `Add-Type -AssemblyName System.Windows.Forms; ` +
			`$d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
			`$d.Description = 'Choose a folder to share with Claudy'; ` +
			`$d.RootFolder = 'MyComputer'; ` +
			`if ($d.ShowDialog() -eq 'OK') { $d.SelectedPath }`
		out, err := exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
		if err == nil {
			if chosen := strings.TrimSpace(string(out)); chosen != "" {
				return chosen
			}
		}
	}
	return readLine("Folder to share", defaultDir)
}

func setupWizard() *Config {
	fmt.Println()
	fmt.Println("╔════════════════════════════════╗")
	fmt.Println("║      Claudy — First Setup      ║")
	fmt.Println("╚════════════════════════════════╝")
	fmt.Println()

	cfg := &Config{}
	cfg.RelayURL = strings.TrimRight(readLine("Relay server URL", "http://23.172.217.149"), "/")

	cfg.Username = readLine("Username", "")
	for cfg.Username == "" {
		fmt.Println("  Username cannot be empty.")
		cfg.Username = readLine("Username", "")
	}

	cfg.Password = readPassword("Password")
	for cfg.Password == "" {
		fmt.Println("  Password cannot be empty.")
		cfg.Password = readPassword("Password")
	}

	// Ask: register or login?
	fmt.Println()
	fmt.Println("Do you have an account?")
	fmt.Println("  [1] Log in to existing account")
	fmt.Println("  [2] Create a new account")
	choice := readLine("Choice", "1")

	if choice == "2" {
		fmt.Println("Registering...")
		if err := apiRegister(cfg.RelayURL, cfg.Username, cfg.Password); err != nil {
			fmt.Printf("  Registration failed: %s\n", err.Error())
			// If already taken, fall back to login.
			if strings.Contains(err.Error(), "already taken") {
				fmt.Println("  Username already taken — logging in with existing account.")
			} else {
				fmt.Println("Please try again.")
				return setupWizard()
			}
		} else {
			fmt.Printf("  Account created for %s.\n", cfg.Username)
		}
	}

	// Verify credentials.
	fmt.Println("Logging in...")
	if _, err := apiLogin(cfg.RelayURL, cfg.Username, cfg.Password); err != nil {
		fmt.Printf("  Login failed: %v\n", err)
		fmt.Println("Please check your credentials and try again.")
		return setupWizard()
	}
	fmt.Printf("  Logged in as %s.\n\n", cfg.Username)

	// Share folder.
	home, _ := os.UserHomeDir()
	defaultDir := filepath.Join(home, "CloudDrive")
	fmt.Println("Opening folder picker...")
	cfg.ShareDir = pickFolder(defaultDir)
	fmt.Printf("  Selected: %s\n\n", cfg.ShareDir)

	if err := os.MkdirAll(cfg.ShareDir, 0755); err != nil {
		log.Fatalf("Cannot create share directory: %v", err)
	}

	fmt.Printf("Config saved to %s\n\n", configPath())
	if err := saveConfig(cfg); err != nil {
		log.Printf("Warning: could not save config: %v", err)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// REST helpers
// ---------------------------------------------------------------------------

func apiRegister(relayURL, username, password string) error {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := http.Post(relayURL+"/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s", e["error"])
	}
	return nil
}

func apiLogin(relayURL, username, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := http.Post(relayURL+"/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("cannot reach relay: %v", err)
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

// ---------------------------------------------------------------------------
// Dynamic DAV handler (supports live folder swap via tray "Change Folder")
// ---------------------------------------------------------------------------

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
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("[webdav] %s %s — %v", r.Method, r.URL.Path, err)
			}
		},
	}
	d.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	cfg := &Config{
		RelayURL: os.Getenv("CLAUDY_RELAY"),
		Username: os.Getenv("CLAUDY_USER"),
		Password: os.Getenv("CLAUDY_PASS"),
		ShareDir: os.Getenv("CLAUDY_DIR"),
	}

	if cfg.RelayURL == "" || cfg.Username == "" || cfg.Password == "" || cfg.ShareDir == "" {
		saved, err := loadConfig()
		if err != nil {
			cfg = setupWizard()
		} else {
			if cfg.RelayURL == "" {
				cfg.RelayURL = saved.RelayURL
			}
			if cfg.Username == "" {
				cfg.Username = saved.Username
			}
			if cfg.Password == "" {
				cfg.Password = saved.Password
			}
			if cfg.ShareDir == "" {
				cfg.ShareDir = saved.ShareDir
			}
		}
	}

	info, err := os.Stat(cfg.ShareDir)
	if err != nil || !info.IsDir() {
		log.Fatalf("[daemon] share directory does not exist: %s", cfg.ShareDir)
	}

	log.Printf("[daemon] logging in as %s...", cfg.Username)
	token, err := apiLogin(cfg.RelayURL, cfg.Username, cfg.Password)
	if err != nil {
		log.Fatalf("[daemon] login failed: %v", err)
	}
	log.Println("[daemon] authenticated")

	wsURL, err := buildWSURL(cfg.RelayURL, token)
	if err != nil {
		log.Fatalf("[daemon] invalid relay URL: %v", err)
	}

	davPrefix := "/dav/" + cfg.Username
	dav := &dynamicDAV{}
	dav.setDir(davPrefix, cfg.ShareDir)
	log.Printf("[daemon] sharing %s as %s", cfg.ShareDir, davPrefix)

	// Status bus for tray updates.
	bus := tray.NewBus()

	// Quit channel — closed by tray Quit or OS signal.
	quit := make(chan struct{})

	// OS signal handler.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		close(quit)
	}()

	// Connection loop runs in background.
	go connectLoop(wsURL, dav, bus, quit)

	// Headless mode (no tray) — used when run via env vars / terminal.
	if os.Getenv("CLAUDY_NO_TRAY") == "1" {
		<-quit
		log.Println("[daemon] shutting down")
		return
	}

	// Autostart manager.
	as := autostart.New()
	autostartOn, _ := as.IsEnabled()

	// Tray runs on main goroutine (required by systray on macOS/Windows).
	tray.Run(tray.Options{
		RelayURL:    cfg.RelayURL,
		ShareDir:    cfg.ShareDir,
		Username:    cfg.Username,
		Bus:         bus,
		AutostartOn: autostartOn,
		OnQuit: func() {
			select {
			case <-quit:
			default:
				close(quit)
			}
		},
		OnFolder: func() string {
			dir := pickFolder(cfg.ShareDir)
			if dir == "" || dir == cfg.ShareDir {
				return ""
			}
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("[daemon] cannot create folder: %v", err)
				return ""
			}
			cfg.ShareDir = dir
			dav.setDir(davPrefix, dir)
			_ = saveConfig(cfg)
			log.Printf("[daemon] sharing folder changed to %s", dir)
			return dir
		},
		OnAutostart: func(enable bool) error {
			execPath, _ := os.Executable()
			if enable {
				return as.Enable(execPath)
			}
			return as.Disable()
		},
	})

	log.Println("[daemon] shutting down")
}

// ---------------------------------------------------------------------------
// WebSocket URL builder
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Connection loop
// ---------------------------------------------------------------------------

func connectLoop(wsURL string, dav http.Handler, bus *tray.Bus, quit <-chan struct{}) {
	backoff := time.Second
	for {
		bus.Send(tray.StatusConnecting)
		log.Printf("[daemon] connecting to relay...")
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			bus.Send(tray.StatusDisconnected)
			log.Printf("[daemon] connection failed: %v — retry in %s", err, backoff)
			select {
			case <-time.After(backoff):
			case <-quit:
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		bus.Send(tray.StatusConnected)
		log.Println("[daemon] connected — sharing folder")
		handleConnection(ws, dav)
		bus.Send(tray.StatusDisconnected)
		log.Println("[daemon] disconnected — reconnecting...")
		select {
		case <-time.After(time.Second):
		case <-quit:
			return
		}
	}
}

func handleConnection(ws *websocket.Conn, dav http.Handler) {
	var writeMu sync.Mutex
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var req tunnel.Request
		if json.Unmarshal(msg, &req) != nil {
			continue
		}
		go func() {
			resp := serveDav(dav, &req)
			data, _ := json.Marshal(resp)
			writeMu.Lock()
			_ = ws.WriteMessage(websocket.TextMessage, data)
			writeMu.Unlock()
		}()
	}
}

func serveDav(dav http.Handler, req *tunnel.Request) *tunnel.Response {
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
	return &tunnel.Response{
		ID:      req.ID,
		Status:  result.StatusCode,
		Headers: result.Header,
		Body:    body,
	}
}
