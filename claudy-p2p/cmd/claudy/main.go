// claudy: one-binary MVP that wraps dav-owner / dav-client behind a
// local web UI. v2 supports sharing one folder with multiple friends in
// parallel by spawning N independent dav-owner subprocesses, each in its
// own room. The UI (webui/index.html) is a single embedded HTML file.
//
// Concurrency model:
//   - Connect side stays 1:1 — at most one viewer subprocess at a time.
//   - Share side becomes 1:N — one chosen folder, zero or more "slots".
//     Each slot is a fully independent dav-owner subprocess: own room,
//     own PeerConnection, own keyring alias. They all read the same
//     local directory; the operating system handles concurrent reads
//     fine. Concurrent writes from different viewers race the way they
//     would on any shared filesystem.
//
// Platform-specific bits live at the bottom: native folder picker (osascript
// / PowerShell / zenity), mount commands (mount_webdav / `net use`),
// browser-opener (open / start / xdg-open).
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

// subprocess wraps a single dav-owner or dav-client child plus everything
// the UI needs to render its lifecycle: room code, recent log lines,
// status banner, mount path (viewer only). Methods are safe to call from
// multiple goroutines; logLines and lastStatus are guarded by mu.
type subprocess struct {
	role      string // "share" or "connect"
	room      string
	dir       string // share only — absolute path of the folder being exposed
	localAddr string // connect only — http://127.0.0.1:PORT served by dav-client
	started   time.Time
	log       *slog.Logger

	mu         sync.Mutex
	cmd        *exec.Cmd
	mountPath  string // connect only, set after successful mount
	logLines   []string
	lastStatus string
}

// SlotInfo is one row in the Share screen's viewer list, plus the body
// of POST /api/share/add. RecentLogs is bounded to ~30 lines.
type SlotInfo struct {
	Room       string   `json:"room"`
	State      string   `json:"state"`
	UptimeSecs int      `json:"uptime_secs,omitempty"`
	RecentLogs []string `json:"recent_logs,omitempty"`
}

// ConnectInfo is the connect-side equivalent. Same fields plus mount_path.
type ConnectInfo struct {
	Room       string   `json:"room"`
	State      string   `json:"state"`
	MountPath  string   `json:"mount_path,omitempty"`
	UptimeSecs int      `json:"uptime_secs,omitempty"`
	RecentLogs []string `json:"recent_logs,omitempty"`
}

// ShareInfo describes the active share session: the chosen folder plus
// every spawned slot. Empty Dir means the user hasn't picked a folder yet.
type ShareInfo struct {
	Dir   string     `json:"dir"`
	Slots []SlotInfo `json:"slots"`
}

// Status is the JSON returned by GET /api/status. Either Share or
// Connect (or both) may be nil when the user isn't using that mode yet.
type Status struct {
	Share   *ShareInfo   `json:"share,omitempty"`
	Connect *ConnectInfo `json:"connect,omitempty"`
}

// allLogs returns the full ring buffer (up to ~200 lines) under the
// lock. Used by /api/logs for the "copy everything" tester button —
// snapshot only hands out the trailing 30 lines that fit in the UI.
func (s *subprocess) allLogs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.logLines))
	copy(out, s.logLines)
	return out
}

// snapshot copies the subprocess state under the lock, returning a
// JSON-friendly view.
func (s *subprocess) snapshot() (state string, uptimeSecs int, logs []string, mountPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state = s.lastStatus
	if !s.started.IsZero() {
		uptimeSecs = int(time.Since(s.started).Seconds())
	}
	mountPath = s.mountPath
	n := len(s.logLines)
	if n > 30 {
		logs = append(logs, s.logLines[n-30:]...)
	} else if n > 0 {
		logs = append(logs, s.logLines...)
	}
	return
}

