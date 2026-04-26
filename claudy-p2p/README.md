# Claudy

Share a folder directly between two computers — no cloud, no account.
Pair via a 6-character code, mount the friend's folder as a drive in
Finder / Explorer / Files. Files stream peer-to-peer over a WebRTC
DataChannel encrypted with Noise_IK; the only thing that ever touches
our server is the tiny pairing handshake.

## Download

| OS | Bundle |
|---|---|
| macOS (Apple Silicon + Intel) | http://23.172.217.149:8080/Claudy-mac.zip |
| Windows 10 / 11 (x64) | http://23.172.217.149:8080/Claudy-windows-tunnel.zip |
| Linux x86_64 | http://23.172.217.149:8080/Claudy-linux-amd64.tar.gz |
| Linux ARM64 | http://23.172.217.149:8080/Claudy-linux-arm64.tar.gz |

Run: extract the bundle, double-click `claudy` (or `./claudy` from
terminal on Linux). A browser opens with the UI on `127.0.0.1:<random>`.

## Architecture

- **One DataChannel per session** ("tunnel") with Noise_IK once at
  setup; every WebDAV request rides a yamux stream over it.
- **Direct P2P** via WebRTC ICE: host-host on LAN, srflx-srflx
  through NAT. Falls back to the project's TURN server only when
  both sides have symmetric NAT.
- **TOFU identity pinning**: each peer's Ed25519 pubkey is recorded
  on first contact in `~/.claudy/known_peers.json`; subsequent
  sessions verify the pin before any byte flows.

```
cmd/
  claudy/        UI wrapper — local web UI, spawns dav-{owner,client}
  dav-owner/     WebDAV server — exposes a folder over P2P
  dav-client/    WebDAV proxy — turns a remote owner into a local mount
  signal/        Signaling relay — pairs peers by room, forwards SDP/ICE
internal/
  peer/          pion/webrtc + DataChannel net.Conn wrapper
  tunnel/        Noise + yamux session over a DataChannel
  secure/        Noise_IK transport
  signaling/     WebSocket signal client
  identity/      Ed25519 keypair + TOFU keyring
  ownerfs/       webdav.FileSystem layers (junk filter, NFC, Win
                 share-delete, stat cache)
  powerlock/     keep-system-awake hooks
```

## Running from source

```bash
go build -o claudy ./cmd/claudy/
go build -o claudy-dav-owner ./cmd/dav-owner/
go build -o claudy-dav-client ./cmd/dav-client/
./claudy
```

`claudy.exe` resolves the sibling binaries via `os.Executable() →
filepath.Dir`. They must live in the same directory.

The signaling server is hosted at `ws://23.172.217.149:7042/signal`
by default. Override with `CLAUDY_SIGNAL_URL=ws://your-host:7042/signal`.

## Per-OS notes

### macOS

Mount uses `mount_webdav` (kernel-mode WebDAV client, builtin). The
mount appears as `claudy-<room>` in Finder's sidebar.

If Gatekeeper blocks the unsigned binary on first launch:
- Right-click → Open
- Or `xattr -cr Claudy.app` to clear the quarantine bit.

### Windows

Mount uses Windows WebClient (Mini-Redirector) via `net use`. Two
known limitations are inherent to that built-in client, not our code:

- **50 MB file size cap** by default. Lift it once via PowerShell as
  Admin:
  ```powershell
  Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters' `
    -Name FileSizeLimitInBytes -Value 4294967295 -Type DWord
  Restart-Service WebClient
  ```
- **WebClient service must be running** (default is "Manual" on Win11
  Home). Start it once:
  ```powershell
  Set-Service WebClient -StartupType Automatic
  Start-Service WebClient
  ```

### Linux

Mount uses `gio mount dav://...` (GVFS, userspace, no `sudo`). The
mount appears under `/run/user/$UID/gvfs/dav:host=...,port=...` and
in Files / Nautilus / Dolphin sidebars.

Requirements: `gio` and a running gvfs daemon. Pre-installed on
GNOME, KDE, XFCE, Cinnamon, Pantheon, Budgie. On minimal installs:

```bash
# Debian / Ubuntu
sudo apt install gvfs-backends

# Fedora / RHEL
sudo dnf install gvfs-fuse
```

Troubleshooting "paired but never reached connected":

- **SELinux** (Fedora / RHEL / Rocky): may block pion's UDP
  socket bind for ICE. Check:
  ```bash
  sudo ausearch -m avc -ts recent | grep claudy
  ```
  If you see `denied { name_bind }` events: move claudy to
  `/usr/local/bin` so the default `bin_t` context applies, or run
  `sudo setenforce 0` temporarily to confirm SELinux is the cause.

- **AppArmor** (Ubuntu, strict policy): similar — check
  `/var/log/syslog` for `apparmor="DENIED"`.

- **Firewall blocking outbound UDP to STUN/TURN**:
  ```bash
  # Quick reachability check
  nc -u -w 3 stun.cloudflare.com 3478
  nc -u -w 3 23.172.217.149 3478
  ```
  Both should "succeed". If timeout → firewall.

## Tests

```bash
go test ./...
```

Most tests are pion-loopback and don't require network. The webdav
integration test creates two PeerConnections in-process.

## Beta / development

The signaling server, TURN, and download host all live on a single
DigitalOcean droplet (`23.172.217.149`). For self-hosting, run
`cmd/signal` anywhere reachable and set `CLAUDY_SIGNAL_URL`.
