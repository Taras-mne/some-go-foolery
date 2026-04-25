//go:build windows

package ownerfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/net/webdav"
)

// shareDeleteDir mirrors webdav.Dir but opens files via syscall.CreateFile
// with FILE_SHARE_DELETE in the share-mode mask. That lets the local
// user delete or rename files inside the shared folder while a remote
// viewer still has them open for read/write — matching POSIX semantics
// users already expect from a "shared folder".
//
// We replicate webdav.Dir's path resolution (jail check + slash-clean)
// because it's not exported. All other FileSystem methods delegate to
// the stock webdav.Dir, since Mkdir / RemoveAll / Rename / Stat don't
// open long-lived handles and so don't trip Windows' default share mode.
type shareDeleteDir string

func (d shareDeleteDir) inner() webdav.Dir { return webdav.Dir(d) }

func (d shareDeleteDir) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return d.inner().Mkdir(ctx, name, perm)
}

func (d shareDeleteDir) RemoveAll(ctx context.Context, name string) error {
	return d.inner().RemoveAll(ctx, name)
}

func (d shareDeleteDir) Rename(ctx context.Context, oldName, newName string) error {
	return d.inner().Rename(ctx, oldName, newName)
}

func (d shareDeleteDir) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	return d.inner().Stat(ctx, name)
}

// resolve mirrors webdav.Dir.resolve: reject path-traversal characters,
// then join against the root using filesystem-native separators. Any
// slashes inside `name` are converted to platform separators by
// filepath.FromSlash. Empty input is treated as the root.
func (d shareDeleteDir) resolve(name string) string {
	if filepath.Separator != '/' && strings.IndexRune(name, filepath.Separator) >= 0 {
		return ""
	}
	if strings.Contains(name, "\x00") {
		return ""
	}
	root := string(d)
	if root == "" {
		root = "."
	}
	return filepath.Join(root, filepath.FromSlash(slashClean(name)))
}

// slashClean canonicalizes the WebDAV path. Unlike path.Clean it always
// keeps a single leading slash so subsequent FromSlash + filepath.Join
// land inside the root rather than at the volume root.
func slashClean(name string) string {
	if name == "" || name[0] != '/' {
		name = "/" + name
	}
	return name
}

// OpenFile maps webdav.FileSystem flags to Windows access/share/disposition,
// then opens the file via syscall.CreateFile with FILE_SHARE_DELETE
// added. The returned *os.File works exactly like one from os.OpenFile
// (Read/Write/Seek/Close), so webdav.Handler can drive it without
// caring how it was obtained.
func (d shareDeleteDir) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	full := d.resolve(name)
	if full == "" {
		return nil, os.ErrNotExist
	}
	pathPtr, err := syscall.UTF16PtrFromString(full)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: full, Err: err}
	}

	// Directories on Windows can only be opened via CreateFile when we
	// pass FILE_FLAG_BACKUP_SEMANTICS in dwFlagsAndAttributes. Without
	// this flag, opening a directory returns ERROR_ACCESS_DENIED — which
	// silently broke webdav.Handler's PROPFIND on the share root (it
	// reads the root via OpenFile + Readdir). Probe with os.Stat so we
	// only set the flag when we really need it; for regular files it'd
	// be harmless but it's nice to keep the flag set tight.
	var attrs uint32 = syscall.FILE_ATTRIBUTE_NORMAL
	if info, statErr := os.Stat(full); statErr == nil && info.IsDir() {
		attrs = syscall.FILE_FLAG_BACKUP_SEMANTICS
	}

	// Map os.OpenFile flags to Windows GENERIC_READ/WRITE.
	var access uint32
	switch {
	case flag&os.O_WRONLY != 0:
		access = syscall.GENERIC_WRITE
	case flag&os.O_RDWR != 0:
		access = syscall.GENERIC_READ | syscall.GENERIC_WRITE
	default:
		access = syscall.GENERIC_READ
	}
	if flag&os.O_APPEND != 0 {
		// FILE_APPEND_DATA without GENERIC_WRITE is enough for append, but
		// callers commonly combine O_APPEND with O_WRONLY / O_RDWR; granting
		// GENERIC_WRITE is the simpler superset.
		access |= syscall.GENERIC_WRITE
	}

	// The whole point of this file: include FILE_SHARE_DELETE so the
	// local user can rm/mv even with our handle live.
	share := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE)

	// Map create/excl/trunc combinations to CreateFile's CreationDisposition.
	// (Same matrix Go's os.openFileNolog uses internally.)
	var disposition uint32
	switch {
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		disposition = syscall.CREATE_NEW
	case flag&(os.O_CREATE|os.O_TRUNC) == os.O_CREATE|os.O_TRUNC:
		disposition = syscall.CREATE_ALWAYS
	case flag&os.O_CREATE != 0:
		disposition = syscall.OPEN_ALWAYS
	case flag&os.O_TRUNC != 0:
		disposition = syscall.TRUNCATE_EXISTING
	default:
		disposition = syscall.OPEN_EXISTING
	}

	handle, err := syscall.CreateFile(
		pathPtr,
		access,
		share,
		nil, // default security descriptor
		disposition,
		attrs,
		0, // no template
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: full, Err: err}
	}

	f := os.NewFile(uintptr(handle), full)
	if f == nil {
		_ = syscall.CloseHandle(handle)
		return nil, errors.New("ownerfs: NewFile returned nil")
	}
	if flag&os.O_APPEND != 0 {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}