// pushLogLine appends to the ring buffer and fires status-line parsing
// for a few well-known markers. The UI uses lastStatus for the big
// banner; recent_logs for the diagnostic pane.
func (s *subprocess) pushLogLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	switch {
	case strings.Contains(line, "waiting for viewer"):
		s.lastStatus = "waiting"
	case strings.Contains(line, "paired with owner"), strings.Contains(line, "paired with viewer"):
		if s.lastStatus != "connected" {
			s.lastStatus = "paired"
		}
	case strings.Contains(line, "tunnel up"):
		s.lastStatus = "connected"
	case strings.Contains(line, "state=failed"), strings.Contains(line, "state=closed"):
		if s.lastStatus != "connected" {
			s.lastStatus = "reconnecting"
		}
	case strings.Contains(line, "ice selected"):
		if strings.Contains(line, "local=relay") || strings.Contains(line, "remote=relay") {
			s.lastStatus = "connected (via relay)"
		}
	}
}

// launch starts cmd, wires its stdout/stderr into the ring buffer, and
// arms a goroutine that flips lastStatus to "stopped" when the process
// exits. Returns an error only if exec.Start fails before the process
// is alive — after that, lifetime watching is async.
func (s *subprocess) launch(cmd *exec.Cmd) error {
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
	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()

	go s.pipeToLog(stdout)
	go s.pipeToLog(stderr)
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.cmd = nil
		s.lastStatus = "stopped"
		s.mu.Unlock()
		if err != nil {
			s.pushLogLine("child exited: " + err.Error())
		}
	}()
	return nil
}

// pipeToLog forwards each line from a child's pipe to our ring buffer.
func (s *subprocess) pipeToLog(r io.ReadCloser) {
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
				s.pushLogLine(line)
				partial = partial[i+1:]
			}
		}
		if err != nil {
			if len(partial) > 0 {
				s.pushLogLine(string(partial))
			}
			return
		}
	}
}

