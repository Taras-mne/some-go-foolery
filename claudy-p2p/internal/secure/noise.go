// Package secure wraps a byte-stream net.Conn with a Noise_IK transport.
//
// Why: TOFU pins a peer's long-term Ed25519 identity, and WebRTC DTLS
// encrypts the DataChannel — but the signaling server sees the SDP
// fingerprints and could in principle swap them to mount a MITM. Running
// Noise_IK inside the DataChannel forces the peer to cryptographically
// prove ownership of the TOFU-pinned static key on every fresh
// connection; a forged signaling relay can no longer silently read or
// tamper with traffic. Each connection also has forward secrecy via the
// fresh ephemeral keys exchanged in the handshake.
//
// Pattern choice: Noise_IK (Initiator Knows responder's static). After
// TOFU, the initiator (viewer) always knows the responder's (owner's)
// pubkey, so IK matches our trust model exactly and completes in one
// round-trip. The responder learns the initiator's static from message 1
// and we verify it against the pinned pubkey before sending message 2.
//
// Key conversion: Noise uses X25519 for DH, but our long-term identity
// is Ed25519 (for signing-capable keys, simpler TOFU display, future
// signatures). We derive X25519 keypairs deterministically from the
// Ed25519 ones — standard and safe when the keys are not reused for
// both signing and KX by third parties.
//
// Framing: the underlying peer.Conn is a byte stream (it already chunks
// internally for DataChannel.Send). We add a 2-byte big-endian length
// prefix per Noise message so reads line up on ciphertext boundaries.
package secure

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"filippo.io/edwards25519"
	"github.com/flynn/noise"
)

// handshakeTimeout caps the whole 1-RTT exchange. The underlying conn's
// deadline is reset afterwards so subsequent I/O is not bounded.
const handshakeTimeout = 10 * time.Second

// maxFramePayload is the largest plaintext chunk we encrypt per frame.
// Noise adds a 16-byte Poly1305 tag and we add a 2-byte length prefix,
// so an encrypted frame stays well under the DataChannel-friendly 16KB
// chunk size used by peer.Conn.
const maxFramePayload = 16*1024 - 64

// ErrHandshakeFailed wraps any handshake-phase failure (IO, protocol,
// or peer static mismatch). Callers may log the wrapped error for
// diagnostics.
var ErrHandshakeFailed = errors.New("noise handshake failed")

// Conn is a net.Conn that transparently encrypts/decrypts via Noise
// transport cipher states established at handshake time.
type Conn struct {
	raw net.Conn

	enc *noise.CipherState
	dec *noise.CipherState

	sendMu sync.Mutex
	recvMu sync.Mutex

	// readBuf holds the tail of a decrypted frame when the caller's
	// Read buffer was smaller than the frame.
	readBuf []byte
}

// Client runs the Noise_IK handshake as the initiator. peerPub MUST be
// the TOFU-pinned Ed25519 public key of the responder.
func Client(raw net.Conn, myPriv ed25519.PrivateKey, peerPub ed25519.PublicKey) (*Conn, error) {
	return doHandshake(raw, myPriv, peerPub, true)
}

// Server runs the Noise_IK handshake as the responder. peerPub is the
// pinned pubkey of the *expected* initiator; the handshake will fail if
// the other side presents a different static.
func Server(raw net.Conn, myPriv ed25519.PrivateKey, peerPub ed25519.PublicKey) (*Conn, error) {
	return doHandshake(raw, myPriv, peerPub, false)
}

func doHandshake(raw net.Conn, myPriv ed25519.PrivateKey, peerPub ed25519.PublicKey, initiator bool) (*Conn, error) {
	// Bound the handshake so a silent peer cannot hold the DC forever.
	// Clear the deadline after, otherwise subsequent Reads would inherit it.
	_ = raw.SetDeadline(time.Now().Add(handshakeTimeout))
	defer func() { _ = raw.SetDeadline(time.Time{}) }()

	myXPriv, myXPub, err := ed25519PrivToX25519(myPriv)
	if err != nil {
		return nil, fmt.Errorf("%w: convert my priv: %v", ErrHandshakeFailed, err)
	}
	peerXPub, err := ed25519PubToX25519(peerPub)
	if err != nil {
		return nil, fmt.Errorf("%w: convert peer pub: %v", ErrHandshakeFailed, err)
	}

	cs := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2b)
	cfg := noise.Config{
		CipherSuite:   cs,
		Pattern:       noise.HandshakeIK,
		Initiator:     initiator,
		StaticKeypair: noise.DHKey{Private: myXPriv, Public: myXPub},
	}
	if initiator {
		// IK requires the initiator to know the responder's static up front.
		cfg.PeerStatic = peerXPub
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: new handshake: %v", ErrHandshakeFailed, err)
	}

	var enc, dec *noise.CipherState
	if initiator {
		// -> e, es, s, ss
		msg1, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: write msg1: %v", ErrHandshakeFailed, err)
		}
		if err := writeFrame(raw, msg1); err != nil {
			return nil, fmt.Errorf("%w: send msg1: %v", ErrHandshakeFailed, err)
		}
		// <- e, ee, se  (completes the handshake on our side)
		msg2, err := readFrame(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: read msg2: %v", ErrHandshakeFailed, err)
		}
		_, cs0, cs1, err := hs.ReadMessage(nil, msg2)
		if err != nil {
			return nil, fmt.Errorf("%w: parse msg2: %v", ErrHandshakeFailed, err)
		}
		if cs0 == nil || cs1 == nil {
			return nil, fmt.Errorf("%w: handshake incomplete after msg2", ErrHandshakeFailed)
		}
		// flynn/noise returns (cs_initiator→responder, cs_responder→initiator)
		// from either side. Initiator encrypts with cs0, decrypts with cs1.
		enc, dec = cs0, cs1
	} else {
		msg1, err := readFrame(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: read msg1: %v", ErrHandshakeFailed, err)
		}
		if _, _, _, err := hs.ReadMessage(nil, msg1); err != nil {
			return nil, fmt.Errorf("%w: parse msg1: %v", ErrHandshakeFailed, err)
		}
		// MITM defence: the initiator's static revealed in msg1 MUST match
		// the pinned key. If a relay swapped DTLS/Noise keys to sit in the
		// middle, it cannot also fake ownership of the pinned private key.
		gotPeer := hs.PeerStatic()
		if !bytes.Equal(gotPeer, peerXPub) {
			return nil, fmt.Errorf("%w: peer static mismatch (pinned %x, got %x)",
				ErrHandshakeFailed, peerXPub, gotPeer)
		}
		// -> e, ee, se
		msg2, cs0, cs1, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: write msg2: %v", ErrHandshakeFailed, err)
		}
		if err := writeFrame(raw, msg2); err != nil {
			return nil, fmt.Errorf("%w: send msg2: %v", ErrHandshakeFailed, err)
		}
		if cs0 == nil || cs1 == nil {
			return nil, fmt.Errorf("%w: handshake incomplete after msg2", ErrHandshakeFailed)
		}
		// Responder encrypts with cs1 (responder→initiator), decrypts with cs0.
		enc, dec = cs1, cs0
	}

	return &Conn{raw: raw, enc: enc, dec: dec}, nil
}

