// Package peer: Conn wraps a *webrtc.DataChannel as net.Conn so that any
// byte-stream consumer (http.Server, webdav.Handler, io.Copy) can use it
// without change.
//
// Design:
//   - DataChannel is message-oriented; net.Conn is byte-oriented.
//   - Read drains an internal channel that OnMessage pushes frames into.
//     A leftover buffer handles partial reads when the caller's slice is
//     smaller than the incoming frame.
//   - Write sends via DataChannel.Send. Large payloads are chunked to stay
//     under maxMsgSize (SCTP/DTLS cap in pion is ~16KB; we use 16384).
//   - Flow control: pion queues Send calls into an internal SCTP send
//     buffer. Without backpressure a multi-hundred-MB upload fills that
//     buffer faster than the wire can drain it, which (1) starves every
//     other DataChannel on the same PeerConnection — Noise handshakes on
//     fresh DCs time out, and (2) lets the buffer grow into hundreds of
//     MB of heap. Write now monitors BufferedAmount and blocks once it
//     crosses a high water mark, resuming on pion's OnBufferedAmountLow
//     signal (the pattern pion's own docs recommend for bulk transfers).
//   - Deadlines are best-effort: a time.AfterFunc cancels the internal
//     context, unblocking Read/Write with os.ErrDeadlineExceeded.
//   - Close is idempotent and signals both sides via done channel + DC.Close.
package peer

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// maxMsgSize is the largest chunk we will hand to DataChannel.Send.
// pion's default SCTP max message size is 65536 but interop with other
// WebRTC stacks is safest under 16KB.
const maxMsgSize = 16 * 1024

// Flow-control watermarks. Bumped from 1 MiB / 256 KiB after WAN testing.
//
// Why the change: at 1 MiB highWaterMark, throughput is window-limited
// to ~ 1 MiB / RTT per tunnel. On a TURN relay leg (RTT ~150 ms) that
// caps the entire session at ~7 MB/s regardless of how many yamux
// streams ride it — peer.Conn is the bottleneck below yamux. Bumping
// the high water mark to 8 MiB lifts that ceiling to ~50 MB/s on the
// same path; on srflx (~50 ms RTT) we go from ~20 MB/s to ~160 MB/s.
//
//   - highWaterMark (8 MiB): when BufferedAmount exceeds this, Write
//     blocks until pion's OnBufferedAmountLow fires. Bigger than the
//     SCTP send queue we expect any single PUT to keep buzzing, small
//     enough that the head-of-line blocking on this DC stays under
//     ~2 s on a slow uplink (≈ 5 MB/s × 8 MiB = 1.6 s worst-case
//     latency for a control message queued behind a body burst).
//   - lowWaterMark (2 MiB): pion fires the resume callback when the
//     outbound buffer drops below this. Hysteresis gap of 6 MiB keeps
//     us in long write-resume cycles instead of flipping on every
//     Send, which is what kept the visible throughput pulsating to 0.
//
// Memory cost: each tunnel can hold up to highWaterMark bytes in pion's
// outbound queue plus the same in our incoming `in` buffer (16 MiB
// already). One additional 8 MiB worst case per tunnel × number of
// active sessions. Acceptable on any laptop made in the last decade.
const (
	highWaterMark = 8 * 1024 * 1024
	lowWaterMark  = 2 * 1024 * 1024
)

// fakeAddr implements net.Addr for Local/RemoteAddr — WebDAV/http.Server
// only use these for logging; the value is opaque.
type fakeAddr struct{ label string }

func (f fakeAddr) Network() string { return "webrtc" }
func (f fakeAddr) String() string  { return f.label }

// Conn is a net.Conn backed by a WebRTC DataChannel.
type Conn struct {
	dc *webrtc.DataChannel

	// in carries complete incoming frames from OnMessage to Read.
	in chan []byte
	// leftover holds the tail of a frame when the caller's read buffer
	// was smaller than the frame. Guarded by readMu.
	leftover []byte
	readMu   sync.Mutex

	// writeMu serializes Write so a single logical Write that is split
	// into multiple Sends stays contiguous.
	writeMu sync.Mutex

	// bufLow receives a signal every time pion reports that the outbound
	// buffer has dropped below lowWaterMark. Buffer size 1 — we only
	// need to know "draining happened since last check", not every event.
	bufLow chan struct{}

	// ctx is cancelled on Close or when either deadline fires.
	ctx    context.Context
	cancel context.CancelFunc

	readDeadline  atomicTimer
	writeDeadline atomicTimer

	closeOnce sync.Once
	closeErr  error
}