// stop sends SIGTERM, waits a few seconds, then kills.
func (s *subprocess) stop() {
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	s.lastStatus = "stopped"
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
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

// waitForStatus polls until pred is true or timeout.
func (s *subprocess) waitForStatus(pred func(state string) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, _, _, _ := s.snapshot()
		if pred(state) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// appState is the singleton controller. Connect side is 1:1 (single
// active subprocess at most). Share side is 1:N: one folder shared via
// any number of independent owner subprocesses, keyed by their room.
type appState struct {
	mu sync.Mutex

	// Connect: one viewer or none.
	connect *subprocess

	// Share: empty folder = not sharing. Slots may be empty even when a
	// folder is set (user picked dir but hasn't added any viewers yet).
	shareDir string
	slots    map[string]*subprocess // keyed by room

	log    *slog.Logger
	binDir string
}

func newAppState(log *slog.Logger, binDir string) *appState {
	return &appState{
		log:    log,
		binDir: binDir,
		slots:  map[string]*subprocess{},
	}
}

// snapshot collects the JSON representation for /api/status.
func (a *appState) snapshot() Status {
	var st Status

	a.mu.Lock()
	connect := a.connect
	dir := a.shareDir
	slots := make([]*subprocess, 0, len(a.slots))
	for _, sp := range a.slots {
		slots = append(slots, sp)
	}
	a.mu.Unlock()

	if connect != nil {
		state, up, logs, mp := connect.snapshot()
		st.Connect = &ConnectInfo{
			Room: connect.room, State: state, MountPath: mp,
			UptimeSecs: up, RecentLogs: logs,
		}
	}
	if dir != "" || len(slots) > 0 {
		share := &ShareInfo{Dir: dir, Slots: make([]SlotInfo, 0, len(slots))}
		for _, sp := range slots {
			state, up, logs, _ := sp.snapshot()
			share.Slots = append(share.Slots, SlotInfo{
				Room: sp.room, State: state, UptimeSecs: up, RecentLogs: logs,
			})
		}
		st.Share = share
	}
	return st
}

// startShare records the folder the user wants to expose. Slots are
// added separately via /api/share/add — picking the folder up front is
// what lets us keep the per-viewer flow trivial (just "+ another viewer").
func (a *appState) startShare(dir string) error {
	if dir == "" {
		return fmt.Errorf("dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("%q is not a directory", abs)
	}
	a.mu.Lock()
	a.shareDir = abs
	a.mu.Unlock()
	return nil
}

// addViewer spawns a fresh dav-owner in a new room sharing the active
// folder. Returns the new slot's room code so the UI can display it.
func (a *appState) addViewer() (string, error) {
	a.mu.Lock()
	dir := a.shareDir
	a.mu.Unlock()
	if dir == "" {
		return "", fmt.Errorf("no folder selected; call /api/share/start first")
	}
	room := randomRoom()
	alias := "viewer-" + room
	sp := &subprocess{
		role:       "share",
		room:       room,
		dir:        dir,
		started:    time.Now(),
		log:        a.log,
		lastStatus: "starting",
	}
	ownerBin := filepath.Join(a.binDir, ownerName())
	cmd := exec.Command(ownerBin,
		"-signal", defaultSignalURL(),
		"-room", room,
		"-dir", dir,
		"-peer-alias", alias,
	)
	if err := sp.launch(cmd); err != nil {
		return "", err
	}
	a.mu.Lock()
	a.slots[room] = sp
	a.mu.Unlock()
	return room, nil
}

// removeViewer stops a single slot and deletes it from the map. Idempotent
// for unknown rooms.
func (a *appState) removeViewer(room string) {
	a.mu.Lock()
	sp := a.slots[room]
	delete(a.slots, room)
	a.mu.Unlock()
	if sp != nil {
		sp.stop()
	}
}

// stopShare kills every viewer slot and forgets the folder.
func (a *appState) stopShare() {
	a.mu.Lock()
	slots := a.slots
	a.slots = map[string]*subprocess{}
	a.shareDir = ""
	a.mu.Unlock()
	for _, sp := range slots {
		sp.stop()
	}
}

// startConnect spawns a viewer subprocess and arms the post-handshake
// auto-mount goroutine.
func (a *appState) startConnect(room string) error {
	a.mu.Lock()
	if a.connect != nil {
		a.mu.Unlock()
		return fmt.Errorf("already connected to %q", a.connect.room)
	}
	a.mu.Unlock()

	// UI displays codes in upper case for legibility; server stores them
	// verbatim and Share generates lower case. Normalize so a friend's
	// "ABC123" lookup hits the owner sitting in "abc123".
	room = strings.ToLower(strings.TrimSpace(room))
	if room == "" {
		return fmt.Errorf("room code is required")
	}
	alias := "owner-" + room
	localAddr := fmt.Sprintf("127.0.0.1:%d", freePort())
	sp := &subprocess{
		role:       "connect",
		room:       room,
		localAddr:  localAddr,
		started:    time.Now(),
		log:        a.log,
		lastStatus: "starting",
	}
	clientBin := filepath.Join(a.binDir, clientName())
	cmd := exec.Command(clientBin,
		"-signal", defaultSignalURL(),
		"-room", room,
		"-local", localAddr,
		"-peer-alias", alias,
	)
	if err := sp.launch(cmd); err != nil {
		return err
	}
	a.mu.Lock()
	a.connect = sp
	a.mu.Unlock()

	// Mount once the tunnel reports connected. We wait for "connected"
	// (PC + Noise + yamux all up) rather than just the proxy listener,
	// otherwise the first PROPFIND races the handshake and Finder times
	// out the mount before bytes flow.
	go func() {
		ok := sp.waitForStatus(func(state string) bool {
			return state == "connected" || state == "connected (via relay)"
		}, 60*time.Second)
		if !ok {
			sp.pushLogLine("viewer never reached connected within 60s — mount skipped")
			return
		}
		if err := mount(localAddr, room, sp); err != nil {
			sp.pushLogLine("mount failed: " + err.Error())
		}
	}()
	return nil
}

// stopConnect kills the viewer subprocess and unmounts the share.
func (a *appState) stopConnect() {
	a.mu.Lock()
	sp := a.connect
	a.connect = nil
	a.mu.Unlock()
	if sp == nil {
		return
	}
	sp.mu.Lock()
	mountPath := sp.mountPath
	sp.mu.Unlock()
	if mountPath != "" {
		_ = exec.Command(unmountBin(), unmountArgs(mountPath)...).Run()
	}
	sp.stop()
}

// stopAll is the catch-all used at process exit.
func (a *appState) stopAll() {
	a.stopConnect()
	a.stopShare()
}

// httpHandlers wires the embedded UI plus the JSON API. The API is split
// into two flat namespaces: /api/share/* drives the multi-viewer share
// list, /api/connect/* drives the single viewer mount. /api/status
// returns the union; /api/pick-dir runs the OS-native folder picker.
func (a *appState) httpHandlers() http.Handler {
	mux := http.NewServeMux()

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

	// /api/logs returns a plain-text dump of every subprocess's full
	// ring buffer — the "I'm a beta tester, here's a paste" button.
	// Includes both connect and all share slots, separated by headers.
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		var b strings.Builder
		fmt.Fprintf(&b, "Claudy log dump — %s\nOS: %s/%s\n\n",
			time.Now().Format(time.RFC3339), runtime.GOOS, runtime.GOARCH)

		a.mu.Lock()
		connect := a.connect
		dir := a.shareDir
		slots := make([]*subprocess, 0, len(a.slots))
		for _, sp := range a.slots {
			slots = append(slots, sp)
		}
		a.mu.Unlock()

		if connect != nil {
			fmt.Fprintf(&b, "=== Connect (room: %s) ===\n", connect.room)
			for _, line := range connect.allLogs() {
				b.WriteString(line)
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
		}
		if dir != "" || len(slots) > 0 {
			fmt.Fprintf(&b, "=== Share — folder: %s ===\n\n", dir)
			for _, sp := range slots {
				fmt.Fprintf(&b, "--- slot room: %s ---\n", sp.room)
				for _, line := range sp.allLogs() {
					b.WriteString(line)
					b.WriteByte('\n')
				}
				b.WriteByte('\n')
			}
		}
		if connect == nil && len(slots) == 0 {
			b.WriteString("(no active sessions)\n")
		}
		_, _ = w.Write([]byte(b.String()))
	})

	mux.HandleFunc("/api/pick-dir", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		path, err := pickFolder()
		if errors.Is(err, errCanceled) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"dir": path})
	})

	// Share: pick folder, then add/remove viewer slots independently.
	mux.HandleFunc("/api/share/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Dir string `json:"dir"`
		}
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
	mux.HandleFunc("/api/share/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		room, err := a.addViewer()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"room": room})
	})
	mux.HandleFunc("/api/share/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Room string `json:"room"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		a.removeViewer(strings.ToLower(strings.TrimSpace(req.Room)))
		writeJSON(w, a.snapshot())
	})
	mux.HandleFunc("/api/share/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		a.stopShare()
		writeJSON(w, a.snapshot())
	})

	// Connect: single viewer, like before.
	mux.HandleFunc("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Room string `json:"room"`
		}
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
	mux.HandleFunc("/api/connect/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		a.stopConnect()
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

// defaultSignalURL — the public Claudy signaling endpoint, overridable
// via env for self-hosting.
func defaultSignalURL() string {
	if v := os.Getenv("CLAUDY_SIGNAL_URL"); v != "" {
		return v
	}
	return "ws://23.172.217.149:7042/signal"
}

// ----- Platform-specific helpers (folder picker, mount, browser) -----

// errCanceled is the sentinel we return when the user dismissed the
// native folder picker. The HTTP handler maps it to 204 so the UI stays
// on whichever screen the user is on, with no error banner.
var errCanceled = errors.New("canceled")

// errPickerUnavailable means the OS has no working folder dialog (no
// zenity/kdialog on Linux, etc.). The UI catches this and falls back to
// a manual-path text input — the share flow still completes.
var errPickerUnavailable = errors.New("native folder picker unavailable")

// pickFolder runs the OS-native folder selection dialog and returns the
// chosen absolute path. It is a synchronous call that pops a real
// modal — the goroutine handling the HTTP request blocks until the user
// dismisses or selects.
//
// Per platform:
//   - macOS: AppleScript via osascript. Cancel exits 1 with "User canceled."
//     on stderr; we map that to errCanceled.
//   - Windows: PowerShell launches WinForms FolderBrowserDialog. We pin
//     stdout to UTF-8 and parent the dialog to a hidden TopMost form,
//     otherwise (a) Cyrillic paths come back mojibake and (b) the dialog
//     opens behind the browser the user is staring at — they think the
//     click did nothing and panic.
//   - Linux: try zenity first, then kdialog; if neither is on PATH the
//     UI falls back to a manual-path text input.
func pickFolder() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return pickFolderDarwin()
	case "windows":
		return pickFolderWindows()
	case "linux":
		return pickFolderLinux()
	}
	return "", errPickerUnavailable
}

