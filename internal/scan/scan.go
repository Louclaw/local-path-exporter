// Package scan measures one PVC directory without crossing filesystems
// or following symlinks. Hard links are counted once per inode.
// Only inodes with nlink > 1 are remembered: a regular file cannot appear
// again, so the dedup set stays proportional to hard links and directories
// rather than to every file. Directory entries are read in fixed-size batches
// and files are counted immediately, so a large cache directory is not held
// in memory. Allocated size is st_blocks * 512.
package scan

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/Louclaw/local-path-exporter/internal/cgroup"
)

// Usage is the allocated size and the number of directory entries counted.
// Entries is every name walked, including a hard link seen again.
// Inodes counts each inode once.
type Usage struct {
	Bytes   int64
	Inodes  int64
	Entries int64
}

// Report adds filesystem-wide statfs figures for the volume's device.
// TotalBytes, Files and FilesFree describe that whole filesystem.
type Report struct {
	Usage      Usage
	TotalBytes int64
	AvailBytes int64
	Files      int64
	FilesFree  int64
}

type fileStat struct {
	dev    uint64
	ino    uint64
	nlink  uint64
	blocks int64
	mode   uint32
}

type walker interface {
	lstat(path string) (fileStat, error)
	// readDir calls fn for each child, in batches. It must not retain every
	// name from the directory at once.
	readDir(path string, fn func(name string) error) error
}

// direntBufSize is the getdents buffer. One batch is all that is live while
// the names in it are counted. Tests shrink it to force several reads.
var direntBufSize = 32 << 10

// Measure walks root. A timeout or I/O error fails the whole walk so the
// caller can keep the previous sample.
func Measure(ctx context.Context, root string) (Usage, error) {
	u, err := measure(ctx, osWalker{}, root)
	if err != nil {
		return Usage{}, fmt.Errorf("measure %s: %w", root, err)
	}
	return u, nil
}

// Inspect measures the directory and statfs the same path.
func Inspect(ctx context.Context, root string) (Report, error) {
	u, err := Measure(ctx, root)
	if err != nil {
		return Report{}, err
	}
	total, avail, files, free, err := statfs(root)
	if err != nil {
		return Report{}, fmt.Errorf("statfs %s: %w", root, err)
	}
	return Report{Usage: u, TotalBytes: total, AvailBytes: avail, Files: files, FilesFree: free}, nil
}

// ChildDirs lists immediate real directories. Symlinks are not included.
// This is the storage root (one entry per PVC), not the per-volume walk.
func ChildDirs(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && filepath.IsLocal(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func measure(ctx context.Context, w walker, root string) (Usage, error) {
	if err := ctx.Err(); err != nil {
		return Usage{}, err
	}
	rootStat, err := w.lstat(root)
	if err != nil {
		return Usage{}, err
	}
	// Only multi-linked inodes can be reached twice. Remembering every file
	// made a cache directory of a few hundred thousand entries exceed the
	// 128Mi pod limit.
	seen := make(map[uint64]struct{})
	var usage Usage

	var walk func(path string, st fileStat) error
	walk = func(path string, st fileStat) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		usage.Entries++
		cgroup.Note(usage.Entries)
		if st.dev != rootStat.dev {
			return nil
		}
		if st.nlink > 1 {
			if _, ok := seen[st.ino]; ok {
				return nil
			}
			seen[st.ino] = struct{}{}
		}
		usage.Bytes += blocksToBytes(st.blocks)
		usage.Inodes++

		if !isDir(st.mode) || isSymlink(st.mode) {
			return nil
		}
		err := w.readDir(path, func(name string) error {
			if !filepath.IsLocal(name) {
				return nil
			}
			child := filepath.Join(path, name)
			cst, err := w.lstat(child)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			return walk(child, cst)
		})
		if err != nil && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := walk(root, rootStat); err != nil {
		return Usage{}, err
	}
	return usage, nil
}

func blocksToBytes(blocks int64) int64 {
	if blocks <= 0 {
		return 0
	}
	if blocks > math.MaxInt64/512 {
		return math.MaxInt64
	}
	return blocks * 512
}

func isDir(mode uint32) bool {
	return mode&unix.S_IFMT == unix.S_IFDIR
}

func isSymlink(mode uint32) bool {
	return mode&unix.S_IFMT == unix.S_IFLNK
}

type osWalker struct{}

func (osWalker) lstat(path string) (fileStat, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return fileStat{}, err
	}
	return fileStat{
		dev:    uint64(st.Dev),
		ino:    st.Ino,
		nlink:  uint64(st.Nlink),
		blocks: st.Blocks,
		mode:   uint32(st.Mode),
	}, nil
}

func (osWalker) readDir(path string, fn func(name string) error) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	buf := make([]byte, direntBufSize)
	var names []string
	for {
		n, err := unix.ReadDirent(fd, buf)
		if n > 0 {
			names = names[:0]
			_, _, names = unix.ParseDirent(buf[:n], -1, names)
			for _, name := range names {
				if callErr := fn(name); callErr != nil {
					return callErr
				}
			}
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

func statfs(path string) (total, avail, files, filesFree int64, err error) {
	var st unix.Statfs_t
	if err = unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, 0, err
	}
	files, filesFree = clampUint(st.Files), clampUint(st.Ffree)
	if st.Bsize <= 0 {
		return 0, 0, files, filesFree, nil
	}
	bsize := uint64(st.Bsize)
	return mul(st.Blocks, bsize), mul(st.Bavail, bsize), files, filesFree, nil
}

func mul(a, b uint64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > uint64(math.MaxInt64)/b {
		return math.MaxInt64
	}
	return int64(a * b)
}

func clampUint(v uint64) int64 {
	if v > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(v)
}
