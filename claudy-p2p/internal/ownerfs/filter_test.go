package ownerfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"golang.org/x/net/webdav"
)

func TestIsJunk(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"video.mov", false},
		{"Document.pdf", false},

		// AppleDouble sidecars in various positions
		{"._video.mov", true},
		{"/folder/._video.mov", true},
		{"/._root.mov", true},

		// Inner directories named junk should not affect their children
		{"/.DS_Store/legit.txt", false},
		{"/._weird/child.txt", false},

		// Exact matches
		{".DS_Store", true},
		{"/folder/.DS_Store", true},
		{"Thumbs.db", true},
		{"/nested/deep/desktop.ini", true},

		// Similar names that are NOT junk
		{"my.DS_Store_backup", false}, // not exact match
		{"not_._junk", false},         // not prefix
	}

	for _, tc := range cases {
		if got := IsJunk(tc.in); got != tc.want {
			t.Errorf("IsJunk(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestFilterJunk_Stat_HidesJunkReturnsForReal covers the most common
// webdav path: PROPFIND → Stat. Junk files must appear not to exist,
// real files must still work.
func TestFilterJunk_Stat_HidesJunkReturnsForReal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real.txt"), "hi")
	writeFile(t, filepath.Join(dir, "._real.txt"), "appledouble")
	writeFile(t, filepath.Join(dir, ".DS_Store"), "{}")

	wfs := FilterJunk(webdav.Dir(dir))
	ctx := context.Background()

	if _, err := wfs.Stat(ctx, "real.txt"); err != nil {
		t.Errorf("Stat real.txt: unexpected err %v", err)
	}
	if _, err := wfs.Stat(ctx, "._real.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat ._real.txt err = %v, want ErrNotExist", err)
	}
	if _, err := wfs.Stat(ctx, ".DS_Store"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat .DS_Store err = %v, want ErrNotExist", err)
	}
}

// TestFilterJunk_OpenFile_ReadHidden confirms a read on a junk file
// fails with ErrNotExist (→ 404 on the wire) regardless of whether
// the file exists on disk.
func TestFilterJunk_OpenFile_ReadHidden(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "._x"), "payload")

	wfs := FilterJunk(webdav.Dir(dir))
	_, err := wfs.OpenFile(context.Background(), "._x", os.O_RDONLY, 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenFile ._x = %v, want ErrNotExist", err)
	}
}

// TestFilterJunk_OpenFile_WriteDiscards confirms a Finder PUT on a
// junk path succeeds (Finder is happy) but nothing hits the disk.
func TestFilterJunk_OpenFile_WriteDiscards(t *testing.T) {
	dir := t.TempDir()
	wfs := FilterJunk(webdav.Dir(dir))

	f, err := wfs.OpenFile(context.Background(), "._new.mov", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile write: %v", err)
	}
	n, err := f.Write([]byte("AppleDouble metadata blob"))
	if err != nil || n != len("AppleDouble metadata blob") {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Crucially: nothing was written to disk.
	if _, err := os.Stat(filepath.Join(dir, "._new.mov")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file leaked to disk: err = %v, want ErrNotExist", err)
	}
}

// TestFilterJunk_Readdir_FiltersChildren covers PROPFIND on a directory:
// the returned listing must not include junk entries even if they exist
// on disk (someone may have copied a .DS_Store in via another channel).
func TestFilterJunk_Readdir_FiltersChildren(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	writeFile(t, filepath.Join(dir, ".DS_Store"), "{}")
	writeFile(t, filepath.Join(dir, "._a.txt"), "ad")

	wfs := FilterJunk(webdav.Dir(dir))
	root, err := wfs.OpenFile(context.Background(), "/", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile /: %v", err)
	}
	defer root.Close()

	entries, err := root.Readdir(-1)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	want := []string{"a.txt", "b.txt"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

// TestFilterJunk_MkdirRemoveRename_AcceptsJunk confirms the silent
// no-op policy for directory/path operations on junk names: Finder's
// control flow proceeds without a 403 or 500 in its face.
func TestFilterJunk_MkdirRemoveRename_AcceptsJunk(t *testing.T) {
	dir := t.TempDir()
	wfs := FilterJunk(webdav.Dir(dir))
	ctx := context.Background()

	if err := wfs.Mkdir(ctx, ".TemporaryItems", 0o755); err != nil {
		t.Errorf("Mkdir junk: %v", err)
	}
	if err := wfs.RemoveAll(ctx, "._whatever"); err != nil {
		t.Errorf("RemoveAll junk: %v", err)
	}
	if err := wfs.Rename(ctx, "._old", "._new"); err != nil {
		t.Errorf("Rename junk→junk: %v", err)
	}
	// Real file untouched:
	writeFile(t, filepath.Join(dir, "real.txt"), "x")
	if _, err := wfs.Stat(ctx, "real.txt"); err != nil {
		t.Errorf("Stat real.txt: %v", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
