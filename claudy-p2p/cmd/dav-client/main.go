// dav-client: expose a remote WebDAV owner as a local HTTP endpoint.
//
// Supervised PeerConnection: if the PC goes Failed/Closed (ICE timeout,
// NAT binding expiry, etc.) the supervisor rebuilds it on the same
// signaling WebSocket — old DataChannels die, new local TCP accepts
// block on WaitConnected until the fresh PC reaches Connected.
//
// Macro flow:
//  1. Join signaling as "viewer"; supervisor builds PC, creates bootstrap
//     DC (forces SCTP m-section), sends offer, reads answer via signalLoop.
//  2. Bind local TCP. For every inbound conn, wait for the *current* PC to
//     be connected, open a fresh DC on it, io.Copy both directions.
//  3. On PC failure the supervisor closes the old PC and re-runs (1). The
//     local listener keeps accepting; pending proxy() goroutines either
//     finish on the old DC (still carrying its bytes) or, for fresh
//     accepts, wait up to dcOpenTimeout for the rebuilt PC.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/claudy/p2p/internal/identity"
	"github.com/claudy/p2p/internal/peer"
	"github.com/claudy/p2p/internal/secure"
	"github.com/claudy/p2p/internal/signaling"
	"github.com/pion/webrtc/v4"
)

// How long a single proxy() goroutine will wait for the current PC to be
// Connected before giving up on the local TCP accept. WebDAV clients are
// fine with a few seconds of stall; anything longer and Finder will show
// a spinner, which is still better than the mount going zombie.
const dcOpenTimeout = 20 * time.Second

// viewerSession supervises a single PeerConnection as the offerer side.
// Only one PC is live at a time; snapshot() hands out the current one
// along with a "connected" channel that closes when it reaches Connected.
// On Failed/Closed the supervisor goroutine rebuilds, bumping epoch so
// stale OnConnectionStateChange callbacks no-op.
type viewerSession struct {
	sig       *signaling.Client
	log       *slog.Logger
	rebuildCh chan struct{}

	mu        sync.Mutex
	pc        *webrtc.PeerConnection
	connected chan struct{} // closed on Connected; never re-opened, only replaced
	epoch     uint64
}

func newViewerSession(sig *signaling.Client, log *slog.Logger) *viewerSession {
	return &viewerSession{
		sig:       sig,
		log:       log,
		rebuildCh: make(chan struct{}, 1),
		connected: make(chan struct{}), // closed on first successful connect
	}
}

// snapshot returns the current PC and the channel that signals its
// connected state. Both fields are consistent (belong to the same epoch).
func (s *viewerSession) snapshot() (*webrtc.PeerConnection, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pc, s.connected
}

func (s *viewerSession) requestRebuild() {
	select {
	case s.rebuildCh <- struct{}{}:
	default: // rebuild already pending
	}
}

// run is the supervisor loop. It owns all lifecycle of s.pc.
func (s *viewerSession) run(ctx context.Context) {
	s.requestRebuild() // initial build
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			if s.pc != nil {
				_ = s.pc.Close()
			}
			s.mu.Unlock()
			return
		case <-s.rebuildCh:
			if err := s.rebuild(); err != nil {
				s.log.Error("rebuild peer connection", "err", err)
				// back off and retry
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				s.requestRebuild()
			}
		}
	}
}

