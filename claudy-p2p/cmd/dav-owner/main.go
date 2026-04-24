// dav-owner: serve a local directory via WebDAV over P2P DataChannels.
//
// Supervised PC: on Failed/Closed the owner tears down and rebuilds the
// PeerConnection on the same signaling WebSocket, re-wiring the same
// OnDataChannel handler so new inbound DCs keep flowing into the listener.
// The http.Server + peer.Listener outlive any number of PC rebuilds.
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
	"syscall"
	"time"

	"github.com/claudy/p2p/internal/identity"
	"github.com/claudy/p2p/internal/ownerfs"
	"github.com/claudy/p2p/internal/peer"
	"github.com/claudy/p2p/internal/powerlock"
	"github.com/claudy/p2p/internal/secure"
	"github.com/claudy/p2p/internal/signaling"
	"github.com/pion/webrtc/v4"
	"golang.org/x/net/webdav"
)

// secureListener wraps an inner net.Listener (our peer.Listener) so that
// every accepted connection first completes a Noise_IK handshake as the
// responder, pinning the initiator to the TOFU-known viewer pubkey. A
// failed handshake is logged and the underlying conn is dropped; Accept
// continues rather than returning — one bad viewer attempt must not
// kill the whole http.Server.
type secureListener struct {
	inner   net.Listener
	myPriv  ed25519.PrivateKey
	peerPub ed25519.PublicKey
	log     *slog.Logger
}

func (l *secureListener) Accept() (net.Conn, error) {
	for {
		raw, err := l.inner.Accept()
		if err != nil {
			return nil, err
		}
		sec, err := secure.Server(raw, l.myPriv, l.peerPub)
		if err != nil {
			l.log.Warn("noise handshake failed; dropping conn", "err", err)
			_ = raw.Close()
			continue
		}
		return sec, nil
	}
}

func (l *secureListener) Close() error   { return l.inner.Close() }
func (l *secureListener) Addr() net.Addr { return l.inner.Addr() }

// ownerSession holds the currently-live PC as the answerer. It reacts
// to incoming envelopes (SDP offer from viewer) by answering on the
// current PC. Failed/Closed triggers a rebuild.
type ownerSession struct {
	sig      *signaling.Client
	log      *slog.Logger
	incoming chan<- *peer.Conn
	rebuild  chan struct{}

	mu    sync.Mutex
	pc    *webrtc.PeerConnection
	epoch uint64
}

func newOwnerSession(sig *signaling.Client, log *slog.Logger, incoming chan<- *peer.Conn) *ownerSession {
	return &ownerSession{
		sig:      sig,
		log:      log,
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

// currentPC returns the live PeerConnection. Callers must re-check for nil
// under the possibility of an in-flight rebuild.
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
			// Mirror of dav-client: log the actual ICE path so the relay
			// share can be measured from either side independently.
			lt, rt, la, ra := peer.SelectedPath(pc)
			s.log.Info("ice selected",
				"epoch", epoch,
				"local", lt, "remote", rt,
				"local_addr", la, "remote_addr", ra)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.log.Warn("peer connection terminal; rebuilding", "epoch", epoch, "state", st.String())
			s.requestRebuild()
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		s.log.Info("new DataChannel", "epoch", epoch, "label", dc.Label())
		// "bootstrap" is a viewer-side artefact used purely to force SCTP
		// negotiation into the initial SDP. It must never be pushed to
		// the listener — otherwise our secure listener would try a Noise
		// handshake on an empty channel and waste 10s timing out.
		if dc.Label() == "bootstrap" {
			return
		}
		dc.OnOpen(func() {
			select {
			case s.incoming <- peer.NewConn(dc):
			default:
				s.log.Warn("incoming backlog full, dropping DC", "label", dc.Label())
				_ = dc.Close()
			}
		})
	})

	return nil
}

// handleSignal applies a remote envelope to the live PC. For SDP offers
// we immediately produce and send an answer (owner is always answerer).
func (s *ownerSession) handleSignal(env signaling.Envelope) {
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
	// server and breaking any live viewer mount. We can't serve files
	// while the CPU is halted — sleep must be blocked, not worked
	// around. Release fires via defer after the shutdown signal below.
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

	// The incoming channel + listener outlive every PC rebuild. Each new
	// DataChannel (possibly from a freshly rebuilt PC) is pushed here.
	// Buffer keeps a few DCs in flight during the narrow window before
	// http.Serve starts (after Ready+verify).
	incoming := make(chan *peer.Conn, 32)
	listener := peer.NewListener(incoming, "dav-owner")
	defer listener.Close()

	handler := &webdav.Handler{
		// Hide filesystem-junk (AppleDouble sidecars, .DS_Store,
		// Thumbs.db, etc.) from the remote viewer. They're noise on the
		// wire: every one spawns its own WebDAV round-trip = DataChannel
		// = Noise handshake. During a 1 GB upload we saw a handful of
		// new DCs time out just trying to write ._sidecar files;
		// filtering at the FS layer makes those requests invisible.
		FileSystem: ownerfs.FilterJunk(webdav.Dir(*dir)),
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

	sess := newOwnerSession(sig, log, incoming)
	go sess.run(ctx)

	peerPubkeyB64, err := sig.Ready()
	if err != nil {
		log.Error("ready", "err", err)
		os.Exit(1)
	}
	if err := verifyPeer(kr, *peerAlias, peerPubkeyB64, log); err != nil {
		log.Error("peer verification failed", "err", err)
		os.Exit(1)
	}
	// Noise_IK needs the viewer's pubkey to authenticate incoming DCs.
	// The signaling server always sends it now; refuse to continue
	// without one to avoid a degraded no-auth path sneaking in.
	if peerPubkeyB64 == "" {
		log.Error("viewer did not announce a pubkey; cannot establish Noise session")
		os.Exit(1)
	}
	peerPub, err := identity.ParsePublic(peerPubkeyB64)
	if err != nil {
		log.Error("parse viewer pubkey", "err", err)
		os.Exit(1)
	}

	// Now that we know the viewer's pinned pubkey, start serving WebDAV
	// through the secure listener. Every inbound DC completes a fresh
	// Noise_IK handshake before any HTTP byte is parsed.
	secLn := &secureListener{inner: listener, myPriv: id.Private, peerPub: peerPub, log: log}
	go func() {
		if err := srv.Serve(secLn); err != nil && err != http.ErrServerClosed {
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
