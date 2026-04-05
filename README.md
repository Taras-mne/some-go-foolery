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
| `daemon` | Runs on your laptop. Serves local folder as WebDAV, connects to relay. Includes a system tray icon on macOS, Windows, and Linux |

---

## Requirements

- **Go 1.21+**
- A publicly reachable server for the relay (VPS, cloud instance)

---

## Build

**macOS / Linux**

```bash
git clone https://github.com/Taras-mne/some-go-foolery.git
cd some-go-foolery
go build -o relay ./cmd/relay/
go build -o daemon ./cmd/daemon/
```

**Windows** (PowerShell)

```powershell
git clone https://github.com/Taras-mne/some-go-foolery.git
cd some-go-foolery
go build -o relay.exe ./cmd/relay/
go build -o daemon.exe ./cmd/daemon/
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
Environment=BASE_URL=https://your-relay.com
Environment=SMTP_HOST=smtp.gmail.com
Environment=SMTP_PORT=587
Environment=SMTP_USER=noreply@your-domain.com
Environment=SMTP_PASS=your-app-password
Environment=SMTP_FROM=Claudy <noreply@your-domain.com>
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
$env:BASE_URL = "https://your-relay.com"
$env:SMTP_HOST = "smtp.gmail.com"
$env:SMTP_PORT = "587"
$env:SMTP_USER = "noreply@your-domain.com"
$env:SMTP_PASS = "your-app-password"
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
| `BASE_URL` | `http://localhost:<PORT>` | Public relay URL — used in verification email links |
| `SMTP_HOST` | — | SMTP server hostname, e.g. `smtp.gmail.com` |
| `SMTP_PORT` | `587` | SMTP port (STARTTLS) |
| `SMTP_USER` | — | SMTP auth username |
| `SMTP_PASS` | — | SMTP password or app password |
| `SMTP_FROM` | SMTP_USER | From address, e.g. `Claudy <noreply@example.com>` |

> If SMTP is not configured, the relay logs the verification token to stdout instead of emailing it. Useful for local development.

---

## Daemon — Setup

### macOS

**First run** — interactive wizard opens automatically:

```bash
./daemon
```

The wizard will ask for:
1. Relay server URL (e.g. `http://your-server.com`)
2. Username and password
3. Whether to create a new account or log in to an existing one
   - If creating: email address (a verification link will be sent)
4. Folder to share — native macOS folder picker opens

A system tray icon appears in the menu bar showing connection status with options to open the web UI, change the shared folder, and quit.

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

The wizard will ask for relay URL, credentials, email (for new accounts), and folder. For folder picker it tries `zenity` (GNOME) or `kdialog` (KDE), falls back to text input. A system tray icon is shown if a compatible desktop environment is available.

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

### Windows — local setup (relay + daemon on the same machine)

This is the simplest way to get a WebDAV network drive on Windows without a remote server.

**1. Build**

```powershell
git clone https://github.com/Taras-mne/some-go-foolery.git
cd some-go-foolery
go build -o relay.exe ./cmd/relay/
go build -o daemon.exe ./cmd/daemon/
```

**2. One-time registry fix** (allows Basic auth over HTTP — run once as Administrator)

```powershell
Start-Process powershell -Verb RunAs -ArgumentList `
  '-Command Set-ItemProperty `
  "HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters" `
  -Name BasicAuthLevel -Value 2 -Type DWord; `
  Restart-Service WebClient'
```

**3. Create `start.ps1`** — starts relay + daemon on every boot

```powershell
$proj     = "C:\path\to\some-go-foolery"   # <-- change this
$dataDir  = "$env:USERPROFILE\.claudy-data"
$shareDir = "C:\path\to\folder-to-share"   # <-- change this
$user     = "admin"
$pass     = "adminpass"                    # <-- change this
$email    = "you@example.com"              # <-- change this

New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
Get-Process -Name relay,daemon -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep 1

