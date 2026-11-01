package scan

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIncrementalAddDeleteRenameAndFull(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, bytes.Repeat([]byte("x"), 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := NewTreeCache(DefaultCacheMaxBytes)
	ctx := context.Background()
	full, err := measureWith(ctx, dir, cache, true)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Measure(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if full.Bytes != plain.Bytes || full.Inodes != plain.Inodes {
		t.Fatalf("full cache walk %+v plain %+v", full, plain)
	}
	if cache.Bytes() <= 0 {
		t.Fatal("cache empty after full walk")
	}

	again, err := measureWith(ctx, dir, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Bytes != full.Bytes || again.Inodes != full.Inodes {
		t.Fatalf("unchanged = %+v, full = %+v", again, full)
	}
	if again.Entries >= full.Entries || again.Tracked < full.Entries {
		t.Fatalf("incremental entries=%d tracked=%d full entries=%d", again.Entries, again.Tracked, full.Entries)
	}

	// Directory mtime moves in nanoseconds. A create in the same tick is invisible.
	time.Sleep(20 * time.Millisecond)
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(b, bytes.Repeat([]byte("y"), 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	added, err := measureWith(ctx, dir, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	addedPlain, err := Measure(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if added.Bytes != addedPlain.Bytes || added.Inodes != addedPlain.Inodes || added.Bytes <= again.Bytes {
		t.Fatalf("add = %+v plain %+v previous %+v", added, addedPlain, again)
	}

	time.Sleep(20 * time.Millisecond)
	c := filepath.Join(dir, "c")
	if err := os.Rename(b, c); err != nil {
		t.Fatal(err)
	}
	renamed, err := measureWith(ctx, dir, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	renamedPlain, err := Measure(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Bytes != renamedPlain.Bytes || renamed.Inodes != renamedPlain.Inodes || renamed.Inodes != added.Inodes {
		t.Fatalf("rename = %+v plain %+v added inodes %d", renamed, renamedPlain, added.Inodes)
	}

	time.Sleep(20 * time.Millisecond)
	if err := os.Remove(c); err != nil {
		t.Fatal(err)
	}
	deleted, err := measureWith(ctx, dir, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	deletedPlain, err := Measure(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Bytes != deletedPlain.Bytes || deleted.Inodes != deletedPlain.Inodes || deleted.Inodes != renamed.Inodes-1 {
		t.Fatalf("delete = %+v plain %+v", deleted, deletedPlain)
	}

	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(a, bytes.Repeat([]byte("z"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err := measureWith(ctx, dir, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Bytes != deleted.Bytes {
		t.Fatalf("in-place rewrite visible without a full scan: %+v vs %+v", stale, deleted)
	}
	seen, err := measureWith(ctx, dir, cache, true)
	if err != nil {
		t.Fatal(err)
	}
	seenPlain, err := Measure(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Bytes != seenPlain.Bytes || seen.Bytes <= stale.Bytes {
		t.Fatalf("full scan = %+v plain %+v stale %+v", seen, seenPlain, stale)
	}
	if !cache.costConsistent(dir) {
		t.Fatalf("cache cost drifted to %d", cache.Bytes())
	}
}

func TestIncrementalHardlinkStaysSingle(t *testing.T) {
	dir := t.TempDir()
	d1 := filepath.Join(dir, "d1")
	d2 := filepath.Join(dir, "d2")
	if err := os.Mkdir(d1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d2, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(d1, "a")
	if err := os.WriteFile(orig, bytes.Repeat([]byte("x"), 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(orig, filepath.Join(d1, "b")); err != nil {
		t.Fatal(err)
	}
	cache := NewTreeCache(DefaultCacheMaxBytes)
	ctx := context.Background()
	if _, err := measureWith(ctx, dir, cache, true); err != nil {
		t.Fatal(err)
	}
	if _, err := measureWith(ctx, dir, cache, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	extra := filepath.Join(d2, "solo")
	if err := os.WriteFile(extra, bytes.Repeat([]byte("y"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sameUsage(ctx, dir, cache); err != nil {
		t.Fatal(err)
	}
	// A new link of a file that was counted as exclusive must not double-count.
	time.Sleep(20 * time.Millisecond)
	if err := os.Link(extra, filepath.Join(d1, "solo-link")); err != nil {
		t.Fatal(err)
	}
	if err := sameUsage(ctx, dir, cache); err != nil {
		t.Fatal(err)
	}
}

func TestCacheIsBounded(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		sub := filepath.Join(dir, name)
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := NewTreeCache(1)
	got, err := measureWith(context.Background(), dir, cache, true)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Measure(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bytes != plain.Bytes || got.Inodes != plain.Inodes {
		t.Fatalf("bounded cache changed the result: %+v vs %+v", got, plain)
	}
	if cache.Bytes() != 0 {
		t.Fatalf("cache bytes = %d, want 0 when over the cap", cache.Bytes())
	}
}

func TestFullScanManyDirectoriesWithinBound(t *testing.T) {
	const n = 50000
	root := t.TempDir()
	for i := 0; i < n; i++ {
		sub := filepath.Join(root, fmt.Sprintf("d%06d", i))
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		fd, err := os.OpenFile(filepath.Join(sub, "f"), os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if err := fd.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cache := NewTreeCache(DefaultCacheMaxBytes)
	start := time.Now()
	got, err := measureWith(context.Background(), root, cache, true)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("full scan of %d directories took %s, cache %d bytes", n, elapsed, cache.Bytes())
	// 40k directories took 12.9s when every insert rescanned the cache.
	// A linear walk of 50k is well under 5s; the CI runs this test.
	if elapsed > 5*time.Second {
		t.Fatalf("full scan took %s, want < 5s (quadratic cache cost)", elapsed)
	}
	if cache.Bytes() <= 0 || cache.Bytes() > DefaultCacheMaxBytes {
		t.Fatalf("cache bytes = %d, want within 1..%d", cache.Bytes(), DefaultCacheMaxBytes)
	}
	if !cache.costConsistent(root) {
		t.Fatalf("running cache cost %d drifted", cache.Bytes())
	}
	plain, err := Measure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bytes != plain.Bytes || got.Inodes != plain.Inodes {
		t.Fatalf("cached %+v plain %+v", got, plain)
	}
	again, err := measureWith(context.Background(), root, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	// Child directories are stated so their mtime can be checked. Their files are not.
	if again.Bytes != got.Bytes || again.Inodes != got.Inodes || again.Entries >= got.Entries {
		t.Fatalf("incremental %+v full %+v", again, got)
	}
}

func TestFullScanInterval(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := NewTreeCache(DefaultCacheMaxBytes)
	now := time.Unix(1_000, 0)
	if _, err := InspectWith(context.Background(), dir, cache, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if cache.Due(dir, time.Hour, now.Add(time.Minute)) {
		t.Fatal("full scan due too early")
	}
	if !cache.Due(dir, time.Hour, now.Add(time.Hour)) {
		t.Fatal("full scan not due at the interval")
	}
}

func TestAdaptiveCapKeepsDirectoryHeavyVolumes(t *testing.T) {
	c := NewTreeCache(DefaultCacheMaxBytes)
	// About 10MiB each: over the old 8MiB cap, under the 32MiB ceiling together.
	a := wideTree(20000, 200)
	b := wideTree(20000, 200)
	c.commit("/vol-a", a)
	c.commit("/vol-b", b)
	if c.Bytes() <= DefaultCacheMaxBytes {
		t.Fatalf("cache = %d, want more than %d", c.Bytes(), DefaultCacheMaxBytes)
	}
	if c.Bytes() > hardCacheMax {
		t.Fatalf("cache = %d, over hard cap %d", c.Bytes(), hardCacheMax)
	}
	if _, ok := c.vols["/vol-a"]; !ok {
		t.Fatal("first volume was dropped")
	}
	if _, ok := c.vols["/vol-b"]; !ok {
		t.Fatal("second volume was dropped")
	}
	if !c.costConsistent("/vol-a") || !c.costConsistent("/vol-b") {
		t.Fatal("cost drifted")
	}
	// Past the hard ceiling the volume is discarded instead of growing without bound.
	huge := wideTree(200000, 200)
	c.commit("/too-big", huge)
	if _, ok := c.vols["/too-big"]; ok {
		t.Fatalf("volume over the hard cap was kept, bytes=%d", c.Bytes())
	}
	if _, ok := c.vols["/vol-a"]; !ok {
		t.Fatal("dropping the oversized volume removed another one")
	}
}

func TestTwoVolumesStayIncrementalUntilFullScan(t *testing.T) {
	cache := NewTreeCache(DefaultCacheMaxBytes)
	now := time.Unix(1_700_000_000, 0)
	var fullEntries int64
	for _, name := range []string{"immich", "nullclaw"} {
		dir := t.TempDir()
		for i := 0; i < 4; i++ {
			sub := filepath.Join(dir, fmt.Sprintf("d%d", i))
			if err := os.Mkdir(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := InspectWith(context.Background(), dir, cache, now, 6*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		fullEntries += got.Usage.Entries
		again, err := InspectWith(context.Background(), dir, cache, now.Add(time.Hour), 6*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if again.Usage.Entries >= got.Usage.Entries || again.Usage.Bytes != got.Usage.Bytes {
			t.Fatalf("%s rescanned: full %+v again %+v", name, got.Usage, again.Usage)
		}
		if cache.Due(dir, 6*time.Hour, now.Add(5*time.Hour)) {
			t.Fatalf("%s full scan due before 6h", name)
		}
	}
	if fullEntries == 0 || cache.Bytes() == 0 {
		t.Fatalf("entries=%d cache=%d", fullEntries, cache.Bytes())
	}
}

func wideTree(n, nameLen int) *dirNode {
	root := &dirNode{name: "root"}
	root.cost = nodeCost(root)
	pad := strings.Repeat("n", nameLen)
	root.children = make([]*dirNode, n)
	for i := 0; i < n; i++ {
		ch := &dirNode{name: pad}
		ch.cost = nodeCost(ch)
		root.children[i] = ch
	}
	return root
}

func sameUsage(ctx context.Context, dir string, cache *TreeCache) error {
	got, err := measureWith(ctx, dir, cache, false)
	if err != nil {
		return err
	}
	plain, err := Measure(ctx, dir)
	if err != nil {
		return err
	}
	if got.Bytes != plain.Bytes || got.Inodes != plain.Inodes {
		return &mismatch{got: got, plain: plain}
	}
	return nil
}

type mismatch struct {
	got, plain Usage
}

func (m *mismatch) Error() string {
	return fmt.Sprintf("cache %+v full %+v", m.got, m.plain)
}
