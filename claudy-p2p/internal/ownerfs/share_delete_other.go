//go:build !windows

package ownerfs

import (
	"context"
	"os"

	"golang.org/x/net/webdav"
)

// On POSIX open files don't block unlink/rename. shareDeleteDir is a
// thin pass-through to webdav.Dir.
type shareDeleteDir string

func (d shareDeleteDir) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return webdav.Dir(d).Mkdir(ctx, name, perm)
}

func (d shareDeleteDir) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	return webdav.Dir(d).OpenFile(ctx, name, flag, perm)
}

func (d shareDeleteDir) RemoveAll(ctx context.Context, name string) error {
	return webdav.Dir(d).RemoveAll(ctx, name)
}

func (d shareDeleteDir) Rename(ctx context.Context, oldName, newName string) error {
	return webdav.Dir(d).Rename(ctx, oldName, newName)
}

func (d shareDeleteDir) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	return webdav.Dir(d).Stat(ctx, name)
}
