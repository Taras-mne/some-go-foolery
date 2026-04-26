// dav-owner: serve a local directory via WebDAV over P2P DataChannels.
//
// Architecture (post-tunnel-refactor):
//   - One PeerConnection per epoch. On Failed/Closed we tear down and rebuild.
//   - One inbound DataChannel labelled "tunnel" per PC. We do Noise_IK once
//     on it (server side), then yamux server. Every yamux stream becomes
//     one inbound HTTP request feeding the WebDAV handler.
//   - The http.Server reads from a streamListener that delivers yamux
//     streams. The listener and the http.Server outlive every PC rebuild —
//     when the tunnel dies, in-flight streams EOF and webdavfs retries.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/claudy/p2p/internal/identity"
	"github.com/claudy/p2p/internal/ownerfs"
	"github.com/claudy/p2p/internal/peer"
	"github.com/claudy/p2p/internal/powerlock"
	"github.com/claudy/p2p/internal/signaling"
	"github.com/claudy/p2p/internal/tunnel"
	"github.com/hashicorp/yamux"
	"github.com/pion/webrtc/v4"
	"golang.org/x/net/webdav"
)

// streamListener adapts a chan of net.Conn (yamux streams in our case)
// into a net.Listener so http.Server.Serve can drive it without changes.
// The channel is closed-tolerant: a Close call signals Accept to return
// ErrListenerClosed without disturbing producer goroutines.
type streamListener struct {
	incoming  <-chan net.Conn
	closeOnce sync.Once
	closed    chan struct{}
	addr      net.Addr
}

func newStreamListener(in <-chan net.Conn, label string) *streamListener {
	return &streamListener{
		incoming: in,
		closed:   make(chan struct{}),
		addr:     fakeAddr{label: "stream-listener:" + label},
	}
}

// ErrListenerClosed is returned from Accept after Close.
var ErrListenerClosed = errors.New("stream listener closed")

func (l *streamListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, ErrListenerClosed
	case c, ok := <-l.incoming:
		if !ok {
			return nil, ErrListenerClosed
		}
		return c, nil
	}
}

func (l *streamListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *streamListener) Addr() net.Addr { return l.addr }

// fakeAddr — http.Server only uses Listener.Addr() for logging.
type fakeAddr struct{ label string }

func (f fakeAddr) Network() string { return "yamux" }
func (f fakeAddr) String() string  { return f.label }

// ownerSession holds the currently-live PC + tunnel mux as the answerer.
// SDP offers from the viewer come in over signaling; we answer on the
// current PC. Failed/Closed (or yamux death) triggers a rebuild.
type ownerSession struct {
	sig      *signaling.Client
	log      *slog.Logger
	myPriv   ed25519.PrivateKey
	peerPub  ed25519.PublicKey
	incoming chan<- net.Conn // streams flow here for the http.Server

	rebuild chan struct{}

	// relayOnly latches when an earlier epoch reached state=Failed —
	// see the symmetric note in dav-client/viewerSession. Both sides
	// detect failure independently; whichever sees it first escalates
	// its own rebuild path, and the next SDP exchange is relay-only
	// from at least one side, which forces TURN selection.
	relayOnly atomic.Bool

	mu    sync.Mutex
	pc    *webrtc.PeerConnection
	mux   *yamux.Session
	epoch uint64
}

func newOwnerSession(sig *signaling.Client, log *slog.Logger, incoming chan<- net.Conn, myPriv ed25519.PrivateKey, peerPub ed25519.PublicKey) *ownerSession {
	return &ownerSession{
		sig:      sig,
		log:      log,
		myPriv:   myPriv,
		peerPub:  peerPub,
		incoming: incoming,
		rebuild:  make(chan struct{}, 1),
	}
}

func (s *ownerSession) requestRebuild() {
	select {
	case s.rebuild <- struct{}{}:
	default:
	}
}

func (s *ownerSession) currentPC() *webrtc.PeerConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pc
}

