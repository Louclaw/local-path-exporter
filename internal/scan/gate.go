package scan

import "errors"

// ErrDeferred means this volume's full walk waits for a later cycle.
// The previous sample stays, and the error counter does not move.
var ErrDeferred = errors.New("full scan deferred to a later cycle")

// Gate allows one full walk per cycle. Arm names the path chosen for this cycle.
// An empty path defers every full walk.
type Gate struct {
	path string
}

// Arm selects the only volume that may run a full walk during this cycle.
func (g *Gate) Arm(path string) {
	if g == nil {
		return
	}
	g.path = path
}

// Allow returns ErrDeferred when full is set for a path other than the one Arm chose.
func (g *Gate) Allow(path string, full bool) error {
	if g == nil || !full || g.path == path {
		return nil
	}
	return ErrDeferred
}
