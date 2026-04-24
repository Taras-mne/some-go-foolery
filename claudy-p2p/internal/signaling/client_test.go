package signaling

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// TestClient_ConcurrentSend spins up a real gorilla WebSocket server,
// connects a Client, then fires N concurrent Send calls. With -race
// this catches the pre-fix bug where two goroutines invoked WriteJSON
// on the same conn simultaneously. Without the writeMu in Client.Send,
// this test tripped the Go race detector on the gorilla conn's write
// state; with it, -race stays silent and every frame the server
// reads back parses cleanly.
func TestClient_ConcurrentSend(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}

	// Minimal echo-ish server: read every envelope, verify it parses,
	// count kinds. Only one reader goroutine → matches gorilla's 1R+1W
	// rule on the server side.
	gotMu := sync.Mutex{}
	got := map[string]int{}
	serverDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		defer close(serverDone)
		// First frame: join. Then stream of sdp/ice.
		for {
			var e Envelope
			if err := conn.ReadJSON(&e); err != nil {
				return
			}
			gotMu.Lock()
			got[e.Kind]++
			gotMu.Unlock()
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/signal"

	c, err := Dial(wsURL, "room-x", "viewer", "pubkeyB64")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	const workers = 8
	const perWorker = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		kind := "sdp"
		if w%2 == 1 {
			kind = "ice"
		}
		go func(kind string) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				payload := map[string]any{"i": i, "kind": kind}
				if err := c.Send(kind, payload); err != nil {
					t.Errorf("send %s #%d: %v", kind, i, err)
					return
				}
			}
		}(kind)
	}
	wg.Wait()

	// Close our side; server will exit its read loop.
	_ = c.Close()
	<-serverDone

	gotMu.Lock()
	defer gotMu.Unlock()
	// 1 join + workers*perWorker envelopes, split evenly between sdp/ice.
	totalEnv := got["join"] + got["sdp"] + got["ice"]
	if totalEnv != 1+workers*perWorker {
		t.Errorf("server saw %d envelopes; want %d", totalEnv, 1+workers*perWorker)
	}
	if got["join"] != 1 {
		t.Errorf("join count = %d, want 1", got["join"])
	}
	expectPerKind := workers / 2 * perWorker
	if got["sdp"] != expectPerKind {
		t.Errorf("sdp count = %d, want %d", got["sdp"], expectPerKind)
	}
	if got["ice"] != expectPerKind {
		t.Errorf("ice count = %d, want %d", got["ice"], expectPerKind)
	}
}

// TestClient_SendEncoding sanity-checks that a payload round-trips
// through Envelope.Payload as a JSON-encoded string, which is the
// contract the rest of the code relies on.
func TestClient_SendEncoding(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var got Envelope
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()
		_ = conn.ReadJSON(&Envelope{}) // swallow join
		_ = conn.ReadJSON(&got)
		close(done)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/signal"
	c, err := Dial(wsURL, "r", "viewer", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	type payload struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	if err := c.Send("sdp", payload{A: 7, B: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	<-done

	if got.Kind != "sdp" {
		t.Errorf("kind = %q, want sdp", got.Kind)
	}
	var p payload
	if err := json.Unmarshal([]byte(got.Payload), &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.A != 7 || p.B != "hi" {
		t.Errorf("payload = %+v, want {7 hi}", p)
	}
}
