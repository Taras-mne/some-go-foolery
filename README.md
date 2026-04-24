# Claudy

Turn a computer you already own into a personal cloud — no relay, no account,
no data custodian. Two Claudy peers find each other through a tiny signaling
broker, then move bytes directly over an authenticated WebRTC DataChannel.
On the viewer side the remote folder shows up as a native WebDAV mount in
Finder / File Explorer / `mount.davfs`.

```
 [ viewer laptop ]                         [ owner laptop ]
  Finder mount                              local folder
       │                                         │
       ▼                                         ▼
  local HTTP :8910 ── TCP ──┐         ┌── TCP ── WebDAV server
                            │         │
                            ▼         ▼
                      ┌───────────────────┐
                      │  WebRTC DataChan  │   ← Noise_IK inside DTLS
                      └───────────────────┘
                            ▲         ▲
                            │         │
                  SDP/ICE ──┘         └── SDP/ICE
                            │         │
                            ▼         ▼
                        [ signaling broker ]   ← only sees public pubkeys
                          (public VPS)           and encrypted SDP blobs
```

## Components

All code lives under [`claudy-p2p/`](./claudy-p2p) as a self-contained Go
module.

| Binary | Role |
|--------|------|
| `signal` | Stateless WebSocket broker. Pairs one viewer + one owner per room, forwards SDP/ICE, announces pubkeys. Never sees file bytes. |
| `dav-owner` | Runs where your files live. Serves a local directory via `golang.org/x/net/webdav` on top of a DataChannel listener. |
| `dav-client` | Runs where you want to browse from. Exposes a local HTTP port you can mount with WebDAV; every inbound request rides a fresh DataChannel to the owner. |

Legacy demos `owner` / `viewer` / `fileserv` are retained as manual smoke
tests; they are not part of the supported mount path.

## Security

- **Identity**: each host generates an Ed25519 keypair on first run,
  persisted as a 32-byte seed at `~/.claudy/identity.key` (mode 0600).
- **TOFU**: the first time you pair with a peer under an alias
  (`-peer-alias mac-laptop`), the peer's pubkey is pinned in
  `~/.claudy/known_peers.json`. Subsequent connections that present a
  different key are refused with `ErrUntrusted`.
- **Transport**: every DataChannel is wrapped in Noise_IK (DH25519 /
  ChaCha20-Poly1305 / BLAKE2b) *inside* the WebRTC DTLS layer. A
  compromised signaling relay cannot substitute DTLS fingerprints
  without also forging ownership of a pinned private key, so the attack
  surface of the broker is limited to denial-of-service.

## Quick start

Build:

```bash
cd claudy-p2p
go build -o dist/signal     ./cmd/signal
go build -o dist/dav-owner  ./cmd/dav-owner
go build -o dist/dav-client ./cmd/dav-client
```

Cross-compile for other platforms by setting `GOOS`/`GOARCH`, e.g.
`GOOS=windows GOARCH=amd64 go build -o dist/dav-owner.exe ./cmd/dav-owner`.

Run on three machines (or three terminals, if you just want a loopback
smoke test):

```bash
# 1. Public VPS — just a dumb rendezvous point.
./signal -addr :7042

# 2. Your laptop with the files.
./dav-owner \
    -signal ws://<vps>:7042/signal \
    -room   myroom \
    -dir    ~/share \
    -peer-alias laptop-viewer

# 3. The machine you want to read from.
./dav-client \
    -signal ws://<vps>:7042/signal \
    -room   myroom \
    -local  127.0.0.1:8910 \
    -peer-alias desktop-owner
```

Then mount the viewer's local port:

```bash
# macOS
mount_webdav -v claudy "http://127.0.0.1:8910/" /tmp/claudy

# Linux
sudo mount -t davfs http://127.0.0.1:8910/ /mnt/claudy

# Windows: Cmd+K in Explorer → http://127.0.0.1:8910/
```

## Status

Phase A (serverless data path) is implemented and verified on WAN across
macOS arm64 ↔ Windows amd64. Next up: TURN fallback for symmetric NATs,
integration into a system-tray daemon, and threat-model docs. See
`claudy-p2p/README.md` for per-package details.
