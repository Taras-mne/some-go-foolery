// Package tunnel establishes the long-lived secure transport between two
// Claudy peers and multiplexes many logical streams over it.
//
// Why this exists:
//   - Earlier versions opened a fresh DataChannel for every WebDAV request.
//     Each DC required its own Noise_IK handshake (1 RTT through signaling
//     plus 1 RTT through SCTP) and produced a flurry of stream
//     create/teardown events. Under bursts of small files Finder issued
//     ~8 parallel HTTP requests; the resulting pile of in-flight Noise
//     handshakes would race against the SCTP send queue saturated by the
//     last large body, time out at 10 s, and the whole mount stalled.
//   - This package replaces that pattern with: ONE DataChannel per
//     PeerConnection (label "tunnel"), ONE Noise_IK handshake on it, then
//     a yamux session multiplexing every WebDAV request as a logical
//     stream. Streams are cheap (no handshake, no SCTP stream id churn)
//     and yamux handles per-stream flow control out of the box.
//
// Lifecycle:
//   - The viewer (offerer) calls Dial right after the tunnel DC reaches
//     OnOpen. Returns a *yamux.Session that the supervisor exposes to
//     proxy goroutines via the viewerSession's snapshot helpers.
//   - The owner (answerer) calls Serve from OnDataChannel→OnOpen for the
//     tunnel-labelled DC. The returned session feeds yamux streams into
//     the http.Server via Accept.
//   - When the underlying PeerConnection rebuilds, the old yamux session
//     dies (Close on the secure.Conn cascades). In-flight streams return
//     io.EOF; webdavfs sees connection drops and retries on the new
//     session that the supervisor brings up.
package tunnel

import (
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/claudy/p2p/internal/peer"
	"github.com/claudy/p2p/internal/secure"
	"github.com/hashicorp/yamux"
	"github.com/pion/webrtc/v4"
)

// yamuxConfig returns a yamux config tuned for our usage.
//
// We turn off yamux's text logger (it writes to stderr by default and
// pollutes our slog stream) and bump the keepalive interval — yamux's
// default 30 s pings every minute create avoidable wakeups on idle
// mounts. Stream window size is 1 MiB, the same order as our DC flow
// control high water mark, so a single bulk PUT can saturate the wire
// without yamux artificially throttling.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 30 * time.Second
	cfg.MaxStreamWindowSize = 8 * 1024 * 1024
	return cfg
}

// Dial wraps an open DataChannel as a Noise client (initiator) and then
// a yamux client. Caller owns the returned session and is responsible
// for Close (which cascades to the secure conn and the DC).
func Dial(dc *webrtc.DataChannel, myPriv ed25519.PrivateKey, peerPub ed25519.PublicKey, log *slog.Logger) (*yamux.Session, error) {
	raw := peer.NewConn(dc)
	sec, err := secure.Client(raw, myPriv, peerPub)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("noise client: %w", err)
	}
	sess, err := yamux.Client(sec, yamuxConfig())
	if err != nil {
		_ = sec.Close()
		return nil, fmt.Errorf("yamux client: %w", err)
	}
	if log != nil {
		log.Info("tunnel up (viewer)", "label", dc.Label())
	}
	return sess, nil
}

// Serve wraps an open DataChannel as a Noise server (responder) and
// then a yamux server. Same ownership rules as Dial.
func Serve(dc *webrtc.DataChannel, myPriv ed25519.PrivateKey, peerPub ed25519.PublicKey, log *slog.Logger) (*yamux.Session, error) {
	raw := peer.NewConn(dc)
	sec, err := secure.Server(raw, myPriv, peerPub)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("noise server: %w", err)
	}
	sess, err := yamux.Server(sec, yamuxConfig())
	if err != nil {
		_ = sec.Close()
		return nil, fmt.Errorf("yamux server: %w", err)
	}
	if log != nil {
		log.Info("tunnel up (owner)", "label", dc.Label())
	}
	return sess, nil
}
