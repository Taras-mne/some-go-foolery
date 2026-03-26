package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jlaffaye/ftp"
)

func main() {
	// Usage: go run client.go [host:port] [username] [password] <local_path> <remote_path>
	if len(os.Args) < 6 {
		fmt.Println("Usage: go run client.go [host:port] [username] [password] <local_path> <remote_path>")
		os.Exit(1)
	}
	host := os.Args[1]
	username := os.Args[2]
	password := os.Args[3]
	srcPath := os.Args[4]
	destPath := os.Args[5]

	fmt.Printf("🔌 Connecting to %s as %s...\n", host, username)
	c, err := ftp.Dial(host)
	if err != nil {
		fmt.Printf("❌ Connect error: %v\n", err)
		os.Exit(1)
	}
	defer c.Quit()

	if err := c.Login(username, password); err != nil {
		fmt.Printf("❌ Login error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ Logged in")

	_ = c.MakeDir(destPath)

	fmt.Printf("📤 Uploading %s → %s\n", srcPath, destPath)
	if err := uploadDir(c, srcPath, destPath); err != nil {
		fmt.Printf("❌ Upload error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ Done!")
}

func uploadDir(c *ftp.ServerConn, localDir, remoteDir string) error {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		localPath := filepath.Join(localDir, entry.Name())
		remotePath := filepath.ToSlash(filepath.Join(remoteDir, entry.Name()))
		if entry.IsDir() {
			fmt.Printf("   📂 %s/\n", entry.Name())
			_ = c.MakeDir(remotePath)
			if err := uploadDir(c, localPath, remotePath); err != nil {
				return err
			}
		} else {
			fmt.Printf("   📄 %s\n", entry.Name())
			if err := uploadFile(c, localPath, remotePath); err != nil {
				return err
			}
		}
	}
	return nil
}

func uploadFile(c *ftp.ServerConn, localPath, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()
	return c.Stor(remotePath, file)
}