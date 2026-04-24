package peer

import (
	"errors"
	"net"
	"sync"
)

// Listener adapts a stream of incoming *Conn objects (one per viewer
// DataChannel) into a net.Listener so that http.Serve and friends can
// drive it without modification.
//
// The Listener is deliberately decoupled from signaling and PeerConnection
// setup: callers push ready *Conn instances onto the incoming channel
// provided at construction time. This keeps the type testable with in-
// process pion pairs and reusable regardless of how peers are discovered.
type Listener struct {
	incoming <-chan *Conn
	addr     net.Addr

	closeOnce sync.Once
	closed    chan struct{}
}

// ErrListenerClosed is returned from Accept after Close.
var ErrListenerClosed = errors.New("peer: listener closed")

// NewListener wraps the given channel. The caller retains ownership of the
// channel and is responsible for closing it if desired; closing the channel
// is treated as a permanent end-of-stream (Accept returns ErrListenerClosed).
func NewListener(incoming <-chan *Conn, label string) *Listener {
	return &Listener{
		incoming: incoming,
		addr:     fakeAddr{label: "listener:" + label},
		closed:   make(chan struct{}),
	}
}

// Accept blocks until the next peer connection is available.
func (l *Listener) Accept() (net.Conn, error) {
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

// Close is idempotent. It does not close the underlying incoming channel —
// the caller may still be producing Conns and will observe no receiver.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

// Addr returns a synthetic address; callers only use it for logging.
func (l *Listener) Addr() net.Addr { return l.addr }
