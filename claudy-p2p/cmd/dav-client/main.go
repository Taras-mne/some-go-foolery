// dav-client: expose a remote WebDAV owner as a local HTTP endpoint.
//
// Architecture (post-tunnel-refactor):
//   - One PeerConnection per epoch. On Failed/Closed we tear down and rebuild.
//   - One DataChannel per PC, label "tunnel". Noise_IK runs once on it.
//   - A yamux session multiplexes every WebDAV request onto its own logical
//     stream over the single Noise-secured byte stream. No per-request
//     handshake; stream open is essentially free.
//   - Local TCP listener accepts WebDAV clients (Finder/Explorer). Each
//     accept opens a yamux stream and io.Copy's both directions.
//
// Why the rewrite: the previous design opened a fresh DC + Noise per
// request. Under bursts of small files Finder issued ~8 parallel HTTP
// requests; the resulting pile of in-flight Noise handshakes raced
// against an SCTP send queue saturated by the last large body, timed
// out at 10 s, and the whole mount stalled. yamux gives us per-stream
// flow control and zero-cost stream creation in exchange for one
// long-lived secure session.
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
	"github.com/claudy/p2p/internal/signaling"
	"github.com/claudy/p2p/internal/tunnel"
	"github.com/hashicorp/yamux"
	"github.com/pion/webrtc/v4"
)

// Time a single proxy() goroutine will wait for the current tunnel to be
// ready before giving up on the local TCP accept. WebDAV clients are
// fine with a few seconds of stall during PC rebuilds; anything longer
// and Finder will show a spinner, which is still better than the mount
// going zombie.
const tunnelReadyTimeout = 20 * time.Second

// viewerSession supervises a single PeerConnection + tunnel as the
// offerer side. Only one (PC, mux) pair is live at a time. On Failed /
// Closed PC, or yamux session death, the supervisor rebuilds, bumping
// epoch so stale callbacks no-op.
type viewerSession struct {
	sig     *signaling.Client
	log     *slog.Logger
	myPriv  ed25519.PrivateKey
	peerPub ed25519.PublicKey

	rebuildCh chan struct{}

	mu    sync.Mutex
	pc    *webrtc.PeerConnection
	mux   *yamux.Session
	ready chan struct{} // closed once mux is established this epoch
	epoch uint64
}

func newViewerSession(sig *signaling.Client, log *slog.Logger, myPriv ed25519.PrivateKey, peerPub ed25519.PublicKey) *viewerSession {
	return &viewerSession{
		sig:       sig,
		log:       log,
		myPriv:    myPriv,
		peerPub:   peerPub,
		rebuildCh: make(chan struct{}, 1),
		ready:     make(chan struct{}),
	}
}

// snapshotMux returns the current yamux session (or nil) and the channel
// that closes when the *current* session is ready. Both come from the
// same epoch so callers don't observe a half-installed state.
func (s *viewerSession) snapshotMux() (*yamux.Session, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mux, s.ready
}

func (s *viewerSession) requestRebuild() {
	select {
	case s.rebuildCh <- struct{}{}:
	default: // already pending
	}
}

