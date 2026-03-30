package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func hashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

func serveNode(hubAddr, alias, password, rootDir string) error {
	conn, err := net.Dial("tcp", hubAddr)
	if err != nil {
		return fmt.Errorf("cannot connect to hub: %w", err)
	}
	defer conn.Close()

	passHash := hashPassword(password)
	fmt.Fprintf(conn, "NODE %s %s\n", alias, passHash)

	fmt.Println("Connected to Hub as Node:", alias)
	fmt.Println("Serving files from:", rootDir)

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("connection lost: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		cmd := parts[0]

		switch cmd {
		case "LIST":
			entries, _ := os.ReadDir(rootDir)
			var names []string
			for _, e := range entries {
				if !e.IsDir() {
					names = append(names, e.Name())
				}
			}
			fmt.Fprintf(conn, "200 OK\n%s\n---END---\n", strings.Join(names, "\n"))

		case "GET":
			if len(parts) < 2 {
				fmt.Fprintf(conn, "500 Error: No filename\n")
				continue
			}
			filename := parts[1]
			filePath := rootDir + "/" + filename

			f, err := os.Open(filePath)
			if err != nil {
				fmt.Fprintf(conn, "404 Not Found\n")
				continue
			}

			stat, _ := f.Stat()
			fmt.Fprintf(conn, "200 OK %d\n", stat.Size())
			io.Copy(conn, f)
			fmt.Fprintf(conn, "\n")
			f.Close()

		case "PUT":
			if len(parts) < 3 {
				fmt.Fprintf(conn, "500 Error: No filename or size\n")
				continue
			}
			filename := parts[1]
			size, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				fmt.Fprintf(conn, "500 Error: Invalid size\n")
				continue
			}

			filePath := rootDir + "/" + filename
			f, err := os.Create(filePath)
			if err != nil {
				fmt.Fprintf(conn, "500 Error: Cannot create file\n")
				continue
			}

			fmt.Fprintf(conn, "200 OK\n")

			limitedReader := io.LimitReader(reader, size)
			written, _ := io.Copy(f, limitedReader)

			reader.ReadByte() // consume trailing newline

			f.Close()
			fmt.Printf("Uploaded %s (%d bytes)\n", filename, written)
		}
	}
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: node <hub_address> <alias> <password>")
		os.Exit(1)
	}
	hubAddr := os.Args[1]
	alias := os.Args[2]
	password := os.Args[3]
	rootDir := "./shared_files"

	os.MkdirAll(rootDir, 0755)

	for {
		err := serveNode(hubAddr, alias, password, rootDir)
		if err != nil {
			fmt.Printf("[NODE] %v — reconnecting in 5s...\n", err)
			time.Sleep(5 * time.Second)
		}
	}
}
