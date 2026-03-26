package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fclairamb/ftpserverlib"
	"github.com/jlaffaye/ftp"
	"github.com/spf13/afero"
	"golang.org/x/crypto/bcrypt"
)

// NodeInfo хранит данные о подключении к узлу и хеш пароля
type NodeInfo struct {
	Addr       string `json:"addr"`
	Username   string `json:"username"` // Логин на самом узле (например, "admin" или уникальный)
	PasswordHash string `json:"-"`      // Хеш пароля (не передаётся по сети)
}

var aliases = sync.Map{} // map[string]*NodeInfo

type RegReq struct {
	Alias    string `json:"alias"`
	Addr     string `json:"addr"`
	Username string `json:"username"`
	Password string `json:"password"` // Передаётся только при регистрации, сразу хешируется
}

// --- HTTP: регистрация нод ---
func registerHandler(w http.ResponseWriter, r *http.Request) {
	var req RegReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Alias == "" || req.Addr == "" || req.Password == "" {
		http.Error(w, "alias, addr and password required", http.StatusBadRequest)
		return
	}

	// Создаём солёный хеш пароля
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "hashing error", http.StatusInternalServerError)
		return
	}

	info := &NodeInfo{
		Addr:       req.Addr,
		Username:   req.Username,
		PasswordHash: string(hash),
	}
	aliases.Store(req.Alias, info)
	fmt.Printf("📡 [HUB] Registered: %s -> %s (user: %s)\n", req.Alias, req.Addr, req.Username)
	w.WriteHeader(http.StatusOK)
}

// --- HubDriver ---
type HubDriver struct {
	afero.Fs
}

func (d *HubDriver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	val, ok := aliases.Load(user)
	if !ok {
		return nil, fmt.Errorf("unknown alias '%s'", user)
	}
	info := val.(*NodeInfo)

	// 1. Проверяем пароль против хеша на стороне Hub
	if err := bcrypt.CompareHashAndPassword([]byte(info.PasswordHash), []byte(pass)); err != nil {
		fmt.Printf("🔒 [HUB] Auth failed for %s: invalid password\n", user)
		return nil, fmt.Errorf("access denied")
	}

	fmt.Printf("🔑 [HUB] Login: %s -> %s (as %s)\n", user, info.Addr, info.Username)

	// 2. Проверяем доступность ноды (техническое подключение)
	c, err := ftp.Dial(info.Addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("node unavailable: %v", err)
	}
	// Тут можно использовать внутренний логин ноды, если он отличается от клиентского
	// Для примера используем тот, что указан при регистрации
	if err := c.Login(info.Username, pass); err != nil {
		c.Quit()
		return nil, fmt.Errorf("node login failed: %v", err)
	}
	c.Quit()

	return &ProxyDriver{
		Fs:          d.Fs,
		backendAddr: info.Addr,
		username:    info.Username,
		pass:        pass,
	}, nil
}

func (d *HubDriver) GetSettings() (*ftpserver.Settings, error) {
	return &ftpserver.Settings{
		ListenAddr:        ":2122",
		DisableActiveMode: true,
		IdleTimeout:       900,
	}, nil
}
func (d *HubDriver) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	return "Hub: login with your alias", nil
}
func (d *HubDriver) ClientDisconnected(cc ftpserver.ClientContext) {}
func (d *HubDriver) GetTLSConfig() (*tls.Config, error)            { return nil, nil }

// --- ProxyDriver ---
type ProxyDriver struct {
	afero.Fs
	backendAddr string
	username    string
	pass        string
}

func (p *ProxyDriver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	return p, nil
}
func (p *ProxyDriver) GetSettings() (*ftpserver.Settings, error) { return nil, nil }
func (p *ProxyDriver) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	return "Proxied to node", nil
}
func (p *ProxyDriver) ClientDisconnected(cc ftpserver.ClientContext) {}
func (p *ProxyDriver) GetTLSConfig() (*tls.Config, error)            { return nil, nil }

func (p *ProxyDriver) getConn() (ftp.ServerConn, error) {
	c, err := ftp.Dial(p.backendAddr, ftp.DialWithTimeout(10*time.Second))
	if err != nil {
		return nil, err
	}
	// Используем учётные данные, проверенные на уровне Hub
	if err := c.Login(p.username, p.pass); err != nil {
		c.Quit()
		return nil, err
	}
	return c, nil
}

