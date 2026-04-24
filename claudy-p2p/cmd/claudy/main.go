// claudy: minimal one-binary MVP that wraps dav-owner and dav-client
// with a local-web UI. Friends download the bundle, double-click, a
// browser opens, two buttons: Share a folder / Connect to a shared folder.
//
// Design:
//   - We intentionally do NOT refactor the existing dav-owner and
//     dav-client CLI binaries. They're tested, they work, they ship
//     alongside `claudy` in the same directory. `claudy` invokes them
//     via os/exec and tails their stdout for status.
//   - HTTP server binds to 127.0.0.1:0 (random free port). We write
//     the chosen URL to stdout and shell out to the OS's default
//     browser opener. Loopback-only so a malicious LAN peer cannot
//     drive our UI.
//   - All state (current role, mounted path, room code, subprocess
//     handles) is in-memory on a single struct protected by a mutex.
//     Quit the app → every child gets killed and the mount is
//     unmounted; we register signal handlers + runtime.Goexit
//     cleanup.
//   - The HTML is a single file embedded via go:embed. No JS build
//     step, no frontend toolchain. If a friend wants to hack it, they
//     can extract the .app, swap the html, and run it.
//
// Not in scope for this MVP:
//   - Native folder picker (text input only; we'll add AppleScript /
//     PowerShell dialogs in a follow-up)
//   - QR pairing
//   - System tray / menubar integration
//   - Auto-update
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed webui/*
var webuiFS embed.FS

// appState is the singleton controller holding whichever role is
// currently active (share = owner, connect = viewer, idle = none).
// Methods on it serialise subprocess lifecycle and expose JSON-safe
// snapshots to the UI.
type appState struct {
	mu sync.Mutex
	// role: "", "share", "connect"
	role string
	// room is the rendezvous string both peers must share — we
	// auto-generate one on Share, user pastes it on Connect.
	room string
	// dir is the folder the owner shares. Empty on connect.
	dir string
	// mountPath is the filesystem path where the viewer mount landed.
	mountPath string
	// cmd is the running dav-owner or dav-client process.
	cmd *exec.Cmd
	// logLines is a ring buffer of child stdout/stderr lines for the
	// UI status pane. Simpler than SSE; the UI just polls.
	logLines []string
	// started records when the current session began (for "uptime" UI).
	started time.Time
	// lastStatus is the parsed state ("paired", "connected", "failed", …)
	// we try to keep the UI informative without re-parsing logs in JS.
	lastStatus string

	log    *slog.Logger
	binDir string // directory that contains dav-owner / dav-client
}

// ShareReq is POST /api/share body.
type ShareReq struct {
	Dir string `json:"dir"`
}

// ConnectReq is POST /api/connect body.
type ConnectReq struct {
	Room string `json:"room"`
}

// Status is the JSON returned by GET /api/status.
type Status struct {
	Role       string   `json:"role"`
	Room       string   `json:"room,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	MountPath  string   `json:"mount_path,omitempty"`
	State      string   `json:"state"`
	UptimeSecs int      `json:"uptime_secs,omitempty"`
	RecentLogs []string `json:"recent_logs,omitempty"`
}

// snapshot returns the current state for the UI. Holds the lock only
// as long as necessary to copy fields.
func (a *appState) snapshot() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Status{
		Role:      a.role,
		Room:      a.room,
		Dir:       a.dir,
		MountPath: a.mountPath,
		State:     a.lastStatus,
	}
	if !a.started.IsZero() {
		s.UptimeSecs = int(time.Since(a.started).Seconds())
	}
	// Hand back a bounded slice of recent log lines.
	n := len(a.logLines)
	if n > 30 {
		s.RecentLogs = append(s.RecentLogs, a.logLines[n-30:]...)
	} else {
		s.RecentLogs = append(s.RecentLogs, a.logLines...)
	}
	return s
}

// pushLogLine adds a line to the ring buffer and tries to update
// lastStatus if the line matches a known lifecycle marker. The UI
// uses lastStatus for the big banner; RecentLogs for the detail.
func (a *appState) pushLogLine(line string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logLines = append(a.logLines, line)
	if len(a.logLines) > 200 {
		a.logLines = a.logLines[len(a.logLines)-200:]
	}
	switch {
	case strings.Contains(line, "waiting for viewer"):
		a.lastStatus = "waiting"
	case strings.Contains(line, "paired with owner"):
		a.lastStatus = "paired"
	case strings.Contains(line, "state=connected"):
		a.lastStatus = "connected"
	case strings.Contains(line, "state=failed"), strings.Contains(line, "state=closed"):
		// only degrade if we haven't already succeeded since
		if a.lastStatus != "connected" {
			a.lastStatus = "reconnecting"
		}
	case strings.Contains(line, "ice selected"):
		if strings.Contains(line, "local=relay") || strings.Contains(line, "remote=relay") {
			a.lastStatus = "connected (via relay)"
		}
	}
}

// startShare spawns dav-owner with an auto-generated room and a
// fresh per-session peer alias. TOFU still pins whichever pubkey the
// viewer presents first, but because the alias includes the random
// room code, every share is a fresh trust decision — perfect for
// "one-off-give-a-friend-access" without polluting the keyring.
func (a *appState) startShare(dir string) error {
	a.mu.Lock()
	if a.cmd != nil {
		a.mu.Unlock()
		return fmt.Errorf("already running in role %q", a.role)
	}
	if dir == "" {
		a.mu.Unlock()
		return fmt.Errorf("dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("resolve dir: %w", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		a.mu.Unlock()
		return fmt.Errorf("%q is not a directory", abs)
	}
	room := randomRoom()
	alias := "viewer-" + room
	a.role = "share"
	a.dir = abs
	a.room = room
	a.started = time.Now()
	a.lastStatus = "starting"
	a.logLines = nil
	a.mu.Unlock()

	ownerBin := filepath.Join(a.binDir, ownerName())
	cmd := exec.Command(ownerBin,
		"-signal", defaultSignalURL(),
		"-room", room,
		"-dir", abs,
		"-peer-alias", alias,
	)
	return a.launch(cmd)
}

// startConnect spawns dav-client + a platform-specific mount command
// once the client reports "local WebDAV proxy ready".
func (a *appState) startConnect(room string) error {
	a.mu.Lock()
	if a.cmd != nil {
		a.mu.Unlock()
		return fmt.Errorf("already running in role %q", a.role)
	}
	room = strings.TrimSpace(room)
	if room == "" {
		a.mu.Unlock()
		return fmt.Errorf("room code is required")
	}
	alias := "owner-" + room
	a.role = "connect"
	a.room = room
	a.started = time.Now()
	a.lastStatus = "starting"
	a.logLines = nil
	a.mu.Unlock()

	localAddr := fmt.Sprintf("127.0.0.1:%d", freePort())
	clientBin := filepath.Join(a.binDir, clientName())
	cmd := exec.Command(clientBin,
		"-signal", defaultSignalURL(),
		"-room", room,
		"-local", localAddr,
		"-peer-alias", alias,
	)
	if err := a.launch(cmd); err != nil {
		return err
	}

	// Wait for the PC to actually be connected before mounting.
	// "local WebDAV proxy ready" fires as soon as the TCP listener is
	// up — before the WebRTC leg is done. Mounting that early lets
	// Finder's first PROPFIND race the PC handshake and frequently
	// time out the mount_webdav deadline. Waiting for a real
	// state=connected log line makes the mount instant for the user.
	go func() {
		if !a.waitForStatus(func(s Status) bool {
			return s.State == "connected" || s.State == "connected (via relay)"
		}, 60*time.Second) {
			a.pushLogLine("viewer never reached connected within 60s — mount skipped")
			return
		}
		if err := a.mount(localAddr, room); err != nil {
			a.pushLogLine("mount failed: " + err.Error())
		}
	}()
	return nil
}

// launch starts cmd, wires its stdout/stderr into our log ring buffer,
// records the handle, and arms a goroutine that updates lastStatus to
// "stopped" when the process exits.
func (a *appState) launch(cmd *exec.Cmd) error {
	// dav-owner / dav-client emit all logs on stdout via slog's
	// NewTextHandler(os.Stdout) (dav-owner) or os.Stderr (dav-client).
	// We capture both streams through separate pipes and fan them into
	// the same ring buffer.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	a.mu.Lock()
	a.cmd = cmd
	a.mu.Unlock()

	go a.pipeToLog(stdout)
	go a.pipeToLog(stderr)
	go func() {
		err := cmd.Wait()
		a.mu.Lock()
		a.cmd = nil
		a.lastStatus = "stopped"
		a.mu.Unlock()
		if err != nil {
			a.pushLogLine("child exited: " + err.Error())
		}
	}()
	return nil
}

// pipeToLog forwards each line from a child's pipe to our ring buffer.
func (a *appState) pipeToLog(r io.ReadCloser) {
	defer r.Close()
	buf := make([]byte, 4096)
	var partial []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			partial = append(partial, buf[:n]...)
			for {
				i := strings.IndexByte(string(partial), '\n')
				if i < 0 {
					break
				}
				line := strings.TrimRight(string(partial[:i]), "\r")
				a.pushLogLine(line)
				partial = partial[i+1:]
			}
		}
		if err != nil {
			if len(partial) > 0 {
				a.pushLogLine(string(partial))
			}
			return
		}
	}
}

// stop kills the active child and, if viewer, unmounts.
func (a *appState) stop() {
	a.mu.Lock()
	cmd := a.cmd
	role := a.role
	mountPath := a.mountPath
	a.role = ""
	a.dir = ""
	a.room = ""
	a.mountPath = ""
	a.lastStatus = "stopped"
	a.cmd = nil
	a.mu.Unlock()

	if role == "connect" && mountPath != "" {
		_ = exec.Command(unmountBin(), unmountArgs(mountPath)...).Run()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		// Short grace period, then kill.
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
}

// waitForStatus polls snapshot() until pred returns true or timeout.
func (a *appState) waitForStatus(pred func(Status) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred(a.snapshot()) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// httpHandlers wires the static web UI + JSON API. The static files
// live in webui/ and are embedded at compile time.
func (a *appState) httpHandlers() http.Handler {
	mux := http.NewServeMux()

	// Static index — redirect root to /ui/
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := webuiFS.ReadFile("webui/index.html")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.snapshot())
	})

	mux.HandleFunc("/api/share", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req ShareReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := a.startShare(req.Dir); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, a.snapshot())
	})

	mux.HandleFunc("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req ConnectReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := a.startConnect(req.Room); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, a.snapshot())
	})

	mux.HandleFunc("/api/pick-dir", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		path, err := pickFolder()
		if errors.Is(err, errCanceled) {
			// Soft cancel — UI stays on picker screen without an error.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"dir": path})
	})

	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		a.stop()
		writeJSON(w, a.snapshot())
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// randomRoom picks 6 chars from a dictionary that avoids look-alike
// glyphs (I/l/1, O/0). Short enough to dictate over the phone.
func randomRoom() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 6)
	// crypto/rand would be overkill; we just need unguessable-ish
	// within the lifetime of a share. math/rand seeded with time is
	// fine for MVP since the signal server doesn't use the room ID
	// for anything authoritative.
	now := time.Now().UnixNano()
	for i := range b {
		now = now*6364136223846793005 + 1442695040888963407
		b[i] = alphabet[int(uint64(now)>>32)%len(alphabet)]
	}
	return string(b)
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// defaultSignalURL returns the signaling WebSocket Claudy's MVP uses.
// Overridable via CLAUDY_SIGNAL_URL env var for self-hosting.
func defaultSignalURL() string {
	if v := os.Getenv("CLAUDY_SIGNAL_URL"); v != "" {
		return v
	}
	return "ws://23.172.217.149:7042/signal"
}

func ownerName() string {
	if runtime.GOOS == "windows" {
		return "claudy-dav-owner.exe"
	}
	return "claudy-dav-owner"
}

func clientName() string {
	if runtime.GOOS == "windows" {
		return "claudy-dav-client.exe"
	}
	return "claudy-dav-client"
}

// openBrowser fires off a best-effort platform command to open url.
// Errors here are non-fatal — user can always copy the URL from the
// stdout banner.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// mount runs the platform-specific "make this WebDAV URL look like a
// folder" command. We block until the mount succeeds or times out,
// then record the path so stop() can unmount it later.
func (a *appState) mount(localAddr, room string) error {
	switch runtime.GOOS {
	case "darwin":
		path := "/tmp/claudy-" + room
		_ = os.MkdirAll(path, 0o755)
		url := "http://" + localAddr + "/"
		out, err := exec.Command("mount_webdav", "-v", "claudy-"+room, url, path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mount_webdav: %s: %w", strings.TrimSpace(string(out)), err)
		}
		a.mu.Lock()
		a.mountPath = path
		a.mu.Unlock()
		a.pushLogLine("mounted at " + path)
		// Open the folder in Finder so the friend sees it immediately.
		_ = exec.Command("open", path).Start()
		return nil
	case "windows":
		// `net use *` picks the first available drive letter.
		url := "http://" + localAddr + "/"
		out, err := exec.Command("net", "use", "*", url, "/persistent:no").CombinedOutput()
		if err != nil {
			return fmt.Errorf("net use: %s: %w", strings.TrimSpace(string(out)), err)
		}
		// Parse drive letter from output: "Drive X: is now connected to..."
		drive := parseNetUseDrive(string(out))
		a.mu.Lock()
		a.mountPath = drive
		a.mu.Unlock()
		a.pushLogLine("mounted as " + drive)
		if drive != "" {
			_ = exec.Command("explorer", drive).Start()
		}
		return nil
	default:
		return fmt.Errorf("automatic mount not supported on %s; open http://%s manually", runtime.GOOS, localAddr)
	}
}

// pickFolder opens the OS-native folder picker dialog synchronously
// and returns the selected path. Returns (""`,"" `canceled`) when the
// user dismisses the dialog, or (empty, error) on a real failure.
//
// Platform commands:
//   - macOS: `osascript` invokes the Finder "choose folder" panel.
//     If the user hits Cancel, osascript exits with -128 and stderr
//     says "User canceled." We translate that to a sentinel error.
//   - Windows: PowerShell's FolderBrowserDialog (WinForms). Cancel
//     yields an empty stdout; we treat that as canceled.
//   - Linux: fall back to zenity or kdialog if available; if neither,
//     return a soft error so the UI can fall back to the text input.
func pickFolder() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		// -e lines assemble an AppleScript that asks for a folder.
		// We wrap the result in POSIX path so we get "/Users/..." not
		// HFS-style "Macintosh HD:Users:...".
		cmd := exec.Command("osascript",
			"-e", `set p to POSIX path of (choose folder with prompt "Pick a folder to share with Claudy")`,
			"-e", "return p",
		)
		out, err := cmd.Output()
		if err != nil {
			// User cancel shows up as "execution error: User canceled. (-128)"
			if ee, ok := err.(*exec.ExitError); ok && strings.Contains(string(ee.Stderr), "User canceled") {
				return "", errCanceled
			}
			return "", fmt.Errorf("osascript: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	case "windows":
		// Single-line PowerShell that launches FolderBrowserDialog.
		// -STA required because WinForms dialogs must run on a
		// single-threaded apartment; PowerShell Core defaults to MTA
		// and the dialog silently fails.
		script := `Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.FolderBrowserDialog
$f.Description = "Pick a folder to share with Claudy"
if ($f.ShowDialog() -eq 'OK') { Write-Output $f.SelectedPath }`
		cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("powershell: %w", err)
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			return "", errCanceled
		}
		return path, nil
	case "linux":
		// Try zenity first, fall back to kdialog.
		if _, err := exec.LookPath("zenity"); err == nil {
			out, err := exec.Command("zenity", "--file-selection", "--directory",
				"--title=Pick a folder to share with Claudy").Output()
			if err != nil {
				return "", errCanceled
			}
			return strings.TrimSpace(string(out)), nil
		}
		if _, err := exec.LookPath("kdialog"); err == nil {
			out, err := exec.Command("kdialog", "--getexistingdirectory").Output()
			if err != nil {
				return "", errCanceled
			}
			return strings.TrimSpace(string(out)), nil
		}
		return "", fmt.Errorf("no folder picker available; install zenity or kdialog")
	}
	return "", fmt.Errorf("folder picker unsupported on %s", runtime.GOOS)
}

// errCanceled is the sentinel we return when the user dismisses the
// native folder picker. HTTP handler maps it to a soft 204 so the UI
// knows to stay on the pick screen without showing an error.
var errCanceled = errors.New("canceled")

func parseNetUseDrive(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Drive ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1] // "X:"
			}
		}
	}
	return ""
}

func unmountBin() string {
	switch runtime.GOOS {
	case "darwin":
		return "umount"
	case "windows":
		return "net"
	default:
		return "umount"
	}
}

func unmountArgs(path string) []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"use", path, "/delete", "/yes"}
	default:
		return []string{path}
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "UI HTTP listen address (0 = random)")
	noBrowser := flag.Bool("no-browser", false, "do not auto-open the browser")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Binaries must sit next to our own executable.
	exePath, err := os.Executable()
	if err != nil {
		log.Error("resolve own path", "err", err)
		os.Exit(1)
	}
	binDir := filepath.Dir(exePath)

	app := &appState{log: log, binDir: binDir}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	log.Info("claudy UI", "url", url)
	fmt.Printf("\n  Claudy UI running at %s\n  (close this window to quit)\n\n", url)

	srv := &http.Server{Handler: app.httpHandlers()}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("http serve", "err", err)
		}
	}()

	if !*noBrowser {
		openBrowser(url)
	}

	// Cleanup on Ctrl+C or terminate.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Info("shutting down")
	app.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
