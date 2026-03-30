# Claudy Core

Turn your laptop into a personal cloud. Claudy exposes any local folder as a WebDAV drive accessible from any device — through a relay server that punches through NAT and firewalls.

```
[ your phone / browser ]
        ↓ WebDAV (HTTP Basic Auth)
    [ relay server ]
        ↓ WebSocket tunnel
  [ daemon on your laptop ]
        ↓ local filesystem
    ~/your-folder
```

---

## Components

| Binary | Role |
|--------|------|
| `relay` | Public server. Authenticates clients, proxies WebDAV over WebSocket tunnel |
| `daemon` | Runs on your laptop. Serves local folder as WebDAV, connects to relay |

---

## Requirements

- **Go 1.21+**
- A publicly reachable server for the relay (VPS, cloud instance)

---

## Build

```bash
git clone https://github.com/Taras-mne/some-go-foolery.git
cd some-go-foolery
go build -o relay ./cmd/relay/
go build -o daemon ./cmd/daemon/
```

---

## Relay — Setup

### macOS / Linux

```bash
# Run with defaults (port 8080, data in /var/lib/claudy)
./relay

# Custom port and data directory
PORT=80 DATA_DIR=/var/lib/claudy ./relay
```

Run as a background service:

```bash
# systemd (Linux)
sudo nano /etc/systemd/system/claudy-relay.service
```

```ini
[Unit]
Description=Claudy Relay
After=network.target

[Service]
ExecStart=/usr/local/bin/relay
Environment=PORT=80
Environment=DATA_DIR=/var/lib/claudy
Restart=always

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now claudy-relay
```

### Windows

```powershell
# PowerShell — run relay
$env:PORT = "8080"
$env:DATA_DIR = "C:\claudy\data"
.\relay.exe
```

Run as a Windows Service (using NSSM):

```powershell
nssm install claudy-relay C:\claudy\relay.exe
nssm set claudy-relay AppEnvironmentExtra PORT=8080 DATA_DIR=C:\claudy\data
nssm start claudy-relay
```

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Port to listen on |
| `DATA_DIR` | `/var/lib/claudy` | Directory for `users.json` and JWT secret |

---

## Daemon — Setup

### macOS

**First run** — interactive wizard opens automatically:

```bash
./daemon
```

The wizard will ask for:
1. Relay server URL (e.g. `http://your-server.com`)
2. Username and password (auto-registers if new)
3. Folder to share — native macOS folder picker opens

Config is saved to `~/.claudy/config.json` and reused on next start.

**Mount the drive in Finder:**

`Cmd+K` → `http://your-relay/dav/username/` → enter username + password

**Run in background:**

```bash
# launchd — create plist
nano ~/Library/LaunchAgents/com.claudy.daemon.plist
```

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.claudy.daemon</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/daemon</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/com.claudy.daemon.plist
```

---

### Linux

**First run:**

```bash
./daemon
```

The wizard will ask for relay URL, credentials, and folder. For folder picker it tries `zenity` (GNOME) or `kdialog` (KDE), falls back to text input.

**Mount the drive:**

```bash
# Install davfs2
sudo apt install davfs2

# Mount
sudo mount -t davfs http://your-relay/dav/username/ /mnt/claudy
```

**Run as a systemd user service:**

```bash
mkdir -p ~/.config/systemd/user
nano ~/.config/systemd/user/claudy-daemon.service
```

```ini
[Unit]
Description=Claudy Daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/daemon
Restart=always

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now claudy-daemon
```

---

### Windows

**First run** — run in PowerShell or cmd, wizard opens:

```powershell
.\daemon.exe
```

The wizard opens a native Windows folder browser dialog.

**Mount the drive in Explorer:**

1. Open **This PC** → **Map network drive**
2. Folder: `http://your-relay/dav/username/`
3. Check **Connect using different credentials**
4. Enter username + password

Or via command line:

```cmd
net use Z: http://your-relay/dav/username/ /user:username password /persistent:yes
```

**Run as a Windows Service:**

```powershell
nssm install claudy-daemon C:\claudy\daemon.exe
nssm start claudy-daemon
```

---

## Environment variables (daemon)

Override config file or skip the wizard entirely:

| Variable | Description |
|----------|-------------|
| `CLAUDY_RELAY` | Relay URL, e.g. `http://your-server.com` |
| `CLAUDY_USER` | Username |
| `CLAUDY_PASS` | Password |
| `CLAUDY_DIR` | Folder to share |

Example:

```bash
CLAUDY_RELAY=http://your-server.com \
CLAUDY_USER=alice \
CLAUDY_PASS=secret \
CLAUDY_DIR=/home/alice/files \
./daemon
```

---

## API

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/auth/register` | Register `{"username":"...","password":"..."}` |
| `POST` | `/auth/login` | Login → `{"token":"..."}` |
| `GET` | `/tunnel?token=<jwt>` | Daemon WebSocket connection |
| `*` | `/dav/<username>/…` | WebDAV access (Basic Auth) |
| `GET` | `/health` | `{"status":"ok","tunnels":N}` |

---

## Config file

Saved at `~/.claudy/config.json` (macOS/Linux) or `%USERPROFILE%\.claudy\config.json` (Windows):

```json
{
  "relay_url": "http://your-server.com",
  "username": "alice",
  "password": "secret",
  "share_dir": "/home/alice/files"
}
```
