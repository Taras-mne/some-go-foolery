package peer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/webdav"
)

// TestWebDAV_OverP2P is the Step-3 acceptance test: a real
// golang.org/x/net/webdav.Handler serves a tempdir filesystem over a
// DataChannel-backed net.Listener, and an http.Client driving a
// DataChannel-backed net.Conn does PUT → GET → PROPFIND against it.
//
// If this passes, the transport is a drop-in replacement for the existing
// WebSocket tunnel: no webdav changes needed anywhere.
func TestWebDAV_OverP2P(t *testing.T) {
	tempdir := t.TempDir()

	// Server side: pair a Conn, feed into Listener, run webdav.Handler.
	client, server := pairConns(t)
	defer client.Close()
	defer server.Close()

	ch := make(chan *Conn, 1)
	ch <- server
	l := NewListener(ch, "webdav")
	defer l.Close()

	handler := &webdav.Handler{
		FileSystem: webdav.Dir(tempdir),
		LockSystem: webdav.NewMemLS(),
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	// Client transport reuses the one Conn via keep-alive; a second Dial
	// would return ErrOneShot, which we never expect to hit.
	dialed := false
	errOneShot := errors.New("one-shot transport already dialed")
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dialed {
				return nil, errOneShot
			}
			dialed = true
			return client, nil
		},
		// MaxConnsPerHost=1 + keep-alive ensures reuse across requests.
		MaxConnsPerHost: 1,
	}
	hc := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	// 1. PUT a file.
	const content = "hello from the other side of the data channel"
	putReq, _ := http.NewRequest(http.MethodPut, "http://peer/notes.txt", strings.NewReader(content))
	putResp, err := hc.Do(putReq)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	io.Copy(io.Discard, putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusCreated && putResp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d", putResp.StatusCode)
	}

	// Verify it really landed on disk.
	disk, err := os.ReadFile(filepath.Join(tempdir, "notes.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(disk) != content {
		t.Errorf("disk content = %q, want %q", disk, content)
	}

	// 2. GET the same file.
	getResp, err := hc.Get("http://peer/notes.txt")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", getResp.StatusCode)
	}
	if string(body) != content {
		t.Errorf("GET body = %q, want %q", body, content)
	}

	// 3. PROPFIND on the root — must list notes.txt.
	pfReq, _ := http.NewRequest("PROPFIND", "http://peer/", nil)
	pfReq.Header.Set("Depth", "1")
	pfResp, err := hc.Do(pfReq)
	if err != nil {
		t.Fatalf("PROPFIND: %v", err)
	}
	pfBody, _ := io.ReadAll(pfResp.Body)
	pfResp.Body.Close()
	if pfResp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND status = %d, body = %s", pfResp.StatusCode, pfBody)
	}
	if !strings.Contains(string(pfBody), "notes.txt") {
		t.Errorf("PROPFIND did not list notes.txt; body = %s", pfBody)
	}
}
