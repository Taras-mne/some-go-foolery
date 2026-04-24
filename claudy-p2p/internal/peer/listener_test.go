package peer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListener_AcceptReceivesPushedConn(t *testing.T) {
	ch := make(chan *Conn, 1)
	l := NewListener(ch, "t")
	defer l.Close()

	client, server := pairConns(t)
	defer client.Close()
	defer server.Close()

	ch <- server

	got, err := l.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got != server {
		t.Errorf("got wrong conn")
	}
}

func TestListener_CloseUnblocksAccept(t *testing.T) {
	ch := make(chan *Conn)
	l := NewListener(ch, "t")

	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	_ = l.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrListenerClosed) {
			t.Errorf("err = %v, want ErrListenerClosed", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Accept did not unblock")
	}
}

func TestListener_ChannelCloseEndsAccept(t *testing.T) {
	ch := make(chan *Conn)
	l := NewListener(ch, "t")
	defer l.Close()

	close(ch)
	_, err := l.Accept()
	if !errors.Is(err, ErrListenerClosed) {
		t.Errorf("err = %v, want ErrListenerClosed", err)
	}
}

// TestListener_HTTPServerRoundTrip is the real acceptance test for Step 2:
// run http.Serve on the Listener, hit it with http.Client that dials the
// matched peer Conn, get a 200 OK back. This proves any http.Handler
// (including webdav.Handler) works unmodified on top of DataChannels.
func TestListener_HTTPServerRoundTrip(t *testing.T) {
	client, server := pairConns(t)
	defer client.Close()
	defer server.Close()

	ch := make(chan *Conn, 1)
	ch <- server
	l := NewListener(ch, "http")
	defer l.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Peer", "yes")
		_, _ = w.Write([]byte("hi " + r.URL.Query().Get("name")))
	})
	srv := &http.Server{Handler: mux}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(l) }()
	defer srv.Close()

	// Custom transport: return the client-side Conn exactly once. Any
	// retry/reuse attempt will see io.ErrClosedPipe, which is fine because
	// we only issue one request.
	used := false
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if used {
				return nil, io.ErrClosedPipe
			}
			used = true
			return client, nil
		},
		DisableKeepAlives: true,
	}
	hc := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := hc.Get("http://peer/hello?name=claudy")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Peer"); got != "yes" {
		t.Errorf("X-Peer = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hi claudy") {
		t.Errorf("body = %q", body)
	}
}
