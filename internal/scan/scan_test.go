package scan

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMeasureHardlinksAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("x"), 8192)
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "c"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	big := filepath.Join(outside, "big")
	if err := os.WriteFile(big, bytes.Repeat([]byte("y"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(big, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "dirlink")); err != nil {
		t.Fatal(err)
	}

	got, err := Measure(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	wantBytes := sumBlocks(
		dir,
		a,
		sub,
		filepath.Join(sub, "c"),
		filepath.Join(dir, "link"),
		filepath.Join(dir, "dirlink"),
	)
	if got.Bytes != wantBytes {
		t.Fatalf("bytes = %d, want %d (hardlink b must not be added)", got.Bytes, wantBytes)
	}
	// dir, a, sub, c, link, dirlink. b shares a's inode.
	if got.Inodes != 6 {
		t.Fatalf("inodes = %d, want 6", got.Inodes)
	}

	rep, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files <= 0 || rep.Usage.Bytes != got.Bytes {
		t.Fatalf("inspect: %+v", rep)
	}
}

func TestMeasureCancelled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Measure(ctx, dir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestMeasureFakeDeviceAndHardlink(t *testing.T) {
	const (
		reg = unix.S_IFREG
		dir = unix.S_IFDIR
		lnk = unix.S_IFLNK
	)
	fs := &fakeFS{nodes: map[string]fakeNode{
		"/pvc":        {dev: 1, ino: 1, blocks: 2, mode: dir, children: []string{"file", "hard", "link", "mnt"}},
		"/pvc/file":   {dev: 1, ino: 2, blocks: 4, mode: reg},
		"/pvc/hard":   {dev: 1, ino: 2, blocks: 4, mode: reg},
		"/pvc/link":   {dev: 1, ino: 3, blocks: 0, mode: lnk, children: []string{"secret"}},
		"/pvc/mnt":    {dev: 2, ino: 9, blocks: 100, mode: dir, children: []string{"hidden"}},
		"/pvc/secret": {dev: 1, ino: 8, blocks: 50, mode: reg},
		"/pvc/hidden": {dev: 2, ino: 10, blocks: 80, mode: reg},
	}}
	got, err := measure(context.Background(), fs, "/pvc")
	if err != nil {
		t.Fatal(err)
	}
	// root 2 blocks + file 4 blocks. hardlink skipped. symlink counted with 0. mount skipped.
	if got.Bytes != (2+4)*512 {
		t.Fatalf("bytes = %d", got.Bytes)
	}
	if got.Inodes != 3 {
		t.Fatalf("inodes = %d, want 3", got.Inodes)
	}
	for _, listed := range fs.listed {
		if listed == "/pvc/mnt" || listed == "/pvc/link" {
			t.Fatalf("must not list %s", listed)
		}
	}
}

func TestBlocksToBytesOverflow(t *testing.T) {
	if blocksToBytes(-1) != 0 {
		t.Fatal("negative")
	}
	if blocksToBytes(3) != 1536 {
		t.Fatal("scale")
	}
	if blocksToBytes(1<<62) <= 0 {
		t.Fatal("overflow should saturate")
	}
}

func sumBlocks(paths ...string) int64 {
	var total int64
	for _, p := range paths {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			panic(err)
		}
		total += st.Blocks * 512
	}
	return total
}

type fakeNode struct {
	dev, ino uint64
	blocks   int64
	mode     uint32
	children []string
}

type fakeFS struct {
	nodes  map[string]fakeNode
	listed []string
}

func (f *fakeFS) lstat(path string) (fileStat, error) {
	n, ok := f.nodes[path]
	if !ok {
		return fileStat{}, os.ErrNotExist
	}
	return fileStat{dev: n.dev, ino: n.ino, blocks: n.blocks, mode: n.mode}, nil
}

func (f *fakeFS) list(path string) ([]string, error) {
	f.listed = append(f.listed, path)
	n, ok := f.nodes[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]string(nil), n.children...), nil
}
