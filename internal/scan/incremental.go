package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Louclaw/local-path-exporter/internal/cgroup"
)

const DefaultCacheMaxBytes int64 = 8 << 20

// errSurprise means a new hard link appeared in a directory that was
// rescanned while another directory may still count that inode as a
// normal file. The walk restarts from scratch so the inode is counted once.
var errSurprise = errors.New("hardlink cache invalidated")

// TreeCache remembers per-directory sizes so an unchanged directory is not
// read again. It is process memory only: the root filesystem is read-only.
// Directory mtime does not change when an existing file is rewritten, so
// those size changes wait for the next full walk.
type TreeCache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	vols     map[string]*volumeMem
	quiet    map[string]bool
}

type volumeMem struct {
	dirs     map[string]*dirMem
	known    map[uint64]struct{}
	lastFull time.Time
	ready    bool
	cost     int64
}

type dirMem struct {
	mtimeNS    int64
	selfIno    uint64
	selfNlink  uint64
	selfBytes  int64
	exclBytes  int64
	exclInodes int64
	nfiles     int64
	links      []linkMem
	children   []string
}

type linkMem struct {
	ino   uint64
	bytes int64
}

// NewTreeCache bounds the stored directories. maxBytes <= 0 disables storage.
func NewTreeCache(maxBytes int64) *TreeCache {
	return &TreeCache{
		maxBytes: maxBytes,
		vols:     map[string]*volumeMem{},
		quiet:    map[string]bool{},
	}
}

// Bytes is the estimated RAM held by directory records.
func (c *TreeCache) Bytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Due reports whether root needs a full walk.
func (c *TreeCache) Due(root string, every time.Duration, now time.Time) bool {
	if c == nil || every <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	if v == nil || v.lastFull.IsZero() {
		return true
	}
	return now.Sub(v.lastFull) >= every
}

// MarkFull records a successful full walk.
func (c *TreeCache) MarkFull(root string, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.ensure(root)
	v.lastFull = now
	v.ready = len(v.dirs) > 0 || v.ready
	c.recompute()
}

func measureWith(ctx context.Context, root string, cache *TreeCache, full bool) (Usage, error) {
	if cache == nil {
		return measure(ctx, osWalker{}, root)
	}
	cache.arm(root)
	if full {
		cache.reset(root)
	}
	u, surprise, err := walkCached(ctx, root, cache, full)
	if err != nil {
		return Usage{}, err
	}
	if surprise {
		cache.reset(root)
		u, _, err = walkCached(ctx, root, cache, true)
		if err != nil {
			return Usage{}, err
		}
	}
	cache.finish(root)
	return u, nil
}

func walkCached(ctx context.Context, root string, cache *TreeCache, full bool) (Usage, bool, error) {
	w := osWalker{}
	rootStat, err := w.lstat(root)
	if err != nil {
		return Usage{}, false, err
	}
	seen := map[uint64]struct{}{}
	var usage Usage
	var walk func(path, rel string, st fileStat) error
	walk = func(path, rel string, st fileStat) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		usage.Entries++
		cgroup.Note(usage.Entries)
		if st.dev != rootStat.dev {
			return nil
		}
		if ent, ok := cache.load(root, rel); ok && ent.mtimeNS == st.mtimeNS {
			applyCached(&usage, seen, ent)
			for _, name := range ent.children {
				if !filepath.IsLocal(name) {
					continue
				}
				child := filepath.Join(path, name)
				cst, err := w.lstat(child)
				if err != nil {
					if os.IsNotExist(err) {
						return errSurprise
					}
					return err
				}
				if err := walk(child, relJoin(rel, name), cst); err != nil {
					return err
				}
			}
			return nil
		}
		built := dirMem{
			mtimeNS:   st.mtimeNS,
			selfIno:   st.ino,
			selfNlink: st.nlink,
			selfBytes: blocksToBytes(st.blocks),
		}
		addNode(&usage, seen, st.ino, st.nlink, built.selfBytes)
		err := w.readDir(path, func(name string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
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
			if cst.dev != rootStat.dev {
				usage.Entries++
				cgroup.Note(usage.Entries)
				return nil
			}
			if isDir(cst.mode) && !isSymlink(cst.mode) {
				built.children = append(built.children, name)
				return walk(child, relJoin(rel, name), cst)
			}
			usage.Entries++
			cgroup.Note(usage.Entries)
			built.nfiles++
			nbytes := blocksToBytes(cst.blocks)
			if cst.nlink > 1 {
				if !full && cache.ready(root) && !cache.known(root, cst.ino) {
					return errSurprise
				}
				built.links = append(built.links, linkMem{ino: cst.ino, bytes: nbytes})
				addNode(&usage, seen, cst.ino, cst.nlink, nbytes)
				return nil
			}
			built.exclBytes += nbytes
			built.exclInodes++
			usage.Bytes += nbytes
			usage.Inodes++
			return nil
		})
		if err != nil {
			return err
		}
		usage.Tracked += 1 + built.nfiles
		cache.store(root, rel, built)
		return nil
	}
	err = walk(root, "", rootStat)
	if errors.Is(err, errSurprise) {
		return Usage{}, true, nil
	}
	if err != nil {
		return Usage{}, false, err
	}
	return usage, false, nil
}