func pickFolderDarwin() (string, error) {
	cmd := exec.Command("osascript",
		"-e", `set p to POSIX path of (choose folder with prompt "Pick a folder to share with Claudy")`,
		"-e", "return p",
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && strings.Contains(string(ee.Stderr), "User canceled") {
			return "", errCanceled
		}
		return "", fmt.Errorf("osascript: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func pickFolderWindows() (string, error) {
	// Three things this script gets right that the naive WinForms launch
	// gets wrong:
	//  1. UTF-8 stdout — without this, Cyrillic / non-ASCII paths come
	//     back to Go decoded from CP866 as garbage.
	//  2. Hidden TopMost owner form — bare FolderBrowserDialog is
	//     parented to the desktop and frequently opens behind the
	//     browser. Owning it to a TopMost form keeps it on top.
	//  3. -STA — WinForms dialogs require single-threaded apartment;
	//     PowerShell Core defaults to MTA and the dialog silently fails.
	script := `[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$owner = New-Object System.Windows.Forms.Form
$owner.TopMost = $true
$owner.ShowInTaskbar = $false
$owner.StartPosition = 'Manual'
$owner.Location = New-Object System.Drawing.Point(-2000,-2000)
$owner.Size = New-Object System.Drawing.Size(1,1)
$owner.Show()
try {
  $f = New-Object System.Windows.Forms.FolderBrowserDialog
  $f.Description = "Pick a folder to share with Claudy"
  $result = $f.ShowDialog($owner)
  if ($result -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $f.SelectedPath }
} finally { $owner.Close() }`
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("powershell: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errCanceled
	}
	return path, nil
}

func pickFolderLinux() (string, error) {
	// Prefer zenity (GNOME-leaning, present on most desktop distros);
	// fall back to kdialog (KDE). Both have stable CLI for "select a
	// directory" + return path on stdout. If neither is installed we
	// surface a sentinel error so the UI can show a manual-path input.
	if _, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.Command("zenity", "--file-selection", "--directory",
			"--title=Pick a folder to share with Claudy").Output()
		if err != nil {
			// zenity exits 1 on cancel — indistinguishable from real failure
			// without parsing stderr; but stderr is usually empty on cancel.
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) == 0 {
				return "", errCanceled
			}
			return "", errCanceled
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			return "", errCanceled
		}
		return path, nil
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		out, err := exec.Command("kdialog", "--getexistingdirectory",
			"--title", "Pick a folder to share with Claudy").Output()
		if err != nil {
			return "", errCanceled
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			return "", errCanceled
		}
		return path, nil
	}
	return "", errPickerUnavailable
}

