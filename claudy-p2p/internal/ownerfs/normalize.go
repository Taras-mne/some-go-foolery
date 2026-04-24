// NFD → NFC filename normalization.
//
// macOS' HFS+/APFS stores filenames in Unicode NFD (decomposed) form
// — "й" becomes two code points: U+0438 + U+0306. When Finder sends a
// PROPFIND or PUT over WebDAV, the path in the request line carries
// that decomposed form. Windows NTFS, by contrast, stores and returns
// names in whatever form the app passed in, but most tools (Explorer,
// PowerShell, most editors) use NFC (precomposed) by default. Result:
// a single logical file — "Отчёт.pdf" — shows up twice in the
// viewer's mount, because PROPFIND returns the NFC-encoded name the
// user typed on Windows AND the NFD-encoded name Finder created when
// the mac side re-saved it. Delete one, the other lingers. Copy one,
// get two.
//
// Fix: normalize every path the WebDAV layer hands us into NFC before
// it hits the underlying disk FS. The viewer may still send NFD (we
// can't change Finder), but on the owner side all paths collapse to
// a single canonical form, which means:
//   - PROPFIND returns one listing entry per file
//   - PUT/GET/DELETE target the same inode regardless of client form
//   - round-tripping a name through NFD and back yields NFC everywhere
//
// The wrap is lossless: NFC is the default for almost every piece of
// software that writes paths, so files already on disk stay accessible.
// Rare NFD-only filenames on disk (legacy HFS+ Time Machine imports,
// for instance) become invisible until the owner renames them to NFC
// — same tradeoff every other WebDAV server makes.

package ownerfs

import (
	"context"
	"os"

	"golang.org/x/net/webdav"
	"golang.org/x/text/unicode/norm"
)

// NormalizeNFC wraps a webdav.FileSystem so every path argument is
// canonicalised to Unicode NFC before reaching the underlying FS.
// Read-side path observations (e.g. FileInfo.Name from Readdir) are
// not altered: disk already returns whatever it stores, and clients
// that care about exact bytes (almost none) will handle it.
//
// Usage:
//
//	fs := ownerfs.NormalizeNFC(ownerfs.FilterJunk(webdav.Dir(dir)))
//
// Order matters: filter junk names first on their as-observed form
// (AppleDouble ._NAME doesn't depend on normalization), then
// normalize for the real FS.
func NormalizeNFC(inner webdav.FileSystem) webdav.FileSystem {
	return &normalizingFS{inner: inner}
}

type normalizingFS struct{ inner webdav.FileSystem }

func nfc(s string) string { return norm.NFC.String(s) }

func (n *normalizingFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return n.inner.Mkdir(ctx, nfc(name), perm)
}

func (n *normalizingFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	return n.inner.OpenFile(ctx, nfc(name), flag, perm)
}

func (n *normalizingFS) RemoveAll(ctx context.Context, name string) error {
	return n.inner.RemoveAll(ctx, nfc(name))
}

func (n *normalizingFS) Rename(ctx context.Context, oldName, newName string) error {
	return n.inner.Rename(ctx, nfc(oldName), nfc(newName))
}

func (n *normalizingFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	return n.inner.Stat(ctx, nfc(name))
}
