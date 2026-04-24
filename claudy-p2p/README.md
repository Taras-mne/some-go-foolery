# Claudy P2P prototype

WebRTC DataChannel hello-world: owner ↔ viewer matched by `room` ID, SDP
exchanged through a tiny WebSocket signaling server. Goal is to validate the
building blocks before wiring WebDAV and Noise on top.

## Layout

- `cmd/signal` — signaling relay (room-based SDP/ICE forwarder)
- `cmd/owner`  — advertises a room, accepts one peer, echoes back
- `cmd/viewer` — joins a room, opens a DataChannel, sends "hello"
- `internal/signaling` — WS message types + client helper
- `internal/peer` — `pion/webrtc` wrapper (ICE config, offer/answer glue)

## Quick run

    # terminal 1
    go run ./cmd/signal                    # :7000

    # terminal 2
    go run ./cmd/owner -room demo          # waits for viewer

    # terminal 3
    go run ./cmd/viewer -room demo -msg hi # connects, sends "hi"

Viewer prints the echoed message and exits. Owner keeps running.

## Next steps (do not implement yet)

1. Replace string payload with `net.Conn` adapter over DataChannel.
2. Serve `golang.org/x/net/webdav.Handler` on that Conn.
3. Add Noise_IK handshake before WebDAV.
4. Add TURN server config to `internal/peer` ICE servers.
