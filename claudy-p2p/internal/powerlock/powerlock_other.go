//go:build !windows && !darwin && !linux

package powerlock

import (
	"context"
	"errors"
	"log/slog"
)

// Unknown OS: best-effort means "return an error and let Acquire fall
// back to a no-op lock". Nothing prevents sleep, but the owner still
// runs normally. Users on exotic platforms can report a preferred
// mechanism and we'll add a build-tagged file for them.
func acquirePlatform(_ context.Context, _ *slog.Logger) (releaser, error) {
	return nil, errors.New("prevent-sleep not implemented on this platform")
}
