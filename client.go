package main
import (
	"fmt"
	"os"
	"path/filepath"
	"github.com/jlaffaye/ftp"
)
func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: go run client.go [host:port] <local_path> <remote_path>")
		os.Exit(1)
	}
	host := os.Args[1]
	srcPath := os.Args[2]
	destPath := os.Args[3]

	fmt.Printf("🔌 Connecting to %s...\n", host)
	c, err := ftp.Dial(host)
	if err != nil {
		fmt.Printf("❌ Connect error: %v\n", err)
		os.Exit(1)
	}
	defer c.Quit()

	if err := c.Login("kotik", "12345"); err != nil {
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
	if err != nil { return err }
	for _, entry := range entries {
		localPath := filepath.Join(localDir, entry.Name())
		remotePath := filepath.ToSlash(filepath.Join(remoteDir, entry.Name()))
		if entry.IsDir() {
			fmt.Printf("   📂 %s/\n", entry.Name())
			_ = c.MakeDir(remotePath)
			if err := uploadDir(c, localPath, remotePath); err != nil { return err }
		} else {
			fmt.Printf("   📄 %s\n", entry.Name())
			if err := uploadFile(c, localPath, remotePath); err != nil { return err }
		}
	}
	return nil
}
func uploadFile(c *ftp.ServerConn, localPath, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil { return err }
	defer file.Close()
	return c.Stor(remotePath, file)
}