func (s *viewerSession) rebuild() error {
	s.mu.Lock()
	if s.pc != nil {
		_ = s.pc.Close()
		s.pc = nil
	}

	pc, err := peer.New(s.sig)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("new peer: %w", err)
	}
	s.epoch++
	epoch := s.epoch
	// Replace connected channel only if the previous one was already closed
	// (i.e. a prior PC had reached Connected). Otherwise keep the existing
	// unclosed channel so early callers don't get a stale "connected".
	select {
	case <-s.connected:
		s.connected = make(chan struct{})
	default:
	}
	connCh := s.connected
	s.pc = pc
	s.mu.Unlock()

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.log.Info("peer connection state", "epoch", epoch, "state", st.String())
		s.mu.Lock()
		live := s.epoch == epoch
		s.mu.Unlock()
		if !live {
			return
		}
		switch st {
		case webrtc.PeerConnectionStateConnected:
			select {
			case <-connCh:
			default:
				close(connCh)
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			// Disconnected is transient in pion; it may recover to Connected
			// on its own within ~15s. We only rebuild on Failed/Closed.
			if st == webrtc.PeerConnectionStateDisconnected {
				return
			}
			s.log.Warn("peer connection terminal; rebuilding", "epoch", epoch, "state", st.String())
			s.requestRebuild()
		}
	})

	// Bootstrap DC forces SCTP negotiation into the offer's SDP so that
	// later on-demand DataChannels can ride the same association.
	if _, err := pc.CreateDataChannel("bootstrap", nil); err != nil {
		return fmt.Errorf("bootstrap dc: %w", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local: %w", err)
	}
	if err := s.sig.Send("sdp", pc.LocalDescription()); err != nil {
		return fmt.Errorf("send offer: %w", err)
	}
	return nil
}

// handleSignal applies an envelope to the current PC. Stale envelopes
// (e.g. an ICE candidate from a prior epoch arriving after rebuild) are
// handled at pion level — it will just reject them with a warning.
func (s *viewerSession) handleSignal(env signaling.Envelope) {
	pc, _ := s.snapshot()
	if pc == nil {
		return
	}
	if _, err := peer.ApplyRemote(pc, env); err != nil {
		s.log.Warn("apply remote", "err", err)
	}
}

