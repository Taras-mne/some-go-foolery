// Per-platform OpenFile that controls Windows share-mode flags.
//
// Why: stock os.OpenFile on Windows opens a file with FILE_SHARE_READ |
// FILE_SHARE_WRITE share mode. That keeps `unlink` and `rename` blocked
// while any handle is open. So when a remote viewer streams a 200 MB
// file out of the owner's WebDAV root, the local user trying to delete
// or move it gets "file in use". Linux and macOS already permit unlink
// on open files, so users (and our owner code) implicitly assume POSIX
// semantics. This package provides a webdav.FileSystem wrapper that
// adds FILE_SHARE_DELETE on Windows; on other platforms it's a no-op
// pass-through to webdav.Dir.
//
// Risk: with FILE_SHARE_DELETE in effect, an in-flight WebDAV transfer
// can be interrupted mid-stream by a local rm/rename. The viewer sees
// a truncated GET or a PUT writing into an unlinked inode. Both are
// already possible on Linux/macOS owners and the existing code copes
// (Finder retries, stale data is overwritten on the next attempt).
// Net: behavior on Windows now matches the other two OSes.
package ownerfs

import "golang.org/x/net/webdav"

// ShareDeleteDir wraps webdav.Dir so that open files don't block local
// delete/rename. On non-Windows builds this is the identity wrapper.
func ShareDeleteDir(dir string) webdav.FileSystem {
	return shareDeleteDir(dir)
}
