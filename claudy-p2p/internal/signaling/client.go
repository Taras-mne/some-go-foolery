// Package signaling: client wrapper over a gorilla WebSocket connection.
//
// Concurrency contract (important): gorilla/websocket allows at most
// one concurrent reader and at most one concurrent writer per
// connection. Our callers respect the reader side — only a single
// signalLoop calls Recv — but writes come from two sources that run
// in parallel:
//
//   1. The main flow that sends "join" / "sdp" envelopes from the
//      supervisor / rebuild path.
//   2. pion's OnICECandidate callback, which fires from an internal
//      pion goroutine as candidates are gathered.
//
// Without serialization these two writers race, producing malformed
// WebSocket frames that the server rejects. writeMu below enforces
// the "one writer at a time" rule without blocking Recv.
//
// Auto-reconnect: Phase A ran with a single long-lived WS per session.
// One remote restart of the signal server (e.g. a systemd redeploy)
// or a transient network flap left the Client with a broken pipe and
// no way to recover — every subsequent sig.Send failed, the supervisor
// couldn't complete a PC rebuild, and the mount went dead. Now the
// Client keeps the dial parameters around and transparently redials
// when ReadJSON/WriteJSON surface a connection error, re-sending the
// join frame so the signaling server re-pairs us with the counterpart.
// Callers see the same Send/Recv API and never have to think about it.
package signaling

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// reconnectMaxBackoff caps how long we wait between redial attempts.
// Backoff scales from 250 ms exponentially up to this ceiling. Chosen
// to keep recovery within ~10 s of network coming back while not
// hammering a server that's legitimately down.
const reconnectMaxBackoff = 5 * time.Second

// Client wraps a signaling WebSocket for one peer in a room. Safe for
// one concurrent Recv caller + any number of Send callers (the writer
// side serializes internally).
type Client struct {
	url    string
	room   string
	role   string
	pubkey string
	log    *slog.Logger

	// writeMu also guards conn replacement so a redial in progress
	// can't race with a concurrent Send / Recv observing the old conn.
	writeMu sync.Mutex
	conn    *websocket.Conn

	// readMu serialises Recv callers. Not about gorilla's 1R-1W rule
	// (we only have one Recv goroutine in practice), but about keeping
	// reconnect-and-re-read atomic when a transient error triggers
	// redial inside Recv.
	readMu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}
}

// Dial opens the WS and sends the join frame. pubkey is the joiner's
// base64-encoded Ed25519 public key (may be empty for legacy callers
// that have not yet adopted the identity flow — the server will
// simply forward an empty Pubkey in the ready frame).
//
// A default slog logger is used internally for reconnect diagnostics.
// Callers that want to route those through their own logger should
// call DialWithLogger instead.
func Dial(url, room, role, pubkey string) (*Client, error) {
	return DialWithLogger(url, room, role, pubkey, slog.Default())
}

// DialWithLogger is Dial plus a slog destination for reconnect events.
func DialWithLogger(url, room, role, pubkey string, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		url:    url,
		room:   room,
		role:   role,
		pubkey: pubkey,
		log:    log,
		closed: make(chan struct{}),
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	c.conn = conn
	if err := c.writeEnvelope(Envelope{Kind: "join", Room: room, Role: role, Pubkey: pubkey}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send join: %w", err)
	}
	return c, nil
}

// Ready blocks until the server reports the pair is formed. It returns
// the counterpart's base64 Ed25519 public key (may be empty if the
// other side joined without one — caller decides whether to tolerate).
// Ready does NOT go through the reconnect machinery: if the first-ever
// dial fails this early, the caller wants to know and bail.
func (c *Client) Ready() (peerPubkey string, err error) {
	var e Envelope
	if err := c.conn.ReadJSON(&e); err != nil {
		return "", fmt.Errorf("read ready: %w", err)
	}
	if e.Kind == "error" {
		return "", fmt.Errorf("signal error: %s", e.Error)
	}
	if e.Kind != "ready" {
		return "", fmt.Errorf("expected ready, got %q", e.Kind)
	}
	return e.Pubkey, nil
}

