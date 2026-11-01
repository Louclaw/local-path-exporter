package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// DefaultCacheMaxBytes is the configured cap. A value at least this large
	// is raised to hardCacheMax so a directory-heavy volume is kept: dropping
	// it makes every later cycle a full scan.
	DefaultCacheMaxBytes int64 = 8 << 20
	// hardCacheMax is the adaptive ceiling. Two large volumes (about 1.6M
	// entries together) stay cached as directory records, still far under
	// the 128Mi cgroup limit. The Go heap is not what fills that limit.
	hardCacheMax int64 = 32 << 20
)

// errSurprise means a new hard link appeared in a directory that was
// rescanned while another directory may still count that inode as a
// normal file. The walk restarts from scratch so the inode is counted once.
var errSurprise = errors.New("hardlink cache invalidated")

// TreeCache remembers per-directory sizes so an unchanged directory is not
// read again. The copy in memory is also written to SCAN_CACHE_PATH when
// that path is set, so a restart does not repeat the full walk.
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
	root     *dirNode
	lastFull time.Time
	ready    bool
	cost     int64
}

// dirNode is one directory. Direct files are a single aggregate: there is
// no per-file record. Child names are directory names only, so a cache hit
// does not readdir the files.
type dirNode struct {
	name      string
	mtimeNS   int64
	selfIno   uint64
	selfNlink uint64
	selfBytes int64
	aggBytes  int64
	aggInodes int64
	nfiles    int64
	children  []*dirNode
	cost      int64
}

// NewTreeCache bounds the stored directories. maxBytes <= 0 disables storage.
func NewTreeCache(maxBytes int64) *TreeCache {
	return &TreeCache{
		maxBytes: maxBytes,
		vols:     map[string]*volumeMem{},
		quiet:    map[string]bool{},
	}
}

// costConsistent reports whether the running total still matches the records.
// Tests use it to catch a missed increment on insert, replace, or eviction.
func (c *TreeCache) costConsistent(root string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	if v == nil {
		return c.bytes >= 0
	}
	if !costsMatch(v.root) {
		return false
	}
	n := int64(volumeOverhead) + subtreeCost(v.root)
	if n != v.cost {
		return false
	}
	var all int64
	for _, vol := range c.vols {
		all += vol.cost
	}
	return all == c.bytes
}

func costsMatch(n *dirNode) bool {
	if n == nil {
		return true
	}
	if n.cost != nodeCost(n) {
		return false
	}
	for _, ch := range n.children {
		if !costsMatch(ch) {
			return false
		}
	}
	return true
}

func subtreeCost(n *dirNode) int64 {
	if n == nil {
		return 0
	}
	s := n.cost
	for _, ch := range n.children {
		s += subtreeCost(ch)
	}
	return s
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
	v.ready = v.root != nil || v.ready
}

func measureWith(ctx context.Context, root string, cache *TreeCache, full bool) (Usage, error) {
	// Incremental walks lstat every cached directory. On a large volume that
	// is enough slab to pin a 128Mi cgroup, so the throttle applies to every
	// walk, not only the full one.
	ctx = activateThrottle(ctx)
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
		ctx = activateThrottle(ctx)
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
	var walk func(path, name string, st fileStat, old *dirNode) (*dirNode, error)
	walk = func(path, name string, st fileStat, old *dirNode) (*dirNode, error) {
		if err := budgetErr(ctx); err != nil {
			return nil, err
		}
		usage.Entries++
		noteProgress(ctx, usage.Entries)
		if st.dev != rootStat.dev {
			return nil, nil
		}
		if !full && old != nil && old.mtimeNS == st.mtimeNS {
			applyCached(&usage, seen, old)
			for _, ch := range old.children {
				if ch == nil || !filepath.IsLocal(ch.name) {
					continue
				}
				child := filepath.Join(path, ch.name)
				cst, err := w.lstat(child)
				if err != nil {
					if os.IsNotExist(err) {
						return nil, errSurprise
					}
					return nil, err
				}
				if _, err := walk(child, ch.name, cst, ch); err != nil {
					return nil, err
				}
			}
			return old, nil
		}
		oldKids := map[string]*dirNode{}
		if old != nil {
			for _, ch := range old.children {
				if ch != nil {
					oldKids[ch.name] = ch
				}
			}
		}
		n := &dirNode{
			name:      name,
			mtimeNS:   st.mtimeNS,
			selfIno:   st.ino,
			selfNlink: st.nlink,
			selfBytes: blocksToBytes(st.blocks),
		}
		addNode(&usage, seen, st.ino, st.nlink, n.selfBytes)
		err := w.readDir(path, func(cname string) error {
			if err := budgetErr(ctx); err != nil {
				return err
			}
			if !filepath.IsLocal(cname) {
				return nil
			}
			child := filepath.Join(path, cname)
			cst, err := w.lstat(child)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if cst.dev != rootStat.dev {
				usage.Entries++
				noteProgress(ctx, usage.Entries)
				return nil
			}
			if isDir(cst.mode) && !isSymlink(cst.mode) {
				ch, err := walk(child, cname, cst, oldKids[cname])
				if err != nil {
					return err
				}
				if ch != nil {
					n.children = append(n.children, ch)
				}
				return nil
			}
			usage.Entries++
			noteProgress(ctx, usage.Entries)
			n.nfiles++
			nbytes := blocksToBytes(cst.blocks)
			if cst.nlink > 1 {
				// Inodes are not stored. A hard link in a directory that
				// changed cannot be attributed without scanning the volume.
				if !full && cache.ready(root) {
					return errSurprise
				}
				if _, ok := seen[cst.ino]; ok {
					return nil
				}
				n.aggBytes += nbytes
				n.aggInodes++
				addNode(&usage, seen, cst.ino, cst.nlink, nbytes)
				return nil
			}
			n.aggBytes += nbytes
			n.aggInodes++
			usage.Bytes += nbytes
			usage.Inodes++
			return nil
		})
		if err != nil {
			return nil, err
		}
		usage.Tracked += 1 + n.nfiles
		n.cost = nodeCost(n)
		return n, nil
	}
	node, err := walk(root, "", rootStat, cache.rootNode(root, full))
	if errors.Is(err, errSurprise) {
		return Usage{}, true, nil
	}
	if err != nil {
		return Usage{}, false, err
	}
	cache.commit(root, node)
	return usage, false, nil
}

