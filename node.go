package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
	"golang.org/x/crypto/bcrypt"
)

type Driver struct {
	afero.Fs
	username     string
	passwordHash string
}

func (d *Driver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	// Проверяем логин и сверяем хеш пароля
	if user == d.username {
		if err := bcrypt.CompareHashAndPassword([]byte(d.passwordHash), []byte(pass)); err == nil {
			return d, nil
		}
	}
	return nil, fmt.Errorf("access denied")
}

func (d *Driver) GetSettings() (*ftpserver.Settings, error) {
	return &ftpserver.Settings{ListenAddr: ":2121", DisableActiveMode: true}, nil
}
func (d *Driver) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	return "Welcome to Node", nil
}
func (d *Driver) ClientDisconnected(cc ftpserver.ClientContext) {}
func (d *Driver) GetTLSConfig() (*tls.Config, error)            { return nil, nil }

func registerOnHub(hubURL, alias, myAddr, username, password string) {
	// Хешируем пароль перед отправкой на Hub
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Println("❌ Hashing error")
		return
	}

	payload := fmt.Sprintf(`{"alias":"%s","addr":"%s","username":"%s","password":"%s"}`, 
		alias, myAddr, username, string(hash))

	for i := 0; i < 20; i++ {
		resp, err := http.Post(hubURL+"/register", "application/json", bytes.NewBufferString(payload))
		if err == nil && resp.StatusCode == 200 {
			fmt.Println("✅ Registered on Hub!")
			return
		}
		fmt.Printf("⏳ Waiting for Hub... (%d)\n", i+1)
		time.Sleep(3 * time.Second)
	}
}

func main() {
	// Usage: go run node.go [HUB_URL] [ALIAS] [MY_IP] [USERNAME] [PASSWORD]
	if len(os.Args) < 6 {
		fmt.Println("Usage: go run node.go [HUB_URL] [ALIAS] [MY_IP] [USERNAME] [PASSWORD]")
		os.Exit(1)
	}
	hubURL, alias, myIP, username, password := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]
	myAddr := fmt.Sprintf("%s:2121", myIP)
	rootDir := "./ftp_storage"
	os.MkdirAll(rootDir, 0755)

	// Хешируем пароль для локального хранения
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Println("❌ Hashing error")
		os.Exit(1)
	}

	go registerOnHub(hubURL, alias, myAddr, username, password)

	fmt.Printf("📁 Node on :2121 | Alias: %s | User: %s\n", alias, username)

	baseFs := afero.NewBasePathFs(afero.NewOsFs(), rootDir)
	driver := &Driver{
		Fs:           baseFs,
		username:     username,
		passwordHash: string(hash),
	}

	server := ftpserver.NewFtpServer(driver)
	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("❌ %v\n", err)
		os.Exit(1)
	}
}