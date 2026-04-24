package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate_GenerateAndReload(t *testing.T) {
	dir := t.TempDir()

	id1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(id1.Public) != ed25519.PublicKeySize {
		t.Fatalf("public key size = %d", len(id1.Public))
	}

	// Seed file must exist with 0600 perms.
	info, err := os.Stat(filepath.Join(dir, seedFile))
	if err != nil {
		t.Fatalf("stat seed: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("seed perms = %#o, want 0600", mode)
	}

	// Second load must return the same keypair.
	id2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !id1.Public.Equal(id2.Public) {
		t.Errorf("public key changed across loads")
	}

	// Round-trip the base64 form.
	pub, err := ParsePublic(id1.PublicBase64())
	if err != nil {
		t.Fatalf("parse public: %v", err)
	}
	if !pub.Equal(id1.Public) {
		t.Errorf("base64 round-trip mismatch")
	}
}

func TestKeyring_TOFU(t *testing.T) {
	dir := t.TempDir()

	kr, err := OpenKeyring(dir)
	if err != nil {
		t.Fatalf("open empty: %v", err)
	}

	pubA := mustGenPub(t)
	pubB := mustGenPub(t)

	// First encounter: firstSeen=true, no error.
	first, err := kr.Check("win-pc", pubA)
	if err != nil {
		t.Fatalf("first check: %v", err)
	}
	if !first {
		t.Errorf("firstSeen=false on first contact")
	}

	// Second encounter same key: firstSeen=false, no error.
	first, err = kr.Check("win-pc", pubA)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if first {
		t.Errorf("firstSeen=true on repeat contact")
	}

	// Mismatch: ErrUntrusted.
	_, err = kr.Check("win-pc", pubB)
	if !errors.Is(err, ErrUntrusted) {
		t.Errorf("mismatch error = %v, want ErrUntrusted", err)
	}

	// Reopen: entries persist.
	kr2, err := OpenKeyring(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	first, err = kr2.Check("win-pc", pubA)
	if err != nil {
		t.Fatalf("check after reopen: %v", err)
	}
	if first {
		t.Errorf("firstSeen=true after reload; persistence broken")
	}

	// Forget, then same key is new again.
	if err := kr2.Forget("win-pc"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	first, err = kr2.Check("win-pc", pubA)
	if err != nil {
		t.Fatalf("check after forget: %v", err)
	}
	if !first {
		t.Errorf("forget did not reset TOFU state")
	}
}

// mustGenPub returns a fresh Ed25519 pubkey or aborts the test. Wraps
// ed25519.GenerateKey, which has an awkward signature that trips up
// one-liners.
func mustGenPub(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return pub
}
