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
)

type Driver struct {
	afero.Fs
}

func (d *Driver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user == "admin" && pass == "12345" {
		return d, nil
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

func registerOnHub(hubURL, alias, myAddr string) {
	payload := fmt.Sprintf(`{"alias":"%s","addr":"%s"}`, alias, myAddr)
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
	if len(os.Args) < 4 {
		fmt.Println("Usage: go run node.go [HUB_URL] [ALIAS] [MY_IP]")
		os.Exit(1)
	}
	hubURL, alias, myIP := os.Args[1], os.Args[2], os.Args[3]
	myAddr := fmt.Sprintf("%s:2121", myIP)

	rootDir := "./ftp_storage"
	os.MkdirAll(rootDir, 0755)

	go registerOnHub(hubURL, alias, myAddr)

	fmt.Printf("📁 Node on :2121 | Alias: %s | Storage: %s\n", alias, rootDir)

	baseFs := afero.NewBasePathFs(afero.NewOsFs(), rootDir)
	driver := &Driver{Fs: baseFs}

	server := ftpserver.NewFtpServer(driver)
	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("❌ %v\n", err)
		os.Exit(1)
	}
}