func main() {
	signalURL := flag.String("signal", "ws://127.0.0.1:7042/signal", "signaling URL")
	room := flag.String("room", "demo", "room id")
	local := flag.String("local", "127.0.0.1:8910", "local HTTP listen address")
	identityDir := flag.String("identity", defaultIdentityDir(), "identity + keyring directory")
	peerAlias := flag.String("peer-alias", "", "pin owner under this alias in keyring (TOFU). If empty, TOFU is disabled.")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	id, err := identity.LoadOrCreate(*identityDir)
	if err != nil {
		log.Error("load identity", "err", err)
		os.Exit(1)
	}
	log.Info("identity loaded", "pubkey", id.PublicBase64())

	kr, err := identity.OpenKeyring(*identityDir)
	if err != nil {
		log.Error("open keyring", "err", err)
		os.Exit(1)
	}

	sig, err := signaling.Dial(*signalURL, *room, "viewer", id.PublicBase64())
	if err != nil {
		log.Error("dial signal", "err", err)
		os.Exit(1)
	}
	defer sig.Close()

	peerPubkeyB64, err := sig.Ready()
	if err != nil {
		log.Error("ready", "err", err)
		os.Exit(1)
	}
	if err := verifyPeer(kr, *peerAlias, peerPubkeyB64, log); err != nil {
		log.Error("peer verification failed", "err", err)
		os.Exit(1)
	}
	// Parse the owner's pubkey once — it is the fixed peer identity for
	// every DC opened on every PC rebuild. We refuse to run without it:
	// Noise_IK needs the responder's static key to authenticate.
	if peerPubkeyB64 == "" {
		log.Error("owner did not announce a pubkey; cannot establish Noise session")
		os.Exit(1)
	}
	peerPub, err := identity.ParsePublic(peerPubkeyB64)
	if err != nil {
		log.Error("parse owner pubkey", "err", err)
		os.Exit(1)
	}
	log.Info("paired with owner", "room", *room)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := newViewerSession(sig, log)
	go sess.run(ctx)
	go signalLoop(sess, sig, log)

	ln, err := net.Listen("tcp", *local)
	if err != nil {
		log.Error("local listen", "err", err)
		os.Exit(1)
	}
	defer ln.Close()
	log.Info("local WebDAV proxy ready", "url", "http://"+ln.Addr().String())
	log.Info("mount in Finder: Cmd+K → http://" + ln.Addr().String())

	go func() {
		var counter uint64
		for {
			localConn, err := ln.Accept()
			if err != nil {
				return
			}
			n := atomic.AddUint64(&counter, 1)
			go proxy(sess, localConn, n, id.Private, peerPub, log)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
}

// proxy waits for the current PC to be Connected, then opens a fresh DC
// on it and shuttles bytes. If the PC is in the middle of rebuilding,
// the local conn stalls (up to dcOpenTimeout) instead of receiving an
// immediate EOF — that's what keeps Finder's WebDAV client alive across
// ICE failures.
func proxy(sess *viewerSession, local net.Conn, id uint64, myPriv ed25519.PrivateKey, peerPub ed25519.PublicKey, log *slog.Logger) {
	defer local.Close()

	pc, connCh := sess.snapshot()
	select {
	case <-connCh:
	case <-time.After(dcOpenTimeout):
		log.Warn("pc not connected in time", "id", id)
		return
	}
	// Re-snapshot after wait: the PC we waited on may have been swapped.
	pc, _ = sess.snapshot()
	if pc == nil {
		log.Warn("no pc available", "id", id)
		return
	}

	label := fmt.Sprintf("req-%d", id)
	dc, err := pc.CreateDataChannel(label, nil)
	if err != nil {
		log.Warn("create dc", "id", id, "err", err)
		return
	}

	openCh := make(chan struct{}, 1)
	dc.OnOpen(func() {
		select {
		case openCh <- struct{}{}:
		default:
		}
	})

	select {
	case <-openCh:
	case <-time.After(10 * time.Second):
		log.Warn("dc open timeout", "id", id)
		_ = dc.Close()
		return
	}

	raw := peer.NewConn(dc)
	// Noise_IK on top of the DC: binds the session to the TOFU-pinned
	// owner pubkey. If a MITM swapped DTLS fingerprints at the signaling
	// layer the handshake fails here and we bail out of this request.
	remote, err := secure.Client(raw, myPriv, peerPub)
	if err != nil {
		log.Warn("noise handshake failed", "id", id, "err", err)
		_ = raw.Close()
		return
	}
	defer remote.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, local)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, remote)
		done <- struct{}{}
	}()
	<-done
}

func signalLoop(sess *viewerSession, sig *signaling.Client, log *slog.Logger) {
	for {
		env, err := sig.Recv()
		if err != nil {
			log.Warn("signal recv", "err", err)
			return
		}
		sess.handleSignal(env)
	}
}

// defaultIdentityDir mirrors dav-owner: ~/.claudy or ./.claudy fallback.
func defaultIdentityDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".claudy"
	}
	return filepath.Join(home, ".claudy")
}

// verifyPeer runs the TOFU check if an alias was configured. If the
// alias is empty we skip pinning — useful for smoke-tests where both
// sides just want to see each other's keys logged without bailing on
// first-run mismatches.
func verifyPeer(kr *identity.Keyring, alias, peerPubkey string, log *slog.Logger) error {
	if alias == "" {
		log.Warn("TOFU disabled (no -peer-alias set); accepting owner without pinning", "pubkey", peerPubkey)
		return nil
	}
	if peerPubkey == "" {
		return errors.New("owner joined without a pubkey; refusing because TOFU is enabled")
	}
	pub, err := identity.ParsePublic(peerPubkey)
	if err != nil {
		return fmt.Errorf("parse owner pubkey: %w", err)
	}
	first, err := kr.Check(alias, pub)
	if err != nil {
		return err
	}
	if first {
		log.Warn("TOFU: first contact, pinning owner", "alias", alias, "pubkey", peerPubkey)
	} else {
		log.Info("TOFU: owner matches pinned key", "alias", alias)
	}
	return nil
}
