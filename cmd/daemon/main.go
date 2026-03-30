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
//   CLAUDY_RELAY   — relay base URL, e.g. http://23.172.217.149
//   CLAUDY_USER    — username
//   CLAUDY_PASS    — password
//   CLAUDY_DIR     — folder to share
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
// Setup wizard (runs on first launch or when config is missing)
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

// pickFolder opens a native OS folder picker and returns the chosen path.
// Falls back to text input if no GUI is available.
func pickFolder(defaultDir string) string {
	switch runtime.GOOS {
	case "darwin":
		// osascript shows the standard macOS folder chooser.
		script := `tell application "Finder" to set p to choose folder with prompt "Choose a folder to share with Claudy:"
return POSIX path of p`
		out, err := exec.Command("osascript", "-e", script).Output()
		if err == nil {
			chosen := strings.TrimRight(strings.TrimSpace(string(out)), "/")
			if chosen != "" {
				return chosen
			}
		}
	case "linux":
		// Try zenity (GNOME), then kdialog (KDE).
		for _, cmd := range [][]string{
			{"zenity", "--file-selection", "--directory", "--title=Choose a folder to share with Claudy"},
			{"kdialog", "--getexistingdirectory", defaultDir, "--title", "Choose a folder to share with Claudy"},
		} {
			out, err := exec.Command(cmd[0], cmd[1:]...).Output()
			if err == nil {
				chosen := strings.TrimSpace(string(out))
				if chosen != "" {
					return chosen
				}
			}
		}
	case "windows":
		// PowerShell folder browser dialog — works on all Windows versions.
		ps := `Add-Type -AssemblyName System.Windows.Forms; ` +
			`$d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
			`$d.Description = 'Choose a folder to share with Claudy'; ` +
			`$d.RootFolder = 'MyComputer'; ` +
			`if ($d.ShowDialog() -eq 'OK') { $d.SelectedPath }`
		out, err := exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
		if err == nil {
			chosen := strings.TrimSpace(string(out))
			if chosen != "" {
				return chosen
			}
		}
	}
	// Fallback: plain text prompt.
	return readLine("Folder to share", defaultDir)
}

func readPassword(prompt string) string {
	fmt.Printf("%s: ", prompt)
	// Use terminal raw mode if available, fall back to plain read.
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

func setupWizard() *Config {
	fmt.Println()
	fmt.Println("╔═══════════════════════════════╗")
	fmt.Println("║     Claudy — First Setup      ║")
	fmt.Println("╚═══════════════════════════════╝")
	fmt.Println()

	cfg := &Config{}

	// Relay URL
	cfg.RelayURL = readLine("Relay server URL", "http://23.172.217.149")
	cfg.RelayURL = strings.TrimRight(cfg.RelayURL, "/")

	// Username
	cfg.Username = readLine("Username", "")
	for cfg.Username == "" {
		fmt.Println("  Username cannot be empty.")
		cfg.Username = readLine("Username", "")
	}

	// Password
	cfg.Password = readPassword("Password")
	for cfg.Password == "" {
		fmt.Println("  Password cannot be empty.")
		cfg.Password = readPassword("Password")
	}

	// Register or login
	fmt.Println()
	fmt.Println("Trying to register...")
	if err := apiRegister(cfg.RelayURL, cfg.Username, cfg.Password); err != nil {
		// If registration fails because user exists, try login.
		fmt.Printf("  Note: %s — trying to log in instead.\n", err.Error())
	}

	// Verify credentials work.
	if _, err := apiLogin(cfg.RelayURL, cfg.Username, cfg.Password); err != nil {
		fmt.Printf("Login failed: %v\n", err)
		fmt.Println("Please check your credentials and try again.")
		return setupWizard()
	}
	fmt.Printf("  Logged in as %s.\n\n", cfg.Username)

	// Share folder — try native GUI picker, fall back to text.
	home, _ := os.UserHomeDir()
	defaultDir := filepath.Join(home, "CloudDrive")
	fmt.Println("Opening folder picker...")
	cfg.ShareDir = pickFolder(defaultDir)
	fmt.Printf("  Selected: %s\n", cfg.ShareDir)

	// Create folder if it doesn't exist.
	if err := os.MkdirAll(cfg.ShareDir, 0755); err != nil {
		log.Fatalf("Cannot create share directory: %v", err)
	}

	fmt.Println()
	fmt.Printf("Config saved to %s\n\n", configPath())
	if err := saveConfig(cfg); err != nil {
		log.Printf("Warning: could not save config: %v", err)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// REST helpers (talk to relay)
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
// Main
// ---------------------------------------------------------------------------

func main() {
	// Env vars override config file.
	cfg := &Config{
		RelayURL: os.Getenv("CLAUDY_RELAY"),
		Username: os.Getenv("CLAUDY_USER"),
		Password: os.Getenv("CLAUDY_PASS"),
		ShareDir: os.Getenv("CLAUDY_DIR"),
	}

	// If any field is missing, try loading config file.
	if cfg.RelayURL == "" || cfg.Username == "" || cfg.Password == "" || cfg.ShareDir == "" {
		saved, err := loadConfig()
		if err != nil {
			// First run — start the wizard.
			cfg = setupWizard()
		} else {
			// Merge: env vars take precedence over config file.
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

	// Validate share directory.
	info, err := os.Stat(cfg.ShareDir)
	if err != nil || !info.IsDir() {
		log.Fatalf("[daemon] share directory does not exist: %s", cfg.ShareDir)
	}

	// Log in to get a JWT.
	log.Printf("[daemon] logging in as %s...", cfg.Username)
	token, err := apiLogin(cfg.RelayURL, cfg.Username, cfg.Password)
	if err != nil {
		log.Fatalf("[daemon] login failed: %v", err)
	}
	log.Println("[daemon] authenticated")

	// Build WebSocket URL: ws(s)://host/tunnel?token=<jwt>
	wsURL, err := buildWSURL(cfg.RelayURL, token)
	if err != nil {
		log.Fatalf("[daemon] invalid relay URL: %v", err)
	}

	// WebDAV handler with the correct prefix for this user.
	davPrefix := "/dav/" + cfg.Username
	dav := &webdav.Handler{
		Prefix:     davPrefix,
		FileSystem: webdav.Dir(cfg.ShareDir),
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("[webdav] %s %s — %v", r.Method, r.URL.Path, err)
			}
		},
	}
	log.Printf("[daemon] sharing %s as %s", cfg.ShareDir, davPrefix)

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go connectLoop(wsURL, dav, quit)
	<-quit
	log.Println("[daemon] shutting down")
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

// ---------------------------------------------------------------------------
// Connection loop
// ---------------------------------------------------------------------------

func connectLoop(wsURL string, dav http.Handler, quit <-chan os.Signal) {
	backoff := time.Second
	for {
		log.Printf("[daemon] connecting to relay...")
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
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
		log.Println("[daemon] connected — sharing folder")
		handleConnection(ws, dav)
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
