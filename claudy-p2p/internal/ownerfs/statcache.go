// Stat / Readdir cache layer for the WebDAV server.
//
// Why this exists: Windows Mini-Redirector polls aggressively. When a
// macOS owner shares a folder with twenty installers and a Windows
// viewer browses it in Explorer, Mini-Redirector fires the same
// PROPFIND for the same file 10–20 times per second per file —
// hundreds of redundant calls overall. Each PROPFIND lands in
// webdav.Handler which calls FileSystem.Stat() (1 syscall) and, for
// directories, FileSystem.OpenFile + Readdir (1 syscall + N stats).
// During a concurrent multi-GB GET on the same DataChannel the
// metadata flood pinned dav-owner at ~95% CPU and starved the body
// io.Copy long enough that yamux flow control parked and the transfer
// stalled at the halfway point.
//
// Fix: a tiny TTL cache in front of the upstream FileSystem. Reads
// hit the cache on rapid duplicate calls; writes invalidate the
// affected entries. TTL is short (250 ms) so genuine local edits show
// up to viewers within ¼ second — the only correctness window worth
// buying back; everything else trades latency for throughput.
//
// Scope:
//   - Stat — cached. Hottest call by far.
//   - Readdir — cached as a snapshot of FileInfos when callers ask for
//     "all entries" (count <= 0). Bounded results aren't cached because
//     they imply an iterator the caller will continue draining.
//   - OpenFile, Mkdir, Rename, RemoveAll, Write — bypass the cache and
//     invalidate any stale entries.
//
// What this is NOT: a write-back cache, a content cache, or a search
// index. Body bytes for GET still stream straight from the upstream
// FileSystem, so 5 GB transfers don't sit in our heap.
package ownerfs

import (
	"context"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// statCacheTTL is the lifetime of a cache entry. Picked to balance
// "stale view of local edits" (capped at 250 ms — fine for a humans-
// only UX) against "absorb burst PROPFIND from Mini-Redirector"
// (>10 calls per file in 250 ms is typical, all served from cache).
const statCacheTTL = 250 * time.Millisecond

type statEntry struct {
	info    os.FileInfo
	err     error
	expires time.Time
}

type readdirEntry struct {
	infos   []os.FileInfo
	err     error
	expires time.Time
}

// CachedFS wraps an upstream webdav.FileSystem with a TTL cache for
// Stat and full-listing Readdir. All other methods pass through and
// invalidate the affected paths.
type CachedFS struct {
	inner webdav.FileSystem

	mu      sync.Mutex
	stats   map[string]statEntry
	readdir map[string]readdirEntry
}

// Cached wraps inner with a stat / readdir cache.
func Cached(inner webdav.FileSystem) webdav.FileSystem {
	return &CachedFS{
		inner:   inner,
		stats:   map[string]statEntry{},
		readdir: map[string]readdirEntry{},
	}
}

// normalize keeps cache keys consistent across `/`-vs-not, trailing
// slashes, and `.` segments — webdav.Handler hands us the path as
// received from PROPFIND which can vary by client.
func normalize(p string) string {
	if p == "" {
		return "/"
	}
	q := path.Clean("/" + strings.TrimSuffix(p, "/"))
	if q == "" {
		return "/"
	}
	return q
}

// invalidate drops any cached state for the given path AND for its
// parent directory listing — a write to /a/b.txt also invalidates
// the cached Readdir of /a so the new size / mtime / existence is
// visible on next PROPFIND of the directory.
func (c *CachedFS) invalidate(p string) {
	n := normalize(p)
	c.mu.Lock()
	delete(c.stats, n)
	parent := path.Dir(n)
	delete(c.readdir, parent)
	delete(c.stats, parent)
	c.mu.Unlock()
}

func (c *CachedFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	err := c.inner.Mkdir(ctx, name, perm)
	c.invalidate(name)
	return err
}

func (c *CachedFS) RemoveAll(ctx context.Context, name string) error {
	err := c.inner.RemoveAll(ctx, name)
	c.invalidate(name)
	return err
}

func (c *CachedFS) Rename(ctx context.Context, oldName, newName string) error {
	err := c.inner.Rename(ctx, oldName, newName)
	c.invalidate(oldName)
	c.invalidate(newName)
	return err
}

func (c *CachedFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n := normalize(name)
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.stats[n]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.info, e.err
	}
	c.mu.Unlock()

	info, err := c.inner.Stat(ctx, name)
	c.mu.Lock()
	c.stats[n] = statEntry{info: info, err: err, expires: now.Add(statCacheTTL)}
	c.mu.Unlock()
	return info, err
}

// OpenFile bypasses the cache for reads/writes themselves but, on a
// write-mode open, invalidates the path and its parent directory so
// the eventual close-and-flush is reflected in the next stat. We can't
// wait for Close to invalidate — webdav.File doesn't expose enough to
// hook the close cleanly without breaking sniffability for io.Reader.
func (c *CachedFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		c.invalidate(name)
	}
	f, err := c.inner.OpenFile(ctx, name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &cachedFile{File: f, fs: c, name: name, writable: flag&(os.O_WRONLY|os.O_RDWR) != 0}, nil
}

// cachedFile wraps the upstream File so directory listings can be
// cached and so writes invalidate parent state on Close.
type cachedFile struct {
	webdav.File
	fs       *CachedFS
	name     string
	writable bool
}

func (cf *cachedFile) Close() error {
	err := cf.File.Close()
	if cf.writable {
		cf.fs.invalidate(cf.name)
	}
	return err
}

// Readdir caches only the "drain everything" form (count <= 0) — that's
// what webdav.Handler uses to materialize a PROPFIND of a directory.
// Paginated calls (count > 0) consume an iterator that has internal
// state; caching those would require deeper plumbing for marginal
// benefit since webdav itself doesn't paginate.
func (cf *cachedFile) Readdir(count int) ([]os.FileInfo, error) {
	if count > 0 {
		return cf.File.Readdir(count)
	}
	n := normalize(cf.name)
	now := time.Now()
	cf.fs.mu.Lock()
	if e, ok := cf.fs.readdir[n]; ok && now.Before(e.expires) {
		cf.fs.mu.Unlock()
		// Hand back a copy so callers mutating the slice don't poison
		// the cached snapshot.
		out := make([]os.FileInfo, len(e.infos))
		copy(out, e.infos)
		return out, e.err
	}
	cf.fs.mu.Unlock()

	infos, err := cf.File.Readdir(-1)
	cf.fs.mu.Lock()
	if err == nil || err == io.EOF {
		// Cache a defensive copy. Also seed individual stat entries
		// — webdav.Handler typically follows Readdir with Stat per
		// child, and we already have those FileInfos in hand.
		snap := make([]os.FileInfo, len(infos))
		copy(snap, infos)
		cf.fs.readdir[n] = readdirEntry{infos: snap, err: err, expires: now.Add(statCacheTTL)}
		for _, child := range infos {
			childPath := path.Join(n, child.Name())
			cf.fs.stats[childPath] = statEntry{info: child, expires: now.Add(statCacheTTL)}
		}
	}
	cf.fs.mu.Unlock()
	return infos, err
}
