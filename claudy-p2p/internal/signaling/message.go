// Package signaling defines the WebSocket wire format for SDP/ICE exchange.
//
// Two roles connect to the signaling server and join the same room:
//   - owner  sends role="owner"  first, then waits.
//   - viewer sends role="viewer" and the server pairs it with the owner.
//
// After pairing, either side posts Envelope{Kind: "sdp"|"ice", Payload: ...}
// and the server forwards it to the peer. The server never inspects payloads.
//
// Identity exchange: both sides include their base64 Ed25519 public key
// in the join frame. On successful pairing the server includes the
// counterpart's pubkey in the "ready" frame, so each side can run a
// TOFU check before accepting the connection.
package signaling

// Envelope is the single message type on the wire.
type Envelope struct {
	Kind    string `json:"kind"`    // "join" | "sdp" | "ice" | "ready" | "error"
	Room    string `json:"room,omitempty"`
	Role    string `json:"role,omitempty"`   // owner | viewer (only for "join")
	Pubkey  string `json:"pubkey,omitempty"` // base64 Ed25519 pub (join + ready)
	Payload string `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
}
