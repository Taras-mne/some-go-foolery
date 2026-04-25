// Signaling server: pairs two WebSocket clients by room ID and forwards
// SDP/ICE envelopes between them. No payload inspection, no storage
// besides the transient pubkey used to announce each peer's identity
// to the counterpart at pairing time.
//
// Resilience:
//   - Keepalive: server pings every keepaliveInterval and closes the
//     WebSocket if no pong comes back within pongTimeout. Without this
//     a client that disappears ungracefully (power off, Ctrl+C where
//     the OS never sends FIN) stays "alive" on the server until the
//     kernel's default TCP timeout — on Linux that's ~2 minutes of
//     "role conflict" errors for the real owner trying to rejoin.
//   - Force-takeover: a fresh join for an already-filled role closes
//     the incumbent and replaces it. Matches user intent — "I just
//     restarted, this IS me" — and works around any residual stale
//     WS that the keepalive hasn't caught yet. Safe because every
//     client authenticates with Noise at the DataChannel layer; the
//     signaling server has no authority to protect.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/claudy/p2p/internal/signaling"
	"github.com/gorilla/websocket"
)

// Keepalive tuning. Originally 25 s pong timeout matched ~2.5 × ping
// interval — the textbook ratio. In practice we observed clients on
// VPN / mobile / heavily-NATed networks losing one ping due to
// transient TCP retransmits and getting kicked despite a perfectly
// alive process on the other end. Each kick triggered a redial →
// "ready" envelope on counterpart → forced PeerConnection rebuild,
// which interrupts in-flight WebDAV transfers. 60 s is permissive
// enough to ride out a 30-second NAT hiccup or an antivirus stall
// while still closing genuinely dead sockets within a minute.
const (
	keepaliveInterval = 10 * time.Second
	pongTimeout       = 60 * time.Second
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

// room holds the two paired peers.
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

	// Keepalive: read deadline extended by pong handler; writer pings
	// periodically. Mismatch → close and let pump's defer clean up.
	conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	// First frame must be a join.
	var join signaling.Envelope
	if err := conn.ReadJSON(&join); err != nil || join.Kind != "join" || join.Room == "" {
		_ = conn.WriteJSON(signaling.Envelope{Kind: "error", Error: "expected join{room,role,pubkey}"})
		_ = conn.Close()
		return
	}

	me := &peerConn{ws: conn, pubkey: join.Pubkey}
	peer, displaced, ok := h.pair(join.Room, join.Role, me)
	if !ok {
		_ = conn.WriteJSON(signaling.Envelope{Kind: "error", Error: "role conflict or room full"})
		_ = conn.Close()
		return
	}
	if displaced != nil {
		// Force-takeover happened — previous incumbent's WS is now
		// closed, its pump will return on next ReadJSON and skip the
		// drop because the slot's pointer no longer matches.
		h.log.Info("displaced stale peer", "room", join.Room, "role", join.Role)
	}

	h.log.Info("joined", "room", join.Room, "role", join.Role, "pubkey", truncKey(join.Pubkey))

	// The second party to join a room triggers "ready" on BOTH sides,
	// regardless of which role. Fires on first pairing AND on every
	// reconnect, so the peer supervisor can drive a fresh SDP exchange.
	if peer != nil {
		_ = peer.ws.WriteJSON(signaling.Envelope{Kind: "ready", Pubkey: join.Pubkey})
		_ = conn.WriteJSON(signaling.Envelope{Kind: "ready", Pubkey: peer.pubkey})
	}

	// Ping pump: sends a ping every keepaliveInterval until the WS is
	// closed or pump() exits. Exits cleanly when writes fail.
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(keepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			}
		}
	}()
	h.pump(join.Room, join.Role, me)
	close(done)
}

// pair places p into its role slot. If that slot is already taken the
// incumbent is force-closed and displaced is returned so the caller
// can log it. Always returns the counterpart on success, or nil if
// the room is fresh.
func (h *hub) pair(id, role string, p *peerConn) (peer, displaced *peerConn, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, exists := h.rooms[id]
	if !exists {
		r = &room{}
		h.rooms[id] = r
	}
	switch role {
	case "owner":
		displaced = r.owner
		if displaced != nil {
			_ = displaced.ws.Close()
		}
		r.owner = p
		return r.viewer, displaced, true
	case "viewer":
		displaced = r.viewer
		if displaced != nil {
			_ = displaced.ws.Close()
		}
		r.viewer = p
		return r.owner, displaced, true
	}
	return nil, nil, false
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

// drop clears this peer's slot ONLY if it still holds the slot. If we
// were displaced by force-takeover, r.owner/r.viewer points to a
// different peerConn and we must not touch it — otherwise we'd evict
// the new, live peer.
func (h *hub) drop(id, role string, self *peerConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[id]
	if r == nil {
		return
	}
	if role == "owner" && r.owner == self {
		r.owner = nil
	}
	if role == "viewer" && r.viewer == self {
		r.viewer = nil
	}
	if r.owner == nil && r.viewer == nil {
		delete(h.rooms, id)
	}
}

// pump forwards envelopes from this peer to its counterpart until the
// WS errors. Always cleans up its own slot (safely, via drop).
func (h *hub) pump(id, role string, me *peerConn) {
	defer func() {
		_ = me.ws.Close()
		h.drop(id, role, me)
		h.log.Info("left", "room", id, "role", role)
	}()
	for {
		var e signaling.Envelope
		if err := me.ws.ReadJSON(&e); err != nil {
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
