package peer

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// pairConns stands up two loopback PeerConnections, creates a DataChannel
// on the initiator side, waits for the answerer to receive it, and returns
// a Conn on each side. Cleanup closes both PCs.
func pairConns(t *testing.T) (initiator, answerer *Conn) {
	t.Helper()

	pcA, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pcA: %v", err)
	}
	pcB, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pcB: %v", err)
	}
	t.Cleanup(func() { pcA.Close(); pcB.Close() })

	// Forward ICE candidates between A and B.
	pcA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = pcB.AddICECandidate(c.ToJSON())
	})
	pcB.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = pcA.AddICECandidate(c.ToJSON())
	})

	// A creates DC first, then offer.
	dcA, err := pcA.CreateDataChannel("test", nil)
	if err != nil {
		t.Fatalf("create dc: %v", err)
	}

	gotDCB := make(chan *webrtc.DataChannel, 1)
	pcB.OnDataChannel(func(dc *webrtc.DataChannel) { gotDCB <- dc })

	offer, err := pcA.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := pcA.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local A: %v", err)
	}
	if err := pcB.SetRemoteDescription(offer); err != nil {
		t.Fatalf("set remote B: %v", err)
	}
	answer, err := pcB.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := pcB.SetLocalDescription(answer); err != nil {
		t.Fatalf("set local B: %v", err)
	}
	if err := pcA.SetRemoteDescription(answer); err != nil {
		t.Fatalf("set remote A: %v", err)
	}

	var dcB *webrtc.DataChannel
	select {
	case dcB = <-gotDCB:
	case <-time.After(10 * time.Second):
		t.Fatal("no incoming DataChannel on B")
	}

	// Wait for both channels to open.
	openA := make(chan struct{})
	openB := make(chan struct{})
	dcA.OnOpen(func() { close(openA) })
	dcB.OnOpen(func() { close(openB) })

	// Open may have fired before we registered; probe ReadyState too.
	waitOpen := func(dc *webrtc.DataChannel, ch chan struct{}) {
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			return
		}
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("dc %q did not open", dc.Label())
		}
	}
	waitOpen(dcA, openA)
	waitOpen(dcB, openB)

	return NewConn(dcA), NewConn(dcB)
}

func TestConn_EchoBytes(t *testing.T) {
	a, b := pairConns(t)
	defer a.Close()
	defer b.Close()

	const size = 1 << 20 // 1 MiB
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	// Writer goroutine on A.
	writeErr := make(chan error, 1)
	go func() {
		_, err := a.Write(payload)
		writeErr <- err
	}()

	// Read full payload on B.
	got := make([]byte, size)
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("Write: %v", err)
	}

	if sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatal("payload hash mismatch")
	}
}

func TestConn_SmallReadsSplitLargeFrame(t *testing.T) {
	// Single Write of size < maxMsgSize arrives as one frame; reader uses
	// a tiny buffer, which must drain via leftover.
	a, b := pairConns(t)
	defer a.Close()
	defer b.Close()

	msg := []byte("the quick brown fox jumps over the lazy dog")
	if _, err := a.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, 0, len(msg))
	buf := make([]byte, 7)
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	for len(got) < len(msg) {
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != string(msg) {
		t.Errorf("got %q, want %q", got, msg)
	}
}

func TestConn_ReadDeadline(t *testing.T) {
	a, b := pairConns(t)
	defer a.Close()
	defer b.Close()

	_ = b.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 16)
	_, err := b.Read(buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("err = %v, want ErrDeadlineExceeded", err)
	}
}

func TestConn_CloseUnblocksRead(t *testing.T) {
	a, b := pairConns(t)
	defer a.Close()

	done := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 16))
		done <- err
	}()

	time.Sleep(30 * time.Millisecond)
	_ = b.Close()

	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Errorf("err = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestConn_BackpressureBoundsBufferedAmount(t *testing.T) {
	// Regression test for the 500 MB .mov symptom: without BufferedAmount
	// watermarking, pion's outbound SCTP buffer can grow unbounded when
	// a writer is faster than the wire, starving other DCs. We assert
	// here that peer.Conn's flow-control keeps BufferedAmount capped
	// near highWaterMark even under a firehose of writes.
	a, b := pairConns(t)
	defer a.Close()
	defer b.Close()

	// Keep a reader alive on b so a's Sends actually get consumed end to
	// end — otherwise loopback pion may back up at a different layer and
	// confuse the test.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 32*1024)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()

	const total = 8 * 1024 * 1024 // 8 MiB is plenty to blow past 1 MiB
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i)
	}

	// Poll BufferedAmount from a separate goroutine during the writes.
	// We expect it to stay bounded — target is highWaterMark, but pion's
	// Send can race the check so we allow a generous slop (3x) to avoid
	// flakes. A missing flow-control regression would let the buffer
	// grow to ~8 MiB, well past this bound.
	peakCh := make(chan uint64, 1)
	stopPolling := make(chan struct{})
	go func() {
		var peak uint64
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopPolling:
				peakCh <- peak
				return
			case <-ticker.C:
				if v := a.dc.BufferedAmount(); v > peak {
					peak = v
				}
			}
		}
	}()

	writeErr := make(chan error, 1)
	go func() {
		written := 0
		for written < total {
			n, err := a.Write(payload)
			if err != nil {
				writeErr <- err
				return
			}
			written += n
		}
		writeErr <- nil
	}()

	if err := <-writeErr; err != nil {
		t.Fatalf("Write: %v", err)
	}
	close(stopPolling)
	peak := <-peakCh
	_ = a.Close()
	<-readerDone

	// Allow 3x highWaterMark as a slack upper bound. A broken flow control
	// would show the peak at close to `total` (8 MiB).
	maxAllowed := uint64(highWaterMark * 3)
	if peak > maxAllowed {
		t.Errorf("BufferedAmount peak = %d bytes; want <= %d (flow control broken)", peak, maxAllowed)
	}
	// Sanity: we DID go through flow-control territory, not just push a
	// tiny amount that never crossed the threshold. If peak is trivially
	// low the test isn't exercising anything interesting.
	if peak < lowWaterMark/2 {
		t.Logf("note: peak BufferedAmount was only %d bytes — test may not have exercised flow control under this load", peak)
	}
}

func TestConn_WriteLargerThanMaxMsg(t *testing.T) {
	// 100KB payload > maxMsgSize (16KB) forces chunking; reader must
	// reassemble via normal Read loop.
	a, b := pairConns(t)
	defer a.Close()
	defer b.Close()

	payload := make([]byte, 100*1024)
	for i := range payload {
		payload[i] = byte(i)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := a.Write(payload)
		errCh <- err
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i, v := range got {
		if v != byte(i) {
			t.Fatalf("byte %d = %d, want %d", i, v, byte(i))
		}
	}
}
