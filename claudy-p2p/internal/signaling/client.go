package signaling

import (
	"encoding/json"
	"fmt"

	"github.com/gorilla/websocket"
)

// Client wraps a signaling WebSocket for one peer in a room.
type Client struct {
	conn *websocket.Conn
	role string
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
	join := Envelope{Kind: "join", Room: room, Role: role, Pubkey: pubkey}
	if err := conn.WriteJSON(join); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send join: %w", err)
	}
	return &Client{conn: conn, role: role}, nil
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
func (c *Client) Send(kind string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.conn.WriteJSON(Envelope{Kind: kind, Payload: string(data)})
}

// Recv returns the next envelope from the peer.
func (c *Client) Recv() (Envelope, error) {
	var e Envelope
	err := c.conn.ReadJSON(&e)
	return e, err
}

func (c *Client) Close() error { return c.conn.Close() }
