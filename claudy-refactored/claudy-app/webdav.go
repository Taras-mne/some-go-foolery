package main

import (
	"net/http"
	"sync"

	"golang.org/x/net/webdav"
)

// dynamicDAV wraps a webdav.Handler whose share directory can be swapped at runtime.
type dynamicDAV struct {
	mu sync.RWMutex
	h  *webdav.Handler
}

func newDynamicDAV(prefix, dir string) *dynamicDAV {
	d := &dynamicDAV{}
	d.SetDir(prefix, dir)
	return d
}

func (d *dynamicDAV) SetDir(prefix, dir string) {
	h := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: webdav.Dir(dir),
		LockSystem: webdav.NewMemLS(),
	}
	d.mu.Lock()
	d.h = h
	d.mu.Unlock()
}

func (d *dynamicDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	h := d.h
	d.mu.RUnlock()
	if h == nil {
		http.Error(w, "share not configured", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}