func (s *ownerSession) run(ctx context.Context) {
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
		case <-s.rebuild:
			if err := s.buildPC(); err != nil {
				s.log.Error("build peer connection", "err", err)
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

func (s *ownerSession) buildPC() error {
	s.mu.Lock()
	if s.mux != nil {
		_ = s.mux.Close()
		s.mux = nil
	}
	if s.pc != nil {
		_ = s.pc.Close()
		s.pc = nil
	}

	pc, err := peer.NewWithPolicy(s.sig, s.relayOnly.Load())
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("new peer: %w", err)
	}
	s.epoch++
	epoch := s.epoch
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
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if st == webrtc.PeerConnectionStateFailed && s.relayOnly.CompareAndSwap(false, true) {
				s.log.Warn("ICE failed on direct path; escalating to relay-only for next epoch", "epoch", epoch)
			}
			s.log.Warn("peer connection terminal; rebuilding", "epoch", epoch, "state", st.String())
			s.requestRebuild()
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		s.log.Info("new DataChannel", "epoch", epoch, "label", dc.Label())
		// Anything other than the tunnel is a stale viewer (legacy build)
		// or noise — ignore safely.
		if dc.Label() != "tunnel" {
			s.log.Warn("unexpected DC label; ignoring", "label", dc.Label())
			return
		}
		dc.OnOpen(func() {
			// Stale-epoch guard: PC may have been torn down between
			// OnDataChannel and OnOpen during a fast rebuild.
			s.mu.Lock()
			if s.epoch != epoch {
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()

			sess, err := tunnel.Serve(dc, s.myPriv, s.peerPub, s.log)
			if err != nil {
				s.log.Error("tunnel serve", "epoch", epoch, "err", err)
				s.requestRebuild()
				return
			}
			s.mu.Lock()
			if s.epoch != epoch {
				s.mu.Unlock()
				_ = sess.Close()
				return
			}
			s.mux = sess
			s.mu.Unlock()

			// Each yamux stream becomes one HTTP connection for the
			// WebDAV handler. AcceptStream blocks until a stream arrives
			// or the session dies.
			go func() {
				for {
					stream, err := sess.AcceptStream()
					if err != nil {
						s.log.Warn("yamux accept", "epoch", epoch, "err", err)
						return
					}
					select {
					case s.incoming <- stream:
					default:
						s.log.Warn("incoming backlog full; closing stream")
						_ = stream.Close()
					}
				}
			}()

			// Watchdog: surface session death so we rebuild promptly
			// without waiting for the next pion state callback.
			go func() {
				<-sess.CloseChan()
				s.log.Warn("yamux session closed; rebuilding", "epoch", epoch)
				s.requestRebuild()
			}()
		})
	})

	return nil
}

// handleSignal applies a remote envelope to the live PC. For SDP offers
// we immediately produce and send an answer (owner is always answerer).
//
// Mid-session "ready" means the viewer rejoined our room (e.g. they
// restarted, or their WS flapped and redialed). Any SDP offer we might
// have been composing for the old session is now stale. Drop the
// current PC and rebuild so the fresh viewer can hand us a fresh offer.
func (s *ownerSession) handleSignal(env signaling.Envelope) {
	if env.Kind == "ready" {
		s.log.Info("signaling re-ready; triggering peer rebuild")
		s.requestRebuild()
		return
	}
	pc := s.currentPC()
	if pc == nil {
		s.log.Warn("signal arrived without live pc", "kind", env.Kind)
		return
	}
	consumed, err := peer.ApplyRemote(pc, env)
	if err != nil {
		s.log.Warn("apply remote", "err", err)
		return
	}
	if consumed && env.Kind == "sdp" {
		ans, err := pc.CreateAnswer(nil)
		if err != nil {
			s.log.Error("create answer", "err", err)
			return
		}
		if err := pc.SetLocalDescription(ans); err != nil {
			s.log.Error("set local", "err", err)
			return
		}
		if err := s.sig.Send("sdp", pc.LocalDescription()); err != nil {
			s.log.Error("send answer", "err", err)
			return
		}
	}
}

