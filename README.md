it's just me and my friend fooling around with AI and go, nothing big

## Local testing

Open three terminals.

**Terminal 1 — Hub:**
```
go run hub.go
```

**Terminal 2 — Node** (serves files from `./shared_files/`):
```
go run node.go localhost:8000 mynode mypassword
```

**Terminal 3 — Client:**
```
go run client.go localhost:8000 mynode mypassword
```

Then map `http://localhost:8080/` as a network drive:
- **Windows:** File Explorer → This PC → Map network drive → enter the URL
- **macOS:** Finder → Go → Connect to Server → enter the URL
- **Linux:** `davfs2`: `sudo mount -t davfs http://localhost:8080 /mnt/point`