// NewConn wires OnMessage/OnClose handlers and returns a ready Conn.
// The DataChannel must already be open, or will be shortly — Read blocks
// until the first frame arrives anyway.
func NewConn(dc *webrtc.DataChannel) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		dc:     dc,
		// Buffer big enough that a slow consumer (yamux demux + disk
		// writer at the receiver end of a multi-GB PUT) doesn't park
		// pion's per-DataChannel read goroutine. Originally 16 — that
		// gave us ~256 KB of headroom (16 × 16 KB max-msg) and stalled
		// hard on a 6 GB upload: when NTFS write briefly throttled,
		// OnMessage blocked sending into this channel, the pion reader
		// stalled, every yamux stream on the tunnel froze including
		// signaling keepalives, and the WS got reaped by pong timeout.
		// 1024 entries × 16 KB = ~16 MB worst-case heap, which is
		// trivial next to the body it's buffering.
		in:     make(chan []byte, 1024),
		bufLow: make(chan struct{}, 1),
		ctx:    ctx,
		cancel: cancel,
	}

	// Flow control: ask pion to notify us when its outbound buffer drops
	// below lowWaterMark. Write() uses this to unblock after spending
	// time parked on a full buffer.
	dc.SetBufferedAmountLowThreshold(lowWaterMark)
	dc.OnBufferedAmountLow(func() {
		// Non-blocking send: a pending signal is enough for the waiting
		// writer to re-check; we drop any further signals until consumed.
		select {
		case c.bufLow <- struct{}{}:
		default:
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		// Copy because pion reuses the underlying buffer.
		buf := make([]byte, len(msg.Data))
		copy(buf, msg.Data)
		select {
		case c.in <- buf:
		case <-c.ctx.Done():
		}
	})
	dc.OnClose(func() {
		_ = c.Close()
	})
	dc.OnError(func(error) {
		_ = c.Close()
	})
	return c
}

// Read implements net.Conn. It first returns any leftover bytes from a
// previous frame that overflowed the caller's buffer, then blocks for the
// next frame.
func (c *Conn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if len(c.leftover) > 0 {
		n := copy(p, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}

	deadlineCh := c.readDeadline.channel()

	select {
	case <-c.ctx.Done():
		return 0, io.EOF
	case <-deadlineCh:
		return 0, os.ErrDeadlineExceeded
	case frame, ok := <-c.in:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, frame)
		if n < len(frame) {
			c.leftover = frame[n:]
		}
		return n, nil
	}
}

// Write implements net.Conn. Payloads larger than maxMsgSize are split
// into multiple Send calls; before each Send we apply backpressure so the
// SCTP send buffer never grows beyond highWaterMark.
//
// Backpressure details: if BufferedAmount is above highWaterMark we park
// on c.bufLow until pion's OnBufferedAmountLow fires, then re-check and
// loop. Without this guard, a single large io.Copy would pin ~all of the
// PC's outbound bandwidth and any concurrent DC's Noise handshake would
// time out — exactly what we observed in production when uploading a
// 500 MB .mov through Finder.
func (c *Conn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	total := 0
	for len(p) > 0 {
		// Fast-path cancellation before touching the chunk.
		deadlineCh := c.writeDeadline.channel()
		select {
		case <-c.ctx.Done():
			return total, io.ErrClosedPipe
		case <-deadlineCh:
			return total, os.ErrDeadlineExceeded
		default:
		}

		// Backpressure: if pion's outbound buffer is already high, wait
		// for it to drain. Re-check in a loop because OnBufferedAmountLow
		// may fire while we still have pending Sends to issue — each
		// Send bumps the buffer, so we might trip the high water mark
		// again within the same Write call.
		for c.dc.BufferedAmount() > highWaterMark {
			select {
			case <-c.bufLow:
				// draining observed; loop re-checks BufferedAmount
			case <-c.ctx.Done():
				return total, io.ErrClosedPipe
			case <-deadlineCh:
				return total, os.ErrDeadlineExceeded
			}
		}

		chunk := p
		if len(chunk) > maxMsgSize {
			chunk = p[:maxMsgSize]
		}
		if err := c.dc.Send(chunk); err != nil {
			if errors.Is(err, io.ErrClosedPipe) || c.ctx.Err() != nil {
				return total, io.ErrClosedPipe
			}
			return total, err
		}
		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}

// Close tears down the Conn. Safe to call multiple times.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.closeErr = c.dc.Close()
	})
	return c.closeErr
}

func (c *Conn) LocalAddr() net.Addr  { return fakeAddr{label: "local:" + c.dc.Label()} }
func (c *Conn) RemoteAddr() net.Addr { return fakeAddr{label: "remote:" + c.dc.Label()} }

func (c *Conn) SetDeadline(t time.Time) error {
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error  { c.readDeadline.set(t); return nil }
func (c *Conn) SetWriteDeadline(t time.Time) error { c.writeDeadline.set(t); return nil }

// atomicTimer wraps a time.Timer that fires a channel at the deadline.
// set() cancels the previous timer if any. Zero time disables.
type atomicTimer struct {
	mu    sync.Mutex
	timer *time.Timer
	fired chan struct{}
}

func (a *atomicTimer) set(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.fired = nil
	if t.IsZero() {
		return
	}
	ch := make(chan struct{})
	a.fired = ch
	d := time.Until(t)
	if d <= 0 {
		close(ch)
		return
	}
	a.timer = time.AfterFunc(d, func() { close(ch) })
}

// channel returns the current fire channel or nil if no deadline is set.
// A nil channel blocks forever in select, which is the desired behavior.
func (a *atomicTimer) channel() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fired
}
