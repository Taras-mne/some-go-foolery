// Signaling server: pairs two WebSocket clients by room ID and forwards
// SDP/ICE envelopes between them. No payload inspection, no storage
// besides the transient pubkey used to announce each peer's identity
// to the counterpart at pairing time.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/claudy/p2p/internal/signaling"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// peerConn pairs a WebSocket with the pubkey the joiner announced. The
// server never validates the key — it just forwards it so the other
// side can TOFU-check.
type peerConn struct {
	ws     *websocket.Conn
	pubkey string
}

// room holds the two paired peers. owner joins first.
type room struct {
	owner  *peerConn
	viewer *peerConn
}

type hub struct {
	mu    sync.Mutex
	rooms map[string]*room
	log   *slog.Logger
}

func newHub(log *slog.Logger) *hub {
	return &hub{rooms: map[string]*room{}, log: log}
}

func (h *hub) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("upgrade failed", "err", err)
		return
	}

	// First frame must be a join.
	var join signaling.Envelope
	if err := conn.ReadJSON(&join); err != nil || join.Kind != "join" || join.Room == "" {
		_ = conn.WriteJSON(signaling.Envelope{Kind: "error", Error: "expected join{room,role,pubkey}"})
		_ = conn.Close()
		return
	}

	peer, ok := h.pair(join.Room, join.Role, &peerConn{ws: conn, pubkey: join.Pubkey})
	if !ok {
		_ = conn.WriteJSON(signaling.Envelope{Kind: "error", Error: "role conflict or room full"})
		_ = conn.Close()
		return
	}

	h.log.Info("joined", "room", join.Room, "role", join.Role, "pubkey", truncKey(join.Pubkey))

	// The second party to join the room triggers "ready" on BOTH sides,
	// regardless of whether owner or viewer was first. The original code
	// only fired ready when viewer arrived last, which produced a silent
	// deadlock on two realistic flows:
	//   1. Viewer is long-running (auto-start on laptop boot); owner
	//      launches later — ready never fires.
	//   2. Owner process crashes and restarts while viewer is still in
	//      the room — ready never fires on the fresh owner.
	// Making both arrival orders symmetrical also matches user intuition
	// ("both are in → both get paired") and keeps the supervisor's
	// rebuild-on-failure path working across owner restarts.
	if peer != nil {
		_ = peer.ws.WriteJSON(signaling.Envelope{Kind: "ready", Pubkey: join.Pubkey})
		_ = conn.WriteJSON(signaling.Envelope{Kind: "ready", Pubkey: peer.pubkey})
	}

	h.pump(join.Room, join.Role, conn)
}

func (h *hub) pair(id, role string, p *peerConn) (peer *peerConn, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, exists := h.rooms[id]
	if !exists {
		r = &room{}
		h.rooms[id] = r
	}
	switch role {
	case "owner":
		if r.owner != nil {
			return nil, false
		}
		r.owner = p
		return r.viewer, true
	case "viewer":
		if r.viewer != nil {
			return nil, false
		}
		r.viewer = p
		return r.owner, true
	}
	return nil, false
}

func (h *hub) counterpart(id, myRole string) *websocket.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[id]
	if r == nil {
		return nil
	}
	if myRole == "owner" {
		if r.viewer == nil {
			return nil
		}
		return r.viewer.ws
	}
	if r.owner == nil {
		return nil
	}
	return r.owner.ws
}

func (h *hub) drop(id, role string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[id]
	if r == nil {
		return
	}
	if role == "owner" {
		r.owner = nil
	} else {
		r.viewer = nil
	}
	if r.owner == nil && r.viewer == nil {
		delete(h.rooms, id)
	}
}

// pump forwards envelopes from this conn to its counterpart until EOF.
func (h *hub) pump(id, role string, c *websocket.Conn) {
	defer func() {
		_ = c.Close()
		h.drop(id, role)
		h.log.Info("left", "room", id, "role", role)
	}()
	for {
		var e signaling.Envelope
		if err := c.ReadJSON(&e); err != nil {
			return
		}
		peer := h.counterpart(id, role)
		if peer == nil {
			continue // counterpart gone; drop silently
		}
		if err := peer.WriteJSON(e); err != nil {
			return
		}
	}
}

// truncKey shortens a base64 pubkey to a log-friendly prefix.
func truncKey(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}

func main() {
	addr := flag.String("addr", ":7000", "listen address")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newHub(log)

	mux := http.NewServeMux()
	mux.HandleFunc("/signal", h.handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	log.Info("signal listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}
