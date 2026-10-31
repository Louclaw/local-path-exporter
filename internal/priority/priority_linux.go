// Package priority lowers CPU and I/O scheduling priority.
// Both calls are best-effort: a missing privilege is logged once and ignored.
package priority

import (
	"log/slog"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	niceOnce sync.Once
	ioOnce   sync.Once
)

// Lower sets nice 19 and the idle I/O class on the calling thread.
// Call it from a goroutine locked to its OS thread so the I/O class sticks
// for the duration of a walk.
func Lower() {
	if err := unix.Setpriority(unix.PRIO_PROCESS, 0, 19); err != nil {
		niceOnce.Do(func() {
			slog.Warn("nice failed", "err", err)
		})
	}
	// IOPRIO_WHO_PROCESS=1, IOPRIO_CLASS_IDLE=3, class in the high 13 bits.
	// who 0 is the calling thread. Best-effort: idle I/O needs no extra capability.
	const ioprioWhoProcess = 1
	const ioprioClassIdle = 3
	const ioprioClassShift = 13
	_, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassIdle<<ioprioClassShift)
	if errno != 0 {
		ioOnce.Do(func() {
			slog.Warn("ionice idle failed", "err", errno)
		})
	}
}
