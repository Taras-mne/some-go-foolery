package secure

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pipePair returns two net.Conns connected via net.Pipe (synchronous,
// unbuffered). Good enough for round-trip tests because our framing is
// length-prefixed and does not rely on message boundaries.
func pipePair() (net.Conn, net.Conn) { return net.Pipe() }

func mustGenKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return pub, priv
}

// TestHandshake_RoundTrip runs the IK handshake over an in-memory pipe
// and exchanges a few application messages both ways.
func TestHandshake_RoundTrip(t *testing.T) {
	initPub, initPriv := mustGenKey(t)
	respPub, respPriv := mustGenKey(t)

	a, b := pipePair()

	type result struct {
		conn *Conn
		err  error
	}
	clientCh := make(chan result, 1)
	serverCh := make(chan result, 1)

	go func() {
		c, err := Client(a, initPriv, respPub)
		clientCh <- result{c, err}
	}()
	go func() {
		s, err := Server(b, respPriv, initPub)
		serverCh <- result{s, err}
	}()

	cr := <-clientCh
	sr := <-serverCh
	if cr.err != nil {
		t.Fatalf("client handshake: %v", cr.err)
	}
	if sr.err != nil {
		t.Fatalf("server handshake: %v", sr.err)
	}
	defer cr.conn.Close()
	defer sr.conn.Close()

	// Client → server. net.Pipe is synchronous/unbuffered, so Write must
	// run concurrently with Read.
	payload := []byte("hello from viewer")
	writeErr := make(chan error, 1)
	go func() { _, e := cr.conn.Write(payload); writeErr <- e }()
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(sr.conn, buf); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("client write: %v", err)
	}
	if string(buf) != string(payload) {
		t.Errorf("server got %q, want %q", buf, payload)
	}

	// Server → client.
	reply := []byte("hello from owner")
	go func() { _, e := sr.conn.Write(reply); writeErr <- e }()
	buf = make([]byte, len(reply))
	if _, err := io.ReadFull(cr.conn, buf); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("server write: %v", err)
	}
	if string(buf) != string(reply) {
		t.Errorf("client got %q, want %q", buf, reply)
	}
}

// TestHandshake_MITM_PeerStaticMismatch simulates a relay that knows
// neither the client's nor the server's private key: it substitutes its
// own static keypair on each side. Both handshakes should fail.
//
// We simulate this by giving the server a *wrong* expected-peer pubkey,
// so it should reject the handshake with ErrHandshakeFailed when the
// real initiator's static doesn't match.
func TestHandshake_MITM_PeerStaticMismatch(t *testing.T) {
	_, initPriv := mustGenKey(t)
	respPub, respPriv := mustGenKey(t)
	impostorPub, _ := mustGenKey(t)

	a, b := pipePair()

	// Client handshake will eventually block on msg2; close the pipe to
	// unblock it once the server has rejected msg1.
	go func() {
		c, err := Client(a, initPriv, respPub)
		if err == nil {
			c.Close()
		}
	}()

	_, err := Server(b, respPriv, impostorPub)
	if err == nil {
		t.Fatalf("server accepted mismatched peer static")
	}
	if !errors.Is(err, ErrHandshakeFailed) {
		t.Errorf("server err = %v, want ErrHandshakeFailed", err)
	}
}

// TestHandshake_WrongResponderKey: the initiator thinks the responder
// has pubkey X, but the responder holds private key Y. The `es`/`ss`
// steps will compute different DH shared secrets on each side, and the
// first AEAD payload will fail to decrypt → responder rejects msg1.
func TestHandshake_WrongResponderKey(t *testing.T) {
	_, initPriv := mustGenKey(t)
	realRespPub, realRespPriv := mustGenKey(t)
	otherRespPub, _ := mustGenKey(t)
	_ = realRespPub

	a, b := pipePair()

	clientErr := make(chan error, 1)
	serverErr := make(chan error, 1)
	go func() {
		_, err := Client(a, initPriv, otherRespPub) // wrong pubkey
		clientErr <- err
	}()
	go func() {
		// Responder uses its real priv; since it never pins its peer it
		// can only detect tampering via ReadMessage's AEAD failure.
		_, err := Server(b, realRespPriv, ed25519.PublicKey(otherRespPub))
		serverErr <- err
	}()

	// Server will fail to authenticate msg1 (ss DH secret mismatch).
	select {
	case err := <-serverErr:
		if err == nil {
			t.Fatalf("server accepted handshake against wrong responder key")
		}
		if !errors.Is(err, ErrHandshakeFailed) {
			t.Errorf("server err = %v, want ErrHandshakeFailed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server did not return")
	}
	// Unblock the client if still waiting on msg2.
	_ = a.Close()
	<-clientErr
}

// TestConn_LargePayload exercises the chunking path by sending more
// than maxFramePayload bytes in one Write.
func TestConn_LargePayload(t *testing.T) {
	initPub, initPriv := mustGenKey(t)
	respPub, respPriv := mustGenKey(t)

	a, b := pipePair()
	var cli, srv *Conn
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c, err := Client(a, initPriv, respPub)
		if err != nil {
			t.Errorf("client handshake: %v", err)
			return
		}
		cli = c
	}()
	go func() {
		defer wg.Done()
		s, err := Server(b, respPriv, initPub)
		if err != nil {
			t.Errorf("server handshake: %v", err)
			return
		}
		srv = s
	}()
	wg.Wait()
	if cli == nil || srv == nil {
		t.Fatal("handshake failed")
	}
	defer cli.Close()
	defer srv.Close()

	// 3 frames' worth (spans the chunking boundary twice).
	payload := make([]byte, maxFramePayload*3+7)
	for i := range payload {
		payload[i] = byte(i)
	}

	done := make(chan error, 1)
	go func() {
		_, err := cli.Write(payload)
		done <- err
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(srv, got); err != nil {
		t.Fatalf("read full: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("client write: %v", err)
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d: got %d, want %d", i, got[i], payload[i])
		}
	}
}

// TestEd25519ToX25519_RoundTrip: converting both sides independently
// should yield DH pairs that agree on the shared secret. We can't test
// the DH directly without another crypto call, but at minimum the
// public-key derivation must match what you'd get by scalar-mult of
// the private against the X25519 base. We verify by having the noise
// library itself run a handshake — if the math were wrong, the above
// round-trip test would already fail. So here we just check shapes.
func TestEd25519ToX25519_Shapes(t *testing.T) {
	_, priv := mustGenKey(t)
	xp, xpub, err := ed25519PrivToX25519(priv)
	if err != nil {
		t.Fatalf("priv convert: %v", err)
	}
	if len(xp) != 32 {
		t.Errorf("xpriv size = %d, want 32", len(xp))
	}
	if len(xpub) != 32 {
		t.Errorf("xpub size = %d, want 32", len(xpub))
	}
	// Clamp bits must be set.
	if xp[0]&7 != 0 {
		t.Errorf("xpriv low bits not cleared: %#x", xp[0])
	}
	if xp[31]&0x80 != 0 {
		t.Errorf("xpriv high bit not cleared: %#x", xp[31])
	}
	if xp[31]&0x40 == 0 {
		t.Errorf("xpriv bit 254 not set: %#x", xp[31])
	}
}