// ownerName / clientName resolve the sibling binary names. claudy
// requires them to live next to itself (filepath.Dir(os.Executable)),
// so the bundle ships claudy + claudy-dav-owner + claudy-dav-client
// as a single zip.
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

// mount runs the platform-specific "make this WebDAV URL look like a
// folder" command. We block until it succeeds or fails, then record the
// path so stopConnect can unmount cleanly.
func mount(localAddr, room string, sp *subprocess) error {
	switch runtime.GOOS {
	case "darwin":
		path := "/tmp/claudy-" + room
		_ = os.MkdirAll(path, 0o755)
		url := "http://" + localAddr + "/"
		out, err := exec.Command("mount_webdav", "-v", "claudy-"+room, url, path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mount_webdav: %s: %w", strings.TrimSpace(string(out)), err)
		}
		sp.mu.Lock()
		sp.mountPath = path
		sp.mu.Unlock()
		sp.pushLogLine("mounted at " + path)
		// Pop Finder to the new volume so the user knows it's there.
		_ = exec.Command("open", path).Start()
		return nil
	case "windows":
		// Wipe any stale claudy mounts BEFORE creating a fresh one. Each
		// session listens on a new random port, so without cleanup
		// Windows accumulates one dead drive letter per Connect click —
		// users see Y:, X:, W:, V:, … all pointing to dead 127.0.0.1
		// proxies, and Explorer's "Network locations" panel slowly
		// fills with corpses. We only remove entries that map to
		// 127.0.0.1, so the user's other network drives stay intact.
		cleanStaleClaudyDrives()
		url := "http://" + localAddr + "/"
		out, err := exec.Command("net", "use", "*", url, "/persistent:no").CombinedOutput()
		if err != nil {
			return fmt.Errorf("net use: %s: %w", strings.TrimSpace(string(out)), err)
		}
		drive := parseNetUseDrive(string(out))
		sp.mu.Lock()
		sp.mountPath = drive
		sp.mu.Unlock()
		sp.pushLogLine("mounted as " + drive)
		if drive != "" {
			_ = exec.Command("explorer", drive).Start()
		}
		return nil
	case "linux":
		// Prefer GVFS via gio: it's userspace (no sudo), present on every
		// GNOME/KDE/Cinnamon desktop, and the resulting mount appears in
		// Nautilus / Dolphin / Files exactly like a real shared folder.
		// `mount.davfs` would be more universal but requires root and a
		// passwd config; that's a non-starter for "double-click and it
		// works" UX.
		//
		// gio puts the mount under /run/user/$UID/gvfs/dav:host=...,port=...
		// which is what we hand back to the UI as the "mount path". Some
		// minimal X / tiling-WM setups don't run a gvfs daemon — gio mount
		// errors and we surface a hint to mount manually.
		if _, err := exec.LookPath("gio"); err != nil {
			return fmt.Errorf("gio not found; install glib-networking + gvfs-backends, " +
				"or open http://" + localAddr + " in Files via 'Connect to Server'")
		}
		url := "dav://" + localAddr + "/"
		out, err := exec.Command("gio", "mount", url).CombinedOutput()
		if err != nil {
			return fmt.Errorf("gio mount: %s: %w", strings.TrimSpace(string(out)), err)
		}
		// Construct the GVFS mount path. Format is stable across GVFS
		// versions: dav:host=HOST,port=PORT (URL-encoded for `:` etc).
		host, port, _ := strings.Cut(localAddr, ":")
		uid := os.Getuid()
		path := fmt.Sprintf("/run/user/%d/gvfs/dav:host=%s,port=%s", uid, host, port)
		sp.mu.Lock()
		sp.mountPath = path
		sp.mu.Unlock()
		sp.pushLogLine("mounted at " + path)
		// Best-effort: open Files (xdg-open works on most desktops).
		_ = exec.Command("xdg-open", path).Start()
		return nil
	}
	return fmt.Errorf("automatic mount not supported on %s; open http://%s manually", runtime.GOOS, localAddr)
}

