// Package peer wraps pion/webrtc with the glue needed to drive a pairing
// through our signaling server: forward local ICE candidates outbound,
// apply remote SDP + ICE, and expose DataChannel events.
package peer

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/claudy/p2p/internal/signaling"
	"github.com/pion/webrtc/v4"
)

// DefaultICEServers returns the STUN + TURN servers ICE should use.
//
// STUN is harmless to advertise and costs nothing: pion only uses it to
// discover its server-reflexive address. TURN, on the other hand, is
// consulted only when direct P2P paths (host↔host, srflx↔srflx) fail
// — ICE's prioritization keeps relay candidates strictly last. So
// listing a TURN URL here does NOT mean every connection hairpins
// through our VPS; only the ~5-10% of sessions whose NAT is symmetric
// end up using it. The other 90% pay nothing beyond one extra
// STUN/Allocate round-trip during candidate gathering.
//
// TURN credentials are embedded in the binary. This is fine at this
// stage: a leaked credential lets an attacker use our relay for
// arbitrary UDP, but that's bandwidth theft, not a data compromise —
// Noise still authenticates both endpoints over any transport. When
// abuse shows up in practice we'll switch to the use-auth-secret flow
// and mint time-limited credentials from the Claudy daemon. Env var
// CLAUDY_TURN_SECRET overrides the baked-in secret so we can rotate
// without rebuilding binaries.
func DefaultICEServers() []webrtc.ICEServer {
	turnSecret := os.Getenv("CLAUDY_TURN_SECRET")
	if turnSecret == "" {
		turnSecret = defaultTURNSecret
	}
	return []webrtc.ICEServer{
		// Public STUN (free, unlimited, run by Cloudflare/Google). Two
		// providers so a single outage doesn't break candidate gathering.
		{URLs: []string{"stun:stun.cloudflare.com:3478"}},
		{URLs: []string{"stun:stun.l.google.com:19302"}},

		// Our TURN. UDP is the happy path; TCP fallback handles
		// corporate networks that block UDP outright.
		{
			URLs:       []string{"turn:23.172.217.149:3478?transport=udp"},
			Username:   turnUsername,
			Credential: turnSecret,
		},
		{
			URLs:       []string{"turn:23.172.217.149:3478?transport=tcp"},
			Username:   turnUsername,
			Credential: turnSecret,
		},
	}
}

// TURN credentials for the default Claudy relay. Keeping them in a
// named constant makes it obvious where the secret lives and gives
// grep-ability during rotation.
const (
	turnUsername = "claudy"
	// Replaced at deploy time (or via CLAUDY_TURN_SECRET env var) when
	// we rotate. For single-secret LT-cred mech this is the password
	// portion of the user=claudy:<secret> line in turnserver.conf.
	defaultTURNSecret = "RjLZy0GjXCCgqXotwKIbcpfeGmjrFhJJ"
)

// New builds a PeerConnection wired to forward local ICE candidates through
// sig as soon as they are gathered.
func New(sig *signaling.Client) (*webrtc.PeerConnection, error) {
	return NewWithPolicy(sig, false)
}

// NewWithPolicy is like New but lets the caller force
// ICETransportPolicy=Relay. Used by the supervisor when an earlier
// epoch on the same session reached state=Failed despite ICE
// negotiation succeeding briefly — that's the textbook signature of
// a path between two asymmetric NATs (or two different VPN exits)
// where consent-freshness probes drop within seconds. Re-gathering
// with relay-only forces both sides through TURN where reachability
// is symmetric and stable.
func NewWithPolicy(sig *signaling.Client, relayOnly bool) (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{ICEServers: DefaultICEServers()}
	if relayOnly {
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
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

// SelectedPath extracts the currently-negotiated ICE candidate pair
// from the PeerConnection, flattening it into something trivially
// loggable. Returns zero strings if ICE has not yet finished (or if
// any intermediate transport is still being set up by pion).
//
// Use it on state=connected to record whether the session used direct
// P2P or fell back to relay:
//
//	local=host|srflx|relay  → our side of the path
//	remote=host|srflx|relay → peer's side
//
// If either side is "relay", TURN is on the path (asymmetric cases
// like local=relay/remote=srflx still consume relay bandwidth on our
// side).
func SelectedPath(pc *webrtc.PeerConnection) (localType, remoteType, localAddr, remoteAddr string) {
	sctp := pc.SCTP()
	if sctp == nil {
		return "", "", "", ""
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return "", "", "", ""
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return "", "", "", ""
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "", "", "", ""
	}
	return pair.Local.Typ.String(), pair.Remote.Typ.String(),
		pair.Local.Address, pair.Remote.Address
}
