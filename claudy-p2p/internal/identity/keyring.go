package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// keyringFile is the basename of the TOFU keyring inside the identity dir.
const keyringFile = "known_peers.json"

// ErrUntrusted is returned by Check when the given pubkey does not match
// the pinned entry. Callers MUST abort the connection — someone is either
// running a different node or the signaling server is mitm'ing us.
var ErrUntrusted = errors.New("peer pubkey does not match pinned entry")

// KnownPeer is one entry in the TOFU keyring.
type KnownPeer struct {
	// Alias is a human-readable tag ("mac-laptop", "home-pc"). In the
	// current MVP it doubles as the lookup key — room IDs change, keys
	// don't. Callers pass the alias they want to pin/check against.
	Alias string `json:"alias"`
	// PubkeyB64 is the 32-byte Ed25519 public key, base64 (unpadded).
	PubkeyB64 string `json:"pubkey"`
	// FirstSeen records when we accepted the TOFU pin.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is bumped on every successful reunion.
	LastSeen time.Time `json:"last_seen"`
}

// Keyring is a simple JSON-backed TOFU store. Writes go through a tmp
// file + rename so a crash mid-save cannot corrupt the pin list.
type Keyring struct {
	path string

	mu    sync.Mutex
	peers map[string]*KnownPeer // alias → entry
}

// OpenKeyring loads the JSON file at <dir>/known_peers.json, creating
// an empty one if it doesn't exist. Safe to call concurrently; the
// returned *Keyring is itself thread-safe.
func OpenKeyring(dir string) (*Keyring, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir keyring dir: %w", err)
	}
	path := filepath.Join(dir, keyringFile)
	kr := &Keyring{path: path, peers: map[string]*KnownPeer{}}

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return kr, nil
	case err != nil:
		return nil, fmt.Errorf("read keyring: %w", err)
	}

	var entries []KnownPeer
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse keyring: %w", err)
	}
	for i := range entries {
		e := entries[i]
		kr.peers[e.Alias] = &e
	}
	return kr, nil
}

// Check looks up alias and compares against pub.
//
// Return values:
//   - firstSeen=true, err=nil   → no entry existed; pub has just been pinned.
//   - firstSeen=false, err=nil  → entry matched; LastSeen updated on disk.
//   - err=ErrUntrusted          → entry existed but pubkey differs — abort.
//
// In all success cases the keyring is persisted before Check returns.
func (k *Keyring) Check(alias string, pub ed25519.PublicKey) (firstSeen bool, err error) {
	if len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("pubkey size: got %d", len(pub))
	}
	pubB64 := base64.RawStdEncoding.EncodeToString(pub)

	k.mu.Lock()
	defer k.mu.Unlock()

	now := time.Now().UTC()
	entry, ok := k.peers[alias]
	if !ok {
		k.peers[alias] = &KnownPeer{
			Alias:     alias,
			PubkeyB64: pubB64,
			FirstSeen: now,
			LastSeen:  now,
		}
		return true, k.saveLocked()
	}
	if entry.PubkeyB64 != pubB64 {
		return false, fmt.Errorf("%w: alias=%q pinned=%s got=%s",
			ErrUntrusted, alias, entry.PubkeyB64, pubB64)
	}
	entry.LastSeen = now
	return false, k.saveLocked()
}

// Forget drops a pinned alias. Intended for UI-driven "forget this peer"
// flows; not used in any automated codepath.
func (k *Keyring) Forget(alias string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.peers[alias]; !ok {
		return nil
	}
	delete(k.peers, alias)
	return k.saveLocked()
}

// List returns a snapshot of all pinned peers, sorted by alias for
// deterministic output (useful in tests and `claudy peers` CLI later).
func (k *Keyring) List() []KnownPeer {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]KnownPeer, 0, len(k.peers))
	for _, e := range k.peers {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// saveLocked writes the keyring atomically. Caller must hold k.mu.
func (k *Keyring) saveLocked() error {
	entries := make([]KnownPeer, 0, len(k.peers))
	for _, e := range k.peers {
		entries = append(entries, *e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Alias < entries[j].Alias })

	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keyring: %w", err)
	}

	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write tmp keyring: %w", err)
	}
	if err := os.Rename(tmp, k.path); err != nil {
		return fmt.Errorf("rename keyring: %w", err)
	}
	return nil
}