func parseNetUseDrive(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Drive ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	return ""
}

// cleanStaleClaudyDrives removes any existing `net use` mappings that
// point at 127.0.0.1 — those are leftovers from prior Claudy sessions
// (the previous random port is now closed, the drive letter is dead).
// We deliberately scope to localhost so a user's real SMB / DAV
// shares stay attached.
//
// `net use` output format (English locale):
//
//	Status       Local      Remote                Network
//	---------------------------------------------------------------
//	OK           Y:         \\127.0.0.1@54321\DavWWWRoot   Web Client Network
//	Disconnected Z:         \\127.0.0.1@61234\DavWWWRoot   Web Client Network
//
// Other locales translate the headers but leave drive letters and
// remote paths intact, so the column-by-token parse below works on
// non-English Windows too.
func cleanStaleClaudyDrives() {
	out, err := exec.Command("net", "use").CombinedOutput()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Look for mappings that contain 127.0.0.1 anywhere on the line.
		if !strings.Contains(line, "127.0.0.1") {
			continue
		}
		// Extract the drive letter (token ending in ":" — exactly two
		// chars, an ASCII letter then a colon).
		var drive string
		for _, tok := range strings.Fields(line) {
			if len(tok) == 2 && tok[1] == ':' && ((tok[0] >= 'A' && tok[0] <= 'Z') || (tok[0] >= 'a' && tok[0] <= 'z')) {
				drive = tok
				break
			}
		}
		if drive == "" {
			continue
		}
		_ = exec.Command("net", "use", drive, "/delete", "/y").Run()
	}
}