// ... (методы GetFile, PutFile, memFile оставляем как в предыдущем примере, они не меняются) ...
// Для краткости опущены, логика работы с памятью та же.
// Важно: в продакшене нужно реализовать стриминг, а не загрузку в память.
func (p *ProxyDriver) GetFile(path string, offset int64) (afero.File, error) {
	c, err := p.getConn()
	if err != nil { return nil, err }
	defer c.Quit()
	r, err := c.Retr(path)
	if err != nil { return nil, err }
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil { return nil, err }
	return &memFile{ data, name: path}, nil
}
func (p *ProxyDriver) PutFile(path string, offset int64) (afero.File, error) {
	return &memFile{name: path, writable: true, proxy: p}, nil
}
func (p *ProxyDriver) Create(path string) (afero.File, error) {
	return p.PutFile(path, 0)
}

// --- memFile (реализация та же, что и ранее) ---
type memFile struct {
	data     []byte
	pos      int64
	name     string
	writable bool
	proxy    *ProxyDriver
	closed   bool
}
func (m *memFile) Read(p []byte) (int, error) {
	if m.pos >= int64(len(m.data)) { return 0, io.EOF }
	n := copy(p, m.data[m.pos:]); m.pos += int64(n); return n, nil
}
func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.data)) { return 0, io.EOF }
	n := copy(p, m.data[off:]); return n, nil
}
func (m *memFile) Write(p []byte) (int, error) {
	if !m.writable { return 0, fmt.Errorf("file not writable") }
	m.data = append(m.data, p...); return len(p), nil
}
func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	if !m.writable { return 0, fmt.Errorf("file not writable") }
	if off+int64(len(p)) > int64(len(m.data)) {
		newData := make([]byte, off+int64(len(p))); copy(newData, m.data); m.data = newData
	}
	copy(m.data[off:], p); return len(p), nil
}
func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0: m.pos = offset
	case 1: m.pos += offset
	case 2: m.pos = int64(len(m.data)) + offset
	}
	if m.pos < 0 { m.pos = 0 }
	if m.pos > int64(len(m.data)) { m.pos = int64(len(m.data)) }
	return m.pos, nil
}
func (m *memFile) Close() error {
	if m.closed { return nil }
	m.closed = true
	if m.writable && m.proxy != nil && len(m.data) > 0 {
		c, err := m.proxy.getConn()
		if err == nil { c.Stor(m.name, bytes.NewReader(m.data)); c.Quit() }
	}
	return nil
}
func (m *memFile) WriteString(s string) (int, error) {
	if !m.writable { return 0, fmt.Errorf("file not writable") }
	m.data = append(m.data, []byte(s)...); return len(s), nil
}
func (m *memFile) Name() string { return m.name }
func (m *memFile) Readdir(count int) ([]os.FileInfo, error) { return nil, fmt.Errorf("not a directory") }
func (m *memFile) Readdirnames(n int) ([]string, error) { return nil, fmt.Errorf("not a directory") }
func (m *memFile) Stat() (os.FileInfo, error) {
	return &memFileInfo{name: m.name, size: int64(len(m.data))}, nil
}
func (m *memFile) Sync() error { return nil }
func (m *memFile) Truncate(size int64) error {
	if size < int64(len(m.data)) { m.data = m.data[:size] }
	return nil
}

type memFileInfo struct {
	name string
	size int64
}
func (m *memFileInfo) Name() string       { return m.name }
func (m *memFileInfo) Size() int64        { return m.size }
func (m *memFileInfo) Mode() os.FileMode  { return 0644 }
func (m *memFileInfo) ModTime() time.Time { return time.Now() }
func (m *memFileInfo) IsDir() bool        { return false }
func (m *memFileInfo) Sys() interface{}   { return nil }

// --- Main ---
func main() {
	go func() {
		http.HandleFunc("/register", registerHandler)
		fmt.Println("🌐 [HUB] HTTP API on :8080")
		http.ListenAndServe(":8080", nil)
	}()
	driver := &HubDriver{Fs: afero.NewOsFs()}
	server := ftpserver.NewFtpServer(driver)
	fmt.Println("🚀 [HUB] FTP Proxy on :2122")
	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("❌ %v\n", err)
	}
}