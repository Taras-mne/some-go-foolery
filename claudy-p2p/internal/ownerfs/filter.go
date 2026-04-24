// Package ownerfs wraps golang.org/x/net/webdav.FileSystem with filters
// that the plain disk FS should not ever expose to a remote viewer.
//
// Why this exists: Finder and Windows Explorer sprinkle per-folder
// scratch files across every directory they visit — AppleDouble
// sidecars (._NAME), .DS_Store, Thumbs.db, .Spotlight-V100 and friends.
// When a mac viewer mounts a windows owner via Claudy, each of these
// files turns into its own WebDAV round-trip (PROPFIND, LOCK, PUT,
// UNLOCK). During a bulk upload of two 500 MB .mov files we observed
// those tiny metadata calls opening fresh DataChannels that then timed
// out their Noise handshake because the SCTP pipeline was saturated by
// the real payload. Half the pain went away the moment we pretended
// these files don't exist.
//
// Policy:
//   - Reads of junk paths return fs.ErrNotExist (→ 404 via webdav).
//   - Writes to junk paths silently succeed but discard — Finder treats
//     the save as successful and stops retrying, while the owner disk
//     stays clean. This is the only path where we lie about success,
//     and only for files the user never typed a name for.
//   - Directory listings omit junk children so viewers never see them
//     either.
//
// Pure whitelist semantics on a known set of patterns; anything else
// passes through to the underlying FS untouched.
package ownerfs

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"
)

// JunkPrefixes are filename prefixes we hide entirely. AppleDouble
// sidecars (._NAME) are by far the highest-traffic case: Finder writes
// one next to every file it opens, and each triggers its own WebDAV
// request + DataChannel + Noise handshake.
var JunkPrefixes = []string{"._"}

// JunkExact is basename equality — avoids accidental matches on real
// files that happen to start with a dot. Covers the filesystem-junk
// patterns we've seen Finder and Windows Explorer create unasked:
var JunkExact = map[string]struct{}{
	".DS_Store":       {}, // Finder per-folder state
	".Spotlight-V100": {}, // macOS search index root
	".Trashes":        {}, // macOS per-volume trash
	".TemporaryItems": {}, // macOS drag+drop temp
	".fseventsd":      {}, // macOS filesystem events log
	".apdisk":         {}, // Time Machine per-volume metadata
	".localized":      {}, // locale marker
	"$RECYCLE.BIN":    {}, // Windows per-volume trash
	"desktop.ini":     {}, // Windows folder customization
	"Thumbs.db":       {}, // Windows thumbnail cache
}

// IsJunk returns true if the path's basename matches any filter
// pattern. Only the final element is inspected — inner directory
// components are never themselves filtered, so a legitimate file
// nested inside a directory Finder happened to put ._something into
// is still accessible once the junk layer is peeled away.
func IsJunk(name string) bool {
	base := path.Base(path.Clean(name))
	if _, ok := JunkExact[base]; ok {
		return true
	}
	for _, p := range JunkPrefixes {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// FilterJunk wraps a webdav.FileSystem to transparently hide files
// matched by IsJunk. See package doc for the exact policy.
func FilterJunk(inner webdav.FileSystem) webdav.FileSystem {
	return &filteredFS{inner: inner}
}

type filteredFS struct{ inner webdav.FileSystem }

func (f *filteredFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if IsJunk(name) {
		// Finder occasionally tries to mkdir .TemporaryItems; silently
		// humor it rather than returning an error that surfaces in the UI.
		return nil
	}
	return f.inner.Mkdir(ctx, name, perm)
}

func (f *filteredFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if IsJunk(name) {
		// Write or read-write → hand back a discarding File so PUT
		// completes successfully but nothing hits disk.
		if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
			return &discardFile{}, nil
		}
		// Pure read: pretend it never existed.
		return nil, fs.ErrNotExist
	}
	file, err := f.inner.OpenFile(ctx, name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &filterDirReads{File: file}, nil
}

func (f *filteredFS) RemoveAll(ctx context.Context, name string) error {
	if IsJunk(name) {
		return nil
	}
	return f.inner.RemoveAll(ctx, name)
}

func (f *filteredFS) Rename(ctx context.Context, oldName, newName string) error {
	// If either side of the rename is junk we treat it as a no-op.
	// Finder's atomic-save dance ("write tmp, rename to final") can
	// occasionally involve AppleDouble files; returning nil here keeps
	// it happy without touching disk.
	if IsJunk(oldName) || IsJunk(newName) {
		return nil
	}
	return f.inner.Rename(ctx, oldName, newName)
}

func (f *filteredFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if IsJunk(name) {
		return nil, fs.ErrNotExist
	}
	return f.inner.Stat(ctx, name)
}

// discardFile implements webdav.File as a write-only black hole: Writes
// succeed silently, Reads return EOF, seeks and closes are harmless
// zero-value operations. Used only for PUTs to junk paths.
type discardFile struct{}

func (*discardFile) Read(p []byte) (int, error)                   { return 0, io.EOF }
func (*discardFile) Seek(offset int64, whence int) (int64, error) { return 0, nil }
func (*discardFile) Readdir(count int) ([]os.FileInfo, error)     { return nil, nil }
func (*discardFile) Stat() (os.FileInfo, error)                   { return junkStat{}, nil }
func (*discardFile) Write(p []byte) (int, error)                  { return len(p), nil }
func (*discardFile) Close() error                                 { return nil }

// junkStat is a minimal FileInfo returned by discardFile.Stat().
type junkStat struct{}

func (junkStat) Name() string       { return "discarded" }
func (junkStat) Size() int64        { return 0 }
func (junkStat) Mode() os.FileMode  { return 0 }
func (junkStat) ModTime() time.Time { return time.Time{} }
func (junkStat) IsDir() bool        { return false }
func (junkStat) Sys() any           { return nil }

// filterDirReads wraps a real File so Readdir() omits junk children.
// Non-directory reads pass through unchanged because embed-forwarding
// covers Read/Seek/Write/Close/Stat without any extra work.
type filterDirReads struct{ webdav.File }

func (f *filterDirReads) Readdir(count int) ([]os.FileInfo, error) {
	entries, err := f.File.Readdir(count)
	if err != nil {
		return entries, err
	}
	// In-place filter to avoid an extra allocation per PROPFIND; the
	// underlying slice is ours to mutate.
	kept := entries[:0]
	for _, e := range entries {
		if IsJunk(e.Name()) {
			continue
		}
		kept = append(kept, e)
	}
	return kept, nil
}