func applyCached(u *Usage, seen map[uint64]struct{}, n *dirNode) {
	addNode(u, seen, n.selfIno, n.selfNlink, n.selfBytes)
	u.Bytes += n.aggBytes
	u.Inodes += n.aggInodes
	u.Tracked += 1 + n.nfiles
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
		c.bytes -= prev.cost
	}
	c.vols[root] = &volumeMem{lastFull: last, cost: volumeOverhead}
	c.bytes += volumeOverhead
}

func (c *TreeCache) finish(root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	if v == nil || c.maxBytes <= 0 {
		return
	}
	if v.root == nil || v.cost > c.ceiling() {
		c.bytes -= v.cost
		delete(c.vols, root)
		return
	}
	v.ready = true
}

func (c *TreeCache) ready(root string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	return v != nil && v.ready
}

// rootNode is the cached tree for an incremental walk.
// A full walk starts empty so every directory is rebuilt.
func (c *TreeCache) rootNode(root string, full bool) *dirNode {
	if c == nil || full {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vols[root]
	if v == nil || !v.ready {
		return nil
	}
	return v.root
}

// commit swaps in the tree built by one walk. Cost is the sum of the node
// records, computed once: a per-directory rescan of the whole map was quadratic.
func (c *TreeCache) commit(root string, n *dirNode) {
	if c == nil || c.maxBytes <= 0 || n == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.quiet[root] {
		return
	}
	v := c.ensure(root)
	cost := int64(volumeOverhead) + subtreeCost(n)
	if cost > c.ceiling() || c.bytes-v.cost+cost > c.ceiling() {
		c.bytes -= v.cost
		delete(c.vols, root)
		c.quiet[root] = true
		return
	}
	c.bytes += cost - v.cost
	v.root = n
	v.cost = cost
}

// ceiling is the configured cap. At or above the default it grows to
// hardCacheMax so a volume larger than 8MiB is not discarded.
func (c *TreeCache) ceiling() int64 {
	if c == nil || c.maxBytes <= 0 {
		return 0
	}
	if c.maxBytes < DefaultCacheMaxBytes {
		return c.maxBytes
	}
	if c.maxBytes >= hardCacheMax {
		return c.maxBytes
	}
	return hardCacheMax
}

func (c *TreeCache) ensure(root string) *volumeMem {
	v := c.vols[root]
	if v == nil {
		v = &volumeMem{cost: volumeOverhead}
		c.vols[root] = v
		c.bytes += volumeOverhead
	}
	return v
}

const volumeOverhead = 96

func nodeCost(n *dirNode) int64 {
	if n == nil {
		return 0
	}
	// 120 bytes covers the node, its pointer in the parent, and allocator
	// overhead. On a tree of 100k directories and 1M entries this tracks
	// the retained heap, so the 32MiB ceiling is a real-memory ceiling.
	return int64(len(n.name)) + 120
}
