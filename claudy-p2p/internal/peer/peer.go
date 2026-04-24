// Package peer wraps pion/webrtc with the glue needed to drive a pairing
// through our signaling server: forward local ICE candidates outbound,
// apply remote SDP + ICE, and expose DataChannel events.
package peer

import (
	"encoding/json"
	"fmt"

	"github.com/claudy/p2p/internal/signaling"
	"github.com/pion/webrtc/v4"
)

// DefaultICEServers uses public STUN only. TURN will be added later.
func DefaultICEServers() []webrtc.ICEServer {
	return []webrtc.ICEServer{
		{URLs: []string{"stun:stun.cloudflare.com:3478"}},
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
}

// New builds a PeerConnection wired to forward local ICE candidates through
// sig as soon as they are gathered.
func New(sig *signaling.Client) (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{ICEServers: DefaultICEServers()}
	pc, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // gathering complete
		}
		_ = sig.Send("ice", c.ToJSON())
	})

	return pc, nil
}

// ApplyRemote dispatches an incoming envelope to the peer connection.
// Returns true if the envelope was consumed (sdp/ice).
func ApplyRemote(pc *webrtc.PeerConnection, e signaling.Envelope) (bool, error) {
	switch e.Kind {
	case "sdp":
		var sdp webrtc.SessionDescription
		if err := json.Unmarshal([]byte(e.Payload), &sdp); err != nil {
			return true, fmt.Errorf("decode sdp: %w", err)
		}
		if err := pc.SetRemoteDescription(sdp); err != nil {
			return true, fmt.Errorf("set remote sdp: %w", err)
		}
		return true, nil
	case "ice":
		var cand webrtc.ICECandidateInit
		if err := json.Unmarshal([]byte(e.Payload), &cand); err != nil {
			return true, fmt.Errorf("decode ice: %w", err)
		}
		if err := pc.AddICECandidate(cand); err != nil {
			return true, fmt.Errorf("add ice: %w", err)
		}
		return true, nil
	}
	return false, nil
}
