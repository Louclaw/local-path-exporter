// Package cgroup reads the process cgroup memory counters and asks cgroup v2
// to reclaim file cache and slab. GOMEMLIMIT only bounds the Go heap. A walk
// of a large tree charges dentries, inodes and directory pages to the cgroup,
// and that is what pushes a 128Mi pod over its limit.
package cgroup

import (
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Snapshot is one reading of the cgroup and the Go heap.
// Zero fields with OK false mean the cgroup files could not be read.
type Snapshot struct {
	Current           uint64
	Anon              uint64
	File              uint64
	SlabReclaimable   uint64
	SlabUnreclaimable uint64
	Kernel            uint64
	// Max is 0 when the controller has no finite limit.
	Max  uint64
	Heap uint64
	OK   bool
}

const (
	// noteEvery is how many walked entries elapse between pressure checks.
	// Reclaiming too often, or too far below the working set, drops inode
	// pages that the walk immediately reads back.
	noteEvery = 4096
	// maxManaged is the largest memory.max this process will reclaim against.
	// A developer cgroup of many GiB must not be shrunk by the test suite.
	maxManaged = 512 << 20
)

var (
	locateOnce sync.Once
	located    mount
	override   string

	reclaimOnce sync.Once
	// allowReclaim is false in measurements that simulate a read-only cgroup.
	allowReclaim = true
)

type mount struct {
	dir string
	v2  bool
}

// SetRootForTest points reads and reclaim at a fake cgroup directory.
func SetRootForTest(dir string) {
	override = dir
}

// SetReclaimEnabled turns memory.reclaim writes on or off.
// A non-privileged k3s pod gets EROFS from that file; measurements pass false.
func SetReclaimEnabled(on bool) {
	allowReclaim = on
}

// Read returns the current counters. Heap is always filled.
func Read() Snapshot {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s := Snapshot{Heap: m.HeapAlloc}
	mnt, ok := find()
	if !ok {
		return s
	}
	if mnt.v2 {
		s = readV2(mnt.dir, s)
	} else {
		s = readV1(mnt.dir, s)
	}
	return s
}

// Note is called once per walked entry. Every noteEvery entries it reclaims
// when a container-sized memory.max is under pressure.
func Note(entries int64) {
	if entries <= 0 || entries%noteEvery != 0 {
		return
	}
	relieve(false)
}

// Release reclaims after a volume so file cache and slab do not sit in the
// cgroup until the next cycle.
func Release() {
	relieve(true)
}

func relieve(force bool) {
	mnt, ok := find()
	if !ok || !mnt.v2 {
		return
	}
	curText, err := os.ReadFile(mnt.dir + "/memory.current")
	if err != nil {
		return
	}
	cur, ok := parseUint(string(curText))
	if !ok {
		return
	}
	maxBytes, finite := readMax(mnt.dir + "/memory.max")
	if !finite || maxBytes == 0 || maxBytes > maxManaged {
		return
	}
	// During the walk, trim once usage passes 5/8 of the limit, down to 9/16.
	// On a 128Mi limit that is 80Mi down to 72Mi. After the volume, trim to half.
	// A non-privileged pod often gets EROFS here; the write is optional.
	high := maxBytes * 5 / 8
	target := maxBytes * 9 / 16
	if force {
		target = maxBytes / 2
	}
	if cur <= target || (!force && cur <= high) {
		return
	}
	want := cur - target
	if !allowReclaim {
		return
	}
	err = os.WriteFile(mnt.dir+"/memory.reclaim", []byte(strconv.FormatUint(want, 10)+"\n"), 0)
	if err == nil || errors.Is(err, unix.EAGAIN) || errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.EROFS) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		if err != nil {
			reclaimOnce.Do(func() {
				slog.Warn("cgroup memory.reclaim unavailable", "err", err)
			})
		}
		return
	}
	reclaimOnce.Do(func() {
		slog.Warn("cgroup memory.reclaim", "err", err)
	})
}

func find() (mount, bool) {
	if override != "" {
		return mount{dir: override, v2: true}, true
	}
	locateOnce.Do(func() {
		data, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			return
		}
		v2path, v1path := parseProc(string(data))
		if v2path != "" {
			dir := "/sys/fs/cgroup" + v2path
			if _, err := os.Stat(dir + "/memory.current"); err == nil {
				located = mount{dir: dir, v2: true}
				return
			}
		}
		if _, err := os.Stat("/sys/fs/cgroup/memory.current"); err == nil {
			located = mount{dir: "/sys/fs/cgroup", v2: true}
			return
		}
		if v1path != "" {
			dir := "/sys/fs/cgroup/memory" + v1path
			if _, err := os.Stat(dir + "/memory.usage_in_bytes"); err == nil {
				located = mount{dir: dir, v2: false}
				return
			}
		}
		if _, err := os.Stat("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil {
			located = mount{dir: "/sys/fs/cgroup/memory", v2: false}
		}
	})
	return located, located.dir != ""
}

func parseProc(text string) (v2path, v1path string) {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			v2path = parts[2]
			if v2path == "" {
				v2path = "/"
			}
		}
		for _, c := range strings.Split(parts[1], ",") {
			if c == "memory" {
				v1path = parts[2]
				if v1path == "" {
					v1path = "/"
				}
			}
		}
	}
	return v2path, v1path
}

func readV2(dir string, s Snapshot) Snapshot {
	stat := parseStat(dir + "/memory.stat")
	cur, ok := readNumber(dir + "/memory.current")
	if !ok && len(stat) == 0 {
		return s
	}
	s.OK = true
	s.Current = cur
	s.Anon = stat["anon"]
	s.File = stat["file"]
	s.SlabReclaimable = stat["slab_reclaimable"]
	s.SlabUnreclaimable = stat["slab_unreclaimable"]
	s.Kernel = stat["kernel"]
	if maxBytes, finite := readMax(dir + "/memory.max"); finite {
		s.Max = maxBytes
	}
	return s
}

func readV1(dir string, s Snapshot) Snapshot {
	stat := parseStat(dir + "/memory.stat")
	cur, ok := readNumber(dir + "/memory.usage_in_bytes")
	if !ok && len(stat) == 0 {
		return s
	}
	s.OK = true
	s.Current = cur
	s.Anon = stat["anon"]
	if s.Anon == 0 {
		s.Anon = stat["rss"]
	}
	s.File = stat["file"]
	if s.File == 0 {
		s.File = stat["cache"]
	}
	s.SlabReclaimable = stat["slab_reclaimable"]
	s.SlabUnreclaimable = stat["slab_unreclaimable"]
	s.Kernel = stat["kernel"]
	if s.Kernel == 0 {
		if k, ok := readNumber(dir + "/memory.kmem.usage_in_bytes"); ok {
			s.Kernel = k
		}
	}
	if maxBytes, finite := readMax(dir + "/memory.limit_in_bytes"); finite {
		s.Max = maxBytes
	}
	return s
}

func parseStat(path string) map[string]uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if n, ok := parseUint(f[1]); ok {
			out[f[0]] = n
		}
	}
	return out
}

func readNumber(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return parseUint(string(b))
}

func readMax(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(b))
	if text == "" || text == "max" {
		return 0, false
	}
	n, ok := parseUint(text)
	if !ok {
		return 0, false
	}
	// v1 reports "unlimited" as a value near 2^63.
	if n > 1<<50 {
		return 0, false
	}
	return n, true
}

func parseUint(s string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
