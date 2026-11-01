package scan

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const cacheFileVersion uint32 = 2

var cacheMagic = [4]byte{'L', 'P', 'E', 'C'}

// Save writes the cache atomically (temp file, fsync, rename) in the same directory.
// An empty path is a no-op. The root filesystem stays read-only; the file lives on an emptyDir.
func (c *TreeCache) Save(path string) error {
	if c == nil || path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tmp, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err := writeCache(tmp, c); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	if dirf, err := os.Open(dir); err == nil {
		_ = dirf.Sync()
		dirf.Close()
	}
	return nil
}

// Load replaces the in-memory volumes with the file contents.
// A missing file yields an empty cache. A bad version or truncated file is rejected
// and leaves the receiver unchanged.
func (c *TreeCache) Load(path string) error {
	if c == nil || path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	vols, bytes, err := readCache(f)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vols = vols
	c.bytes = bytes
	return nil
}

func writeCache(w io.Writer, c *TreeCache) error {
	if _, err := w.Write(cacheMagic[:]); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, cacheFileVersion); err != nil {
		return err
	}
	roots := make([]string, 0, len(c.vols))
	for root := range c.vols {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	if err := binary.Write(w, binary.LittleEndian, uint32(len(roots))); err != nil {
		return err
	}
	for _, root := range roots {
		v := c.vols[root]
		if err := writeString(w, root); err != nil {
			return err
		}
		if err := binary.Write(w, binary.LittleEndian, timeToNano(v.lastFull)); err != nil {
			return err
		}
		ready := byte(0)
		if v.ready {
			ready = 1
		}
		if _, err := w.Write([]byte{ready}); err != nil {
			return err
		}
		if err := writeNode(w, v.root); err != nil {
			return err
		}
	}
	return nil
}

func writeNode(w io.Writer, n *dirNode) error {
	if n == nil {
		_, err := w.Write([]byte{0})
		return err
	}
	if _, err := w.Write([]byte{1}); err != nil {
		return err
	}
	if err := writeString(w, n.name); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, n.mtimeNS); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, n.selfIno); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, n.selfNlink); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, n.selfBytes); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, n.aggBytes); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, n.aggInodes); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, n.nfiles); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(n.children))); err != nil {
		return err
	}
	for _, ch := range n.children {
		if err := writeNode(w, ch); err != nil {
			return err
		}
	}
	return nil
}

func readCache(r io.Reader) (map[string]*volumeMem, int64, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, 0, err
	}
	if magic != cacheMagic {
		return nil, 0, errors.New("cache magic")
	}
	var version uint32
	if err := binary.Read(r, binary.LittleEndian, &version); err != nil {
		return nil, 0, err
	}
	if version != cacheFileVersion {
		return nil, 0, errors.New("cache version")
	}
	var nvol uint32
	if err := binary.Read(r, binary.LittleEndian, &nvol); err != nil {
		return nil, 0, err
	}
	if nvol > maxCacheRecords {
		return nil, 0, errors.New("cache too many volumes")
	}
	vols := make(map[string]*volumeMem, nvol)
	var total int64
	for i := uint32(0); i < nvol; i++ {
		root, err := readString(r)
		if err != nil {
			return nil, 0, err
		}
		var nano int64
		if err := binary.Read(r, binary.LittleEndian, &nano); err != nil {
			return nil, 0, err
		}
		var ready [1]byte
		if _, err := io.ReadFull(r, ready[:]); err != nil {
			return nil, 0, err
		}
		node, err := readNode(r)
		if err != nil {
			return nil, 0, err
		}
		v := &volumeMem{
			root:     node,
			lastFull: timeFromNano(nano),
			ready:    ready[0] == 1 && node != nil,
			cost:     volumeOverhead + subtreeCost(node),
		}
		vols[root] = v
		total += v.cost
	}
	return vols, total, nil
}

func readNode(r io.Reader) (*dirNode, error) {
	var flag [1]byte
	if _, err := io.ReadFull(r, flag[:]); err != nil {
		return nil, err
	}
	if flag[0] == 0 {
		return nil, nil
	}
	if flag[0] != 1 {
		return nil, errors.New("cache node")
	}
	name, err := readString(r)
	if err != nil {
		return nil, err
	}
	n := &dirNode{name: name}
	if err := binary.Read(r, binary.LittleEndian, &n.mtimeNS); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &n.selfIno); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &n.selfNlink); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &n.selfBytes); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &n.aggBytes); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &n.aggInodes); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &n.nfiles); err != nil {
		return nil, err
	}
	n.cost = nodeCost(n)
	var nchild uint32
	if err := binary.Read(r, binary.LittleEndian, &nchild); err != nil {
		return nil, err
	}
	if nchild > maxCacheRecords {
		return nil, errors.New("cache too many children")
	}
	n.children = make([]*dirNode, 0, nchild)
	for i := uint32(0); i < nchild; i++ {
		ch, err := readNode(r)
		if err != nil {
			return nil, err
		}
		if ch == nil {
			return nil, errors.New("cache missing child")
		}
		n.children = append(n.children, ch)
	}
	return n, nil
}

const maxCacheRecords = 1 << 20

func timeToNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func timeFromNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func writeString(w io.Writer, s string) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

func readString(r io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > 1<<20 {
		return "", errors.New("cache string too long")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
