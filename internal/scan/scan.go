// Package scan measures one PVC directory without crossing filesystems
// or following symlinks. Hard links are counted once per inode.
// Allocated size is st_blocks * 512.
package scan

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Usage is the allocated size and the number of directory entries counted.
type Usage struct {
	Bytes  int64
	Inodes int64
}

// Report adds filesystem-wide statfs figures for the volume's device.
type Report struct {
	Usage      Usage
	AvailBytes int64
	Files      int64
	FilesFree  int64
}

type fileStat struct {
	dev    uint64
	ino    uint64
	blocks int64
	mode   uint32
}

type walker interface {
	lstat(path string) (fileStat, error)
	list(path string) ([]string, error)
}

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
	avail, files, free, err := statfs(root)
	if err != nil {
		return Report{}, fmt.Errorf("statfs %s: %w", root, err)
	}
	return Report{Usage: u, AvailBytes: avail, Files: files, FilesFree: free}, nil
}

// ChildDirs lists immediate real directories. Symlinks are not included.
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
	rootStat, err := w.lstat(root)
	if err != nil {
		return Usage{}, err
	}
	rootDev := rootStat.dev
	seen := map[uint64]struct{}{}
	stack := []string{root}
	var usage Usage

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return Usage{}, err
		}
		path := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		st, err := w.lstat(path)
		if err != nil {
			if os.IsNotExist(err) && path != root {
				continue
			}
			return Usage{}, err
		}
		if st.dev != rootDev {
			continue
		}
		if _, ok := seen[st.ino]; ok {
			continue
		}
		seen[st.ino] = struct{}{}
		usage.Bytes += blocksToBytes(st.blocks)
		usage.Inodes++

		if !isDir(st.mode) || isSymlink(st.mode) {
			continue
		}
		names, err := w.list(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Usage{}, err
		}
		for _, name := range names {
			if !filepath.IsLocal(name) {
				continue
			}
			stack = append(stack, filepath.Join(path, name))
		}
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
	return fileStat{dev: uint64(st.Dev), ino: st.Ino, blocks: st.Blocks, mode: uint32(st.Mode)}, nil
}

func (osWalker) list(path string) ([]string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	return f.Readdirnames(-1)
}

func statfs(path string) (avail, files, filesFree int64, err error) {
	var st unix.Statfs_t
	if err = unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	if st.Bsize <= 0 {
		return 0, clampUint(st.Files), clampUint(st.Ffree), nil
	}
	return mul(uint64(st.Bavail), uint64(st.Bsize)), clampUint(st.Files), clampUint(st.Ffree), nil
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
