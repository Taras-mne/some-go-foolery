// Package identity manages the node's long-term Ed25519 keypair and a
// Trust-On-First-Use keyring of remote peer public keys.
//
// Design:
//   - Each node owns a persistent Ed25519 signing key. This is the basis
//     of identity for both signaling (proof of who is joining the room)
//     and later Noise handshakes (the signing key is converted to an
//     X25519 static key via the standard Ed25519-to-X25519 mapping when
//     Step 5 lands).
//   - The private key is stored as a raw 32-byte seed at <dir>/identity.key
//     with 0600 perms. The public key is derived every load; we don't
//     store it separately to avoid drift.
//   - First-run generates a fresh key. Subsequent runs load the same one
//     so peers can pin us on the other side.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// seedFile is the basename of the private-seed file inside the identity dir.
const seedFile = "identity.key"

// Identity bundles the Ed25519 keypair. The private key is the
// 64-byte ed25519.PrivateKey (which internally is seed||pub).
type Identity struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// PublicBase64 returns the 32-byte public key encoded as unpadded
// base64 — the canonical on-wire form used in signaling envelopes.
func (id *Identity) PublicBase64() string {
	return base64.RawStdEncoding.EncodeToString(id.Public)
}

// LoadOrCreate returns the node's identity, creating dir and generating
// a fresh keypair if no identity.key exists yet. dir is expected to be
// something like "~/.claudy" (callers should resolve home themselves).
func LoadOrCreate(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir identity dir: %w", err)
	}
	path := filepath.Join(dir, seedFile)

	seed, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, fmt.Errorf("generate seed: %w", err)
		}
		// Write with tight perms before anyone else can observe.
		if err := os.WriteFile(path, seed, 0o600); err != nil {
			return nil, fmt.Errorf("write seed: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("read seed: %w", err)
	default:
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("seed file %s: want %d bytes, got %d", path, ed25519.SeedSize, len(seed))
		}
	}

	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{Private: priv, Public: pub}, nil
}

// ParsePublic decodes a base64-encoded 32-byte Ed25519 public key, as
// it appears on the wire. Returns an error on any length/format issue
// so callers can refuse malformed peers immediately.
func ParsePublic(s string) (ed25519.PublicKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode pubkey: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pubkey size: want %d, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