// Send marshals a payload struct and ships it with the given kind
// ("sdp"|"ice"). Safe to call concurrently with Recv and with other
// Send invocations. Transient WS errors trigger a transparent redial.
func (c *Client) Send(kind string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.writeEnvelope(Envelope{Kind: kind, Payload: string(data)})
}

// writeEnvelope is the single funnel for outgoing writes. If the
// current conn's WriteJSON returns a recoverable error we drop that
// conn, redial, and retry exactly once — more aggressive retry would
// mask a genuinely persistent failure without recovery. Deeper retry
// happens inside Recv, which loops on ReadJSON.
func (c *Client) writeEnvelope(e Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.conn.WriteJSON(e); err == nil {
		return nil
	} else if !isRecoverable(err) {
		return err
	}

	c.log.Warn("signaling write failed; reconnecting", "kind", e.Kind)
	if err := c.reconnectLocked(); err != nil {
		return fmt.Errorf("reconnect: %w", err)
	}
	return c.conn.WriteJSON(e)
}

// Recv returns the next envelope from the peer. On a recoverable WS
// error we redial transparently and keep reading. Returns only on
// unrecoverable failure (client closed, dial permanently failing).
func (c *Client) Recv() (Envelope, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		var e Envelope
		err := c.conn.ReadJSON(&e)
		if err == nil {
			return e, nil
		}
		if !isRecoverable(err) {
			return Envelope{}, err
		}

		// Recoverable; redial under writeMu so Send doesn't race the
		// conn swap.
		c.log.Warn("signaling read failed; reconnecting", "err", err)
		c.writeMu.Lock()
		dialErr := c.reconnectLocked()
		c.writeMu.Unlock()
		if dialErr != nil {
			return Envelope{}, fmt.Errorf("reconnect: %w", dialErr)
		}
		// loop and re-read on the fresh conn
	}
}

// reconnectLocked drops the current conn and redials with exponential
// backoff. writeMu MUST be held by the caller — we swap c.conn under
// its protection so concurrent Send/Recv see a consistent view.
//
// The first thing we send on a fresh conn is the same join frame the
// original Dial sent; the signaling server rebinds our slot in the
// room and (per the either-role-second-joiner rule) fires a fresh
// ready on both peers if the counterpart is still present. The peer
// supervisor then drives a fresh SDP exchange as if this were a new
// session.
func (c *Client) reconnectLocked() error {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}

	backoff := 250 * time.Millisecond
	for {
		select {
		case <-c.closed:
			return errors.New("client closed")
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
		if err == nil {
			if jerr := conn.WriteJSON(Envelope{Kind: "join", Room: c.room, Role: c.role, Pubkey: c.pubkey}); jerr != nil {
				_ = conn.Close()
				c.log.Warn("signaling re-join failed; retrying", "err", jerr)
			} else {
				c.conn = conn
				c.log.Info("signaling reconnected", "room", c.room, "role", c.role)
				return nil
			}
		} else {
			c.log.Warn("signaling redial failed; backing off", "err", err, "backoff", backoff)
		}

		select {
		case <-c.closed:
			return errors.New("client closed")
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > reconnectMaxBackoff {
			backoff = reconnectMaxBackoff
		}
	}
}

// Close tears down the underlying WebSocket and cancels any pending
// reconnect loop.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		if c.conn != nil {
			err = c.conn.Close()
		}
	})
	return err
}

// isRecoverable decides whether a gorilla/websocket error warrants a
// transparent redial. Broken-pipe, unexpected EOF, close frames, and
// any net.OpError are treated as recoverable; anything else (JSON
// decode errors, our own framing bugs) propagates to the caller so
// we don't hide real protocol mistakes behind a retry.
func isRecoverable(err error) bool {
	if err == nil {
		return false
	}
	if websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseAbnormalClosure,
		websocket.CloseNoStatusReceived) {
		return true
	}
	if websocket.IsUnexpectedCloseError(err) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// Common string fallbacks — gorilla wraps some underlying errors
	// in ways that defeat errors.As. Cheap and readable.
	s := err.Error()
	for _, frag := range []string{"broken pipe", "use of closed", "EOF", "reset by peer", "connection refused"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}
