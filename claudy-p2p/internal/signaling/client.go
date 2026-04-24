// Package signaling: client wrapper over a gorilla WebSocket connection.
//
// Concurrency contract (important): gorilla/websocket allows at most one
// concurrent reader and at most one concurrent writer per connection.
// Our callers respect the reader side — only a single signalLoop calls
// Recv — but writes come from two sources that run in parallel:
//
//   1. The main flow that sends "join" / "sdp" envelopes from the
//      supervisor / rebuild path.
//   2. pion's OnICECandidate callback, which fires from an internal
//      pion goroutine as candidates are gathered.
//
// Without serialization these two Writers race, and a WriteJSON call
// interleaved with another can produce malformed WebSocket frames that
// the server rejects with "unexpected EOF" — typically at the worst
// time (PC rebuild, flapping network). The writeMu below enforces the
// "one writer at a time" rule without blocking Recv.
package signaling

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
)

// Client wraps a signaling WebSocket for one peer in a room.
type Client struct {
	conn *websocket.Conn
	role string

	// writeMu serializes every WriteJSON. Read path is intentionally
	// unprotected: only one goroutine ever calls Recv/Ready.
	writeMu sync.Mutex
}

// Dial opens the WS and sends the join frame. pubkey is the joiner's
// base64-encoded Ed25519 public key (may be empty for legacy callers
// that have not yet adopted the identity flow — the server will simply
// forward an empty Pubkey in the ready frame).
func Dial(url, room, role, pubkey string) (*Client, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	// No other goroutine has a handle yet, so no lock is required here;
	// but we still go through the Client's own write path for symmetry
	// and to keep the conn hidden.
	c := &Client{conn: conn, role: role}
	join := Envelope{Kind: "join", Room: room, Role: role, Pubkey: pubkey}
	if err := c.writeEnvelope(join); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send join: %w", err)
	}
	return c, nil
}

// Ready blocks until the server reports the pair is formed. It returns
// the counterpart's base64 Ed25519 public key (may be empty if the
// other side joined without one — caller decides whether to tolerate).
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

// Send marshals a payload struct and ships it with the given kind ("sdp"|"ice").
// Safe to call concurrently with Recv and with other Send invocations.
func (c *Client) Send(kind string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.writeEnvelope(Envelope{Kind: kind, Payload: string(data)})
}

// writeEnvelope is the single funnel for all writes on c.conn. Holding
// writeMu for the full WriteJSON call is what makes concurrent Send
// safe — gorilla does not serialize internally.
func (c *Client) writeEnvelope(e Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteJSON(e)
}

// Recv returns the next envelope from the peer. Callers must invoke it
// from a single goroutine only.
func (c *Client) Recv() (Envelope, error) {
	var e Envelope
	err := c.conn.ReadJSON(&e)
	return e, err
}

// Close tears down the underlying WebSocket. Idempotent at the gorilla
// level; we don't add our own Once because close-after-close just
// returns an error the caller can ignore.
func (c *Client) Close() error { return c.conn.Close() }