$env:DATA_DIR = $dataDir; $env:PORT = "8080"
Start-Process "$proj\relay.exe" -WorkingDirectory $proj `
    -RedirectStandardError "$proj\relay.log" -WindowStyle Hidden
Start-Sleep 3

# Register account (only succeeds on first run; safe to run every time)
try {
    $body = '{"username":"' + $user + '","password":"' + $pass + '","email":"' + $email + '"}'
    Invoke-RestMethod http://localhost:8080/auth/register -Method Post `
        -Body $body -ContentType "application/json" | Out-Null
    Write-Host "Check $email for a verification link before the daemon can log in."
} catch {}

$env:CLAUDY_RELAY = "http://localhost:8080"
$env:CLAUDY_USER  = $user
$env:CLAUDY_PASS  = $pass
$env:CLAUDY_DIR   = $shareDir
$env:CLAUDY_NO_TRAY = "1"   # headless — no tray icon needed when running as a service
Start-Process "$proj\daemon.exe" -WorkingDirectory $proj `
    -RedirectStandardError "$proj\daemon.log" -WindowStyle Hidden
```

> **Note:** On first run, verify your email before starting the daemon. Once verified, subsequent runs of `start.ps1` will skip registration and go straight to login.

**4. Create `map_drive.ps1`** — maps the drive (run after `start.ps1`)

```powershell
net use W: /delete /y 2>$null
net use W: \\localhost@8080\DavWWWRoot /user:admin adminpass /persistent:yes
```

**5. Run**

```powershell
powershell -ExecutionPolicy Bypass -File .\start.ps1
Start-Sleep 5
powershell -ExecutionPolicy Bypass -File .\map_drive.ps1
```

**6. Open the web UI**

```
http://localhost:8080
```

Log in with the username and password you set in `start.ps1`.

**7. Open the drive in Explorer**

Your folder now appears as drive `W:` in File Explorer.

> **Note:** To re-map `W:` after a reboot, run `start.ps1` then `map_drive.ps1` again. Or add both to Task Scheduler with trigger "At log on".

**Mount the drive from a remote relay (non-local setup):**

1. Open **This PC** → **Map network drive**
2. Folder: `\\your-relay@80\DavWWWRoot\dav\username\`
3. Check **Connect using different credentials** → enter username + password

Or via command line:

```cmd
net use Z: \\your-relay@80\DavWWWRoot\dav\username\ /user:username password /persistent:yes
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
| `CLAUDY_NO_TRAY` | Set to `1` to disable the system tray icon (headless/service mode) |

Example:

```bash
CLAUDY_RELAY=http://your-server.com \
CLAUDY_USER=alice \
CLAUDY_PASS=secret \
CLAUDY_DIR=/home/alice/files \
./daemon
```

---

## Registration and email verification

When creating a new account, Claudy sends a verification link to the provided email address. The account cannot be used until the link is clicked.

**Flow:**

1. Register → receive verification email
2. Click the link → account activated
3. Log in normally

**API:**

```bash
# 1. Register
curl -X POST https://your-relay.com/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"secret","email":"alice@example.com"}'
# → {"status":"ok","message":"check your email to verify your account"}

# 2. Verify (link from email)
curl https://your-relay.com/auth/verify?token=<token>
# → {"status":"ok","message":"email verified — you can now log in"}

# 3. Login
curl -X POST https://your-relay.com/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"secret"}'
# → {"token":"<jwt>"}
```

**Development without SMTP:**

If `SMTP_HOST` is not set, the relay prints the verify token to stdout:

```
[auth] registered: alice <alice@example.com> — verify token: 8a99080ec969962016eb7062a8d94f2c
```

Verify manually:

```bash
curl http://localhost:8080/auth/verify?token=8a99080ec969962016eb7062a8d94f2c
```

---

## API

| Method | Endpoint | Body / Notes |
|--------|----------|-------------|
| `POST` | `/auth/register` | `{"username":"…","password":"…","email":"…"}` — sends verification email |
| `POST` | `/auth/login` | `{"username":"…","password":"…"}` → `{"token":"…"}` |
| `GET` | `/auth/verify?token=<token>` | Activates account from email link |
| `GET` | `/tunnel?token=<jwt>` | Daemon WebSocket connection |
| `*` | `/dav/<username>/…` | WebDAV access (Basic Auth: username + password or JWT) |
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
