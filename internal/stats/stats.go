// Package stats holds the pure arithmetic behind kubelet volume gauges.
package stats

// AvailableBytes is min(capacity-used, fsFree), never negative.
// capacity is the PVC storage request. fsFree is statfs space still free
// on the underlying filesystem (f_bavail * f_bsize).
func AvailableBytes(capacity, used, fsFree int64) int64 {
	if fsFree < 0 {
		fsFree = 0
	}
	if used < 0 {
		used = 0
	}
	if capacity < 0 {
		capacity = 0
	}
	var remaining int64
	if used < capacity {
		remaining = capacity - used
	}
	if fsFree < remaining {
		return fsFree
	}
	return remaining
}