func unmountBin() string {
	switch runtime.GOOS {
	case "windows":
		return "net"
	case "linux":
		return "gio"
	}
	return "umount"
}

func unmountArgs(mountPath string) []string {
	switch runtime.GOOS {
	case "windows":
		// mountPath is a drive letter like "Z:"; net use /delete frees it.
		return []string{"use", mountPath, "/delete", "/y"}
	case "linux":
		// We mounted via gio; reconstruct the dav:// URL from our recorded
		// path. mountPath looks like /run/user/UID/gvfs/dav:host=H,port=P.
		// `gio mount -u <path>` also works but the URL form is more
		// resilient to future gvfs path-format tweaks.
		const prefix = "dav:host="
		if i := strings.Index(mountPath, prefix); i >= 0 {
			rest := mountPath[i+len(prefix):]
			host, port, ok := strings.Cut(rest, ",port=")
			if ok {
				return []string{"mount", "-u", "dav://" + host + ":" + port + "/"}
			}
		}
		// Fall back to path-form unmount if parse failed.
		return []string{"mount", "-u", mountPath}
	}
	return []string{mountPath}
}

// openBrowser is best-effort; if it fails the user can still copy the
// URL Claudy printed on stdout.
func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", url).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "linux":
		_ = exec.Command("xdg-open", url).Start()
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "UI HTTP listen address (0 = random)")
	noBrowser := flag.Bool("no-browser", false, "do not auto-open the browser")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	exePath, err := os.Executable()
	if err != nil {
		log.Error("os.Executable", "err", err)
		os.Exit(1)
	}
	binDir := filepath.Dir(exePath)
	app := newAppState(log, binDir)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	url := "http://" + ln.Addr().String() + "/"
	log.Info("claudy UI", "url", url)
	fmt.Println()
	fmt.Printf("  Claudy UI running at %s\n", url)
	fmt.Println("  (close this window to quit)")
	fmt.Println()

	if !*noBrowser {
		openBrowser(url)
	}

	srv := &http.Server{Handler: app.httpHandlers()}
	srvCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("http.Serve", "err", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	app.stopAll()
	shutdownCtx, c2 := context.WithTimeout(srvCtx, 3*time.Second)
	defer c2()
	_ = srv.Shutdown(shutdownCtx)
}