func applyCached(u *Usage, seen map[uint64]struct{}, ent dirMem) {
	addNode(u, seen, ent.selfIno, ent.selfNlink, ent.selfBytes)
	u.Bytes += ent.exclBytes
	u.Inodes += ent.exclInodes
	for _, l := range ent.links {
		addNode(u, seen, l.ino, 2, l.bytes)
	}
	u.Tracked += 1 + ent.nfiles
}

func addNode(u *Usage, seen map[uint64]struct{}, ino, nlink uint64, bytes int64) {
	if nlink > 1 {
		if _, ok := seen[ino]; ok {
			return
		}
		seen[ino] = struct{}{}
	}
	u.Bytes += bytes
	u.Inodes++
}

func relJoin(rel, name string) string {
	if rel == "" {
		return name
	}
	return rel + "/" + name
}

func (c *TreeCache) arm(root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.quiet, root)
}

func (c *TreeCache) reset(root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.vols[root]
	var last time.Time
	if prev != nil {
		last = prev.lastFull
	}
	c.vols[root] = &volumeMem{
		dirs:     map[string]*dirMem{},
		known:    map[uint64]struct{}{},
		lastFull: last,
	}
	c.recompute()
}

func (c *TreeCache) finish(root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	if v == nil || c.maxBytes <= 0 {
		return
	}
	if v.cost > c.maxBytes {
		delete(c.vols, root)
		c.recompute()
		return
	}
	v.ready = true
}

func (c *TreeCache) load(root, rel string) (dirMem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	if v == nil || !v.ready {
		return dirMem{}, false
	}
	d := v.dirs[rel]
	if d == nil {
		return dirMem{}, false
	}
	cp := *d
	cp.links = append([]linkMem(nil), d.links...)
	cp.children = append([]string(nil), d.children...)
	return cp, true
}

func (c *TreeCache) ready(root string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	return v != nil && v.ready
}

func (c *TreeCache) known(root string, ino uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	if v == nil {
		return false
	}
	_, ok := v.known[ino]
	return ok
}

func (c *TreeCache) store(root, rel string, d dirMem) {
	if c == nil || c.maxBytes <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.quiet[root] {
		return
	}
	v := c.ensure(root)
	v.pruneChildren(rel, d.children)
	saved := d
	saved.links = append([]linkMem(nil), d.links...)
	saved.children = append([]string(nil), d.children...)
	v.dirs[rel] = &saved
	for _, l := range saved.links {
		v.known[l.ino] = struct{}{}
	}
	c.recompute()
	if c.bytes > c.maxBytes {
		delete(c.vols, root)
		c.quiet[root] = true
		c.recompute()
	}
}

func (c *TreeCache) ensure(root string) *volumeMem {
	v := c.vols[root]
	if v == nil {
		v = &volumeMem{dirs: map[string]*dirMem{}, known: map[uint64]struct{}{}}
		c.vols[root] = v
	}
	return v
}

func (v *volumeMem) pruneChildren(rel string, keep []string) {
	old := v.dirs[rel]
	if old == nil {
		return
	}
	still := map[string]struct{}{}
	for _, name := range keep {
		still[name] = struct{}{}
	}
	for _, name := range old.children {
		if _, ok := still[name]; ok {
			continue
		}
		v.deleteTree(relJoin(rel, name))
	}
}

func (v *volumeMem) deleteTree(rel string) {
	if rel == "" {
		return
	}
	prefix := rel + "/"
	for k := range v.dirs {
		if k == rel || strings.HasPrefix(k, prefix) {
			delete(v.dirs, k)
		}
	}
}

func (c *TreeCache) recompute() {
	var n int64
	for _, v := range c.vols {
		v.cost = v.size()
		n += v.cost
	}
	c.bytes = n
}

func (v *volumeMem) size() int64 {
	var n int64
	n += 96
	for rel, d := range v.dirs {
		n += int64(len(rel)) + 64
		n += int64(len(d.links)) * 16
		n += int64(len(d.children)) * 16
		for _, ch := range d.children {
			n += int64(len(ch))
		}
	}
	n += int64(len(v.known)) * 16
	return n
}