// run is the supervisor loop. It owns all lifecycle of s.pc and s.mux.
func (s *viewerSession) run(ctx context.Context) {
	s.requestRebuild() // initial build
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			if s.mux != nil {
				_ = s.mux.Close()
			}
			if s.pc != nil {
				_ = s.pc.Close()
			}
			s.mu.Unlock()
			return
		case <-s.rebuildCh:
			if err := s.rebuild(ctx); err != nil {
				s.log.Error("rebuild peer connection", "err", err)
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

// rebuild tears down the current (PC, mux) and creates a fresh PC + new
// tunnel DC. The Noise+yamux setup is wired to the DC's OnOpen callback;
// we don't block in rebuild() waiting for it. The supervisor returns
// quickly so the next signal envelope can flow into the new PC.
func (s *viewerSession) rebuild(ctx context.Context) error {
	s.mu.Lock()
	if s.mux != nil {
		_ = s.mux.Close()
		s.mux = nil
	}
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
	// Replace ready channel only if the previous one was already closed
	// (i.e. a prior epoch had reached Ready). Otherwise reuse the existing
	// unclosed channel so early callers don't get stuck on a stale ref.
	select {
	case <-s.ready:
		s.ready = make(chan struct{})
	default:
	}
	readyCh := s.ready
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
			lt, rt, la, ra := peer.SelectedPath(pc)
			s.log.Info("ice selected",
				"epoch", epoch,
				"local", lt, "remote", rt,
				"local_addr", la, "remote_addr", ra)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			// Disconnected is transient in pion; it may recover to Connected
			// on its own within ~15s. Only rebuild on Failed/Closed.
			if st == webrtc.PeerConnectionStateDisconnected {
				return
			}
			s.log.Warn("peer connection terminal; rebuilding", "epoch", epoch, "state", st.String())
			s.requestRebuild()
		}
	})

	// Open the tunnel DC. SCTP m-section gets negotiated as a side effect,
	// so we don't need a separate "bootstrap" DC anymore.
	dc, err := pc.CreateDataChannel("tunnel", nil)
	if err != nil {
		return fmt.Errorf("create tunnel DC: %w", err)
	}
	dc.OnOpen(func() {
		// Stale-epoch guard: by the time OnOpen fires the supervisor may
		// have already torn this PC down (e.g. on a fast Failed→rebuild).
		s.mu.Lock()
		if s.epoch != epoch {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		sess, err := tunnel.Dial(dc, s.myPriv, s.peerPub, s.log)
		if err != nil {
			s.log.Error("tunnel dial", "epoch", epoch, "err", err)
			s.requestRebuild()
			return
		}

		s.mu.Lock()
		if s.epoch != epoch {
			// Won the race against another rebuild; drop our session.
			s.mu.Unlock()
			_ = sess.Close()
			return
		}
		s.mux = sess
		s.mu.Unlock()
		// Closing readyCh unblocks every proxy() goroutine waiting on it.
		close(readyCh)

		// Watchdog: when yamux dies (peer closed, secure conn EOF, etc.)
		// rebuild on the same signaling channel.
		go func() {
			<-sess.CloseChan()
			s.log.Warn("yamux session closed; rebuilding", "epoch", epoch)
			s.requestRebuild()
		}()
	})

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
// handled at pion level — it just rejects them with a warning.
//
// Mid-session "ready": the signaling server re-fires it whenever the
// counterpart rejoins a room we're still in (owner restart, WS redial
// after a flap). When that happens any SDP offer we'd already sent was
// dropped into an empty room, so our current PC is stuck waiting for
// an answer that will never arrive. Trigger a fresh rebuild.
func (s *viewerSession) handleSignal(env signaling.Envelope) {
	if env.Kind == "ready" {
		s.log.Info("signaling re-ready; triggering peer rebuild")
		s.requestRebuild()
		return
	}
	s.mu.Lock()
	pc := s.pc
	s.mu.Unlock()
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

	sess := newViewerSession(sig, log, id.Private, peerPub)
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
			go proxy(sess, localConn, n, log)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
}

// proxy waits for the current tunnel mux to be ready, opens a yamux
// stream, and shuttles bytes. If the tunnel is in the middle of
// rebuilding the local conn stalls (up to tunnelReadyTimeout) instead
// of EOFing immediately — that's what keeps Finder's WebDAV client
// alive across PC rebuilds.
func proxy(sess *viewerSession, local net.Conn, id uint64, log *slog.Logger) {
	defer local.Close()

	_, ready := sess.snapshotMux()
	select {
	case <-ready:
	case <-time.After(tunnelReadyTimeout):
		log.Warn("tunnel not ready in time", "id", id)
		return
	}
	mux, _ := sess.snapshotMux()
	if mux == nil {
		log.Warn("no mux available", "id", id)
		return
	}

	stream, err := mux.OpenStream()
	if err != nil {
		log.Warn("open stream", "id", id, "err", err)
		return
	}
	defer stream.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stream, local)
		// Half-close the write side so the owner sees EOF and finishes
		// flushing its response — same semantics http.Server expects on
		// keep-alive idle.
		_ = stream.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, stream)
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
