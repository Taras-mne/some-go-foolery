package ownerfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/webdav"
)

// TestNormalizeNFC_DecomposedPathHitsPrecomposedFile verifies the
// whole point of this layer: a viewer sends "Отчёт.pdf" in Unicode
// NFD (7 bytes in the Cyrillic base letters + combining breves), and
// the owner resolves that to the SAME inode as a file stored on disk
// under the NFC form. Without normalization Finder and Explorer would
// see two entries.
func TestNormalizeNFC_DecomposedPathHitsPrecomposedFile(t *testing.T) {
	dir := t.TempDir()

	nfc := "Отчёт.pdf"
	// Explicit NFD form: "ё" = U+0435 + U+0308
	nfd := "Отч" + "е\u0308" + "т.pdf"

	if nfc == nfd {
		t.Fatal("test setup broken: NFD and NFC forms should differ byte-wise")
	}

	// Seed disk in NFC form (as Windows/most editors would).
	if err := os.WriteFile(filepath.Join(dir, nfc), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	fs := NormalizeNFC(webdav.Dir(dir))

	// Viewer sends NFD — Stat must find the NFC file on disk.
	info, err := fs.Stat(context.Background(), nfd)
	if err != nil {
		t.Fatalf("Stat(NFD) after NFC seed: %v", err)
	}
	if info.Size() != 5 {
		t.Errorf("size = %d, want 5", info.Size())
	}

	// Reading via OpenFile(NFD) should stream the NFC file's content.
	f, err := fs.OpenFile(context.Background(), nfd, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(NFD): %v", err)
	}
	body, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("body = %q, want hello", body)
	}
}

// TestNormalizeNFC_WriteNormalizes asserts that a PUT with a decomposed
// path creates the file under the precomposed name on disk — so a
// subsequent directory listing only shows one entry regardless of who
// wrote it.
func TestNormalizeNFC_WriteNormalizes(t *testing.T) {
	dir := t.TempDir()
	nfd := "Отч" + "е\u0308" + "т.pdf"
	nfc := "Отчёт.pdf"

	fs := NormalizeNFC(webdav.Dir(dir))
	f, err := fs.OpenFile(context.Background(), nfd, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile write: %v", err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = f.Close()

	// Disk must have exactly one file, named in NFC.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("entries = %v, want exactly one", names)
	}
	if got := entries[0].Name(); got != nfc {
		t.Errorf("on-disk name = %q (%v bytes), want NFC %q (%v bytes)", got, len(got), nfc, len(nfc))
	}
}

// TestNormalizeNFC_RenameBothSidesCollapse confirms that renaming from
// one canonical form to another ends up with one file, not two.
func TestNormalizeNFC_RenameBothSidesCollapse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := NormalizeNFC(webdav.Dir(dir))
	nfd := "Отч" + "е\u0308" + "т.pdf"
	if err := fs.Rename(context.Background(), "a.txt", nfd); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d; want 1", len(entries))
	}
	if !strings.ContainsRune(entries[0].Name(), 'ё') {
		t.Errorf("expected NFC 'ё' in name %q, got composed codepoints", entries[0].Name())
	}
}