func main() {
	signalURL := flag.String("signal", "ws://127.0.0.1:7042/signal", "signaling URL")
	room := flag.String("room", "demo", "room id")
	dir := flag.String("dir", ".", "directory to share")
	identityDir := flag.String("identity", defaultIdentityDir(), "identity + keyring directory")
	peerAlias := flag.String("peer-alias", "", "pin viewer under this alias in keyring (TOFU). If empty, TOFU is disabled and the viewer is accepted without pinning.")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("dav-owner starting", "dir", *dir, "room", *room)

	// Keep the host awake for the lifetime of the owner. Without this
	// the laptop idle-sleeps after ~5-15 min, tearing down the WebDAV
	// server and breaking any live viewer mount.
	sleepLock := powerlock.Acquire(log)
	defer sleepLock.Release()

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

	sig, err := signaling.Dial(*signalURL, *room, "owner", id.PublicBase64())
	if err != nil {
		log.Error("dial signal", "err", err)
		os.Exit(1)
	}
	defer sig.Close()
	log.Info("waiting for viewer")

	// Streams from yamux flow into here. Generously sized to absorb burst
	// opens during a tab-flooding webdavfs sync; small enough that a
	// dead http.Server doesn't leak unbounded memory.
	incoming := make(chan net.Conn, 64)
	listener := newStreamListener(incoming, "dav-owner")
	defer listener.Close()

	handler := &webdav.Handler{
		// Layered FS, outer-in:
		//   1. Cached — TTL-cache Stat / Readdir so Mini-Redirector's
		//      PROPFIND flood doesn't pin CPU and starve concurrent GETs.
		//   2. FilterJunk — hide AppleDouble / .DS_Store / Thumbs.db.
		//   3. NormalizeNFC — collapse macOS NFD names to NFC.
		//   4. ShareDeleteDir — open files with FILE_SHARE_DELETE on
		//      Windows so the local user can rm/mv mid-transfer.
		FileSystem: ownerfs.Cached(ownerfs.FilterJunk(ownerfs.NormalizeNFC(ownerfs.ShareDeleteDir(*dir)))),
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Warn("webdav error", "method", r.Method, "path", r.URL.Path, "err", err)
				return
			}
			log.Info("webdav", "method", r.Method, "path", r.URL.Path)
		},
	}
	srv := &http.Server{Handler: handler}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
		log.Error("viewer did not announce a pubkey; cannot establish Noise session")
		os.Exit(1)
	}
	peerPub, err := identity.ParsePublic(peerPubkeyB64)
	if err != nil {
		log.Error("parse viewer pubkey", "err", err)
		os.Exit(1)
	}

	sess := newOwnerSession(sig, log, incoming, id.Private, peerPub)
	go sess.run(ctx)

	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Error("http.Serve", "err", err)
		}
	}()

	go signalLoop(sess, sig, log)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
}

// defaultIdentityDir returns ~/.claudy — falls back to cwd/.claudy if
// the home directory is unavailable (e.g. stripped-down CI env).
func defaultIdentityDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".claudy"
	}
	return filepath.Join(home, ".claudy")
}

// verifyPeer runs the TOFU check if an alias was configured. Empty
// peerPubkey is only tolerated when TOFU is disabled (no alias).
func verifyPeer(kr *identity.Keyring, alias, peerPubkey string, log *slog.Logger) error {
	if alias == "" {
		log.Warn("TOFU disabled (no -peer-alias set); accepting viewer without pinning", "pubkey", peerPubkey)
		return nil
	}
	if peerPubkey == "" {
		return errors.New("viewer joined without a pubkey; refusing because TOFU is enabled")
	}
	pub, err := identity.ParsePublic(peerPubkey)
	if err != nil {
		return fmt.Errorf("parse viewer pubkey: %w", err)
	}
	first, err := kr.Check(alias, pub)
	if err != nil {
		return err
	}
	if first {
		log.Warn("TOFU: first contact, pinning viewer", "alias", alias, "pubkey", peerPubkey)
	} else {
		log.Info("TOFU: viewer matches pinned key", "alias", alias)
	}
	return nil
}

func signalLoop(sess *ownerSession, sig *signaling.Client, log *slog.Logger) {
	for {
		env, err := sig.Recv()
		if err != nil {
			log.Warn("signal recv", "err", err)
			return
		}
		sess.handleSignal(env)
	}
}
