package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

func hashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

type Multistatus struct {
	XMLName   xml.Name   `xml:"D:multistatus"`
	Xmlns     string     `xml:"xmlns:D,attr"`
	Responses []Response `xml:"D:response"`
}

type Response struct {
	Href     string   `xml:"D:href"`
	Propstat Propstat `xml:"D:propstat"`
}

type Propstat struct {
	Prop   Prop   `xml:"D:prop"`
	Status string `xml:"D:status"`
}

type Prop struct {
	DisplayName      string       `xml:"D:displayname"`
	GetContentLength string       `xml:"D:getcontentlength,omitempty"`
	ResourceType     *Collection  `xml:"D:resourcetype"`
}

type Collection struct {
	Collection *struct{} `xml:"D:collection"`
}

var mu sync.Mutex

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: client <hub_address> <alias> <password>")
		os.Exit(1)
	}
	hubAddr := os.Args[1]
	alias := os.Args[2]
	password := os.Args[3]

	conn, err := net.Dial("tcp", hubAddr)
	if err != nil {
		fmt.Println("Error connecting to Hub:", err)
		return
	}
	defer conn.Close()

	passHash := hashPassword(password)
	fmt.Fprintf(conn, "CLIENT %s %s\n", alias, passHash)

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "200 OK") {
		fmt.Println("Auth failed or Hub error:", line)
		return
	}
	fmt.Println("Authenticated. Tunnel established.")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handleWebDAV(w, r, conn, reader)
	})

	fmt.Println("WebDAV Server started at http://localhost:8080")
	fmt.Println("Map this address as a Network Drive in your File Manager.")
	http.ListenAndServe(":8080", nil)
}

func handleWebDAV(w http.ResponseWriter, r *http.Request, conn net.Conn, reader *bufio.Reader) {
	path := r.URL.Path
	if path == "" {
		path = "/"
	}

	reqPath := strings.TrimPrefix(path, "/")
	if reqPath == "" {
		reqPath = "."
	}

	switch r.Method {
	case "OPTIONS":
		w.Header().Set("DAV", "1")
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, MKCOL")
		w.WriteHeader(http.StatusOK)

	case "PROPFIND":
		mu.Lock()
		defer mu.Unlock()

		fmt.Fprintf(conn, "LIST\n")

		status, _ := reader.ReadString('\n')
		if !strings.Contains(status, "200 OK") {
			http.Error(w, "Node Error", http.StatusInternalServerError)
			return
		}

		var files []string
		for {
			l, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			l = strings.TrimSpace(l)
			if l == "---END---" {
				break
			}
			if l != "" {
				files = append(files, l)
			}
		}

		ms := Multistatus{Xmlns: "DAV:"}

		// Root collection entry — required for Windows/macOS to mount the drive
		ms.Responses = append(ms.Responses, Response{
			Href: path,
			Propstat: Propstat{
				Status: "HTTP/1.1 200 OK",
				Prop: Prop{
					DisplayName:  "",
					ResourceType: &Collection{Collection: &struct{}{}},
				},
			},
		})

		for _, f := range files {
			href := path
			if !strings.HasSuffix(href, "/") {
				href += "/"
			}
			href += f
			ms.Responses = append(ms.Responses, Response{
				Href: href,
				Propstat: Propstat{
					Status: "HTTP/1.1 200 OK",
					Prop: Prop{
						DisplayName:      f,
						GetContentLength: "0",
						ResourceType:     &Collection{},
					},
				},
			})
		}

		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		xmlData, _ := xml.MarshalIndent(ms, "", "  ")
		w.Write(xmlData)

	case "GET":
		mu.Lock()
		defer mu.Unlock()

		fmt.Fprintf(conn, "GET %s\n", reqPath)
		status, _ := reader.ReadString('\n')
		if !strings.Contains(status, "200 OK") {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		parts := strings.Fields(strings.TrimSpace(status))
		if len(parts) < 3 {
			http.Error(w, "Invalid response from node", http.StatusInternalServerError)
			return
		}
		size, _ := strconv.ParseInt(parts[2], 10, 64)

		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		io.CopyN(w, reader, size)
		reader.ReadByte() // consume trailing newline

	case "PUT":
		mu.Lock()
		defer mu.Unlock()

		size := r.ContentLength
		fmt.Fprintf(conn, "PUT %s %d\n", reqPath, size)

		ack, _ := reader.ReadString('\n')
		if !strings.Contains(ack, "200 OK") {
			http.Error(w, "Upload failed", http.StatusInternalServerError)
			return
		}

		io.Copy(conn, r.Body)
		conn.Write([]byte("\n"))

		w.WriteHeader(http.StatusCreated)
	}
}