// Read decrypts the next frame (or returns buffered bytes from a
// previous oversized frame). Partial reads are fine: we hand out what
// fits and stash the rest for the next call.
func (c *Conn) Read(p []byte) (int, error) {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()

	if len(c.readBuf) > 0 {
		n := copy(p, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}

	ct, err := readFrame(c.raw)
	if err != nil {
		return 0, err
	}
	plain, err := c.dec.Decrypt(nil, nil, ct)
	if err != nil {
		// A decrypt failure is fatal for this session — could be tampering
		// or a bit flip the DTLS layer missed; either way we cannot
		// reliably continue on a cipher state with an unknown counter.
		return 0, fmt.Errorf("noise decrypt: %w", err)
	}
	n := copy(p, plain)
	if n < len(plain) {
		c.readBuf = plain[n:]
	}
	return n, nil
}

// Write chunks the plaintext to maxFramePayload, encrypts each chunk,
// and writes a length-prefixed frame per chunk.
func (c *Conn) Write(p []byte) (int, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxFramePayload {
			chunk = p[:maxFramePayload]
		}
		ct, err := c.enc.Encrypt(nil, nil, chunk)
		if err != nil {
			return total, fmt.Errorf("noise encrypt: %w", err)
		}
		if err := writeFrame(c.raw, ct); err != nil {
			return total, err
		}
		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}

func (c *Conn) Close() error                       { return c.raw.Close() }
func (c *Conn) LocalAddr() net.Addr                { return c.raw.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.raw.RemoteAddr() }
func (c *Conn) SetDeadline(t time.Time) error      { return c.raw.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.raw.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }

// readFrame reads a uint16-prefixed blob from r.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// writeFrame writes p with a uint16 length prefix. p must fit in uint16.
func writeFrame(w io.Writer, p []byte) error {
	if len(p) > 0xFFFF {
		return fmt.Errorf("frame too large: %d", len(p))
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))
	// Coalesce into one Write so peer.Conn's internal chunker doesn't
	// split header from body across SCTP messages (harmless but noisy).
	out := make([]byte, 0, 2+len(p))
	out = append(out, hdr[:]...)
	out = append(out, p...)
	_, err := w.Write(out)
	return err
}

// ed25519PrivToX25519 converts an Ed25519 private key to a clamped
// X25519 scalar, using the standard construction: SHA-512(seed)[:32]
// with the three canonical bit tweaks.
//
// Returns both the X25519 private scalar and the matching public point
// (derived from the Ed25519 public, not by scalar-mult, so we avoid an
// extra Curve25519 operation).
func ed25519PrivToX25519(priv ed25519.PrivateKey) (xpriv, xpub []byte, err error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, nil, errors.New("invalid ed25519 private key size")
	}
	seed := priv.Seed()
	h := sha512.Sum512(seed)
	x := make([]byte, 32)
	copy(x, h[:32])
	x[0] &= 248
	x[31] &= 127
	x[31] |= 64

	pub, err := ed25519PubToX25519(priv.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, nil, err
	}
	return x, pub, nil
}

// ed25519PubToX25519 maps an Ed25519 public key (compressed Edwards
// point) to an X25519 public key (Montgomery u-coordinate) via the
// birational map. This is the inverse of what flynn/noise would produce
// by scalar-multiplying the clamped private by the X25519 base.
func ed25519PubToX25519(pub ed25519.PublicKey) ([]byte, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("invalid ed25519 public key size")
	}
	p, err := new(edwards25519.Point).SetBytes(pub)
	if err != nil {
		return nil, fmt.Errorf("decode edwards point: %w", err)
	}
	return p.BytesMontgomery(), nil
}
