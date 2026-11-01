package scan

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestPersistRoundTripSkipsFullWalk(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewTreeCache(8 << 20)
	now := time.Now()
	full, err := InspectWith(context.Background(), root, cache, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cache.v1")
	if err := cache.Save(path); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".cache-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}

	next := NewTreeCache(8 << 20)
	if err := next.Load(path); err != nil {
		t.Fatal(err)
	}
	if !next.costConsistent(root) {
		t.Fatal("loaded cost does not match the records")
	}
	again, err := InspectWith(context.Background(), root, next, now.Add(time.Minute), 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if again.Usage.Entries >= full.Usage.Entries {
		t.Fatalf("restart walked the tree again: full=%d again=%d", full.Usage.Entries, again.Usage.Entries)
	}
	if again.Usage.Bytes != full.Usage.Bytes || again.Usage.Inodes != full.Usage.Inodes {
		t.Fatalf("usage changed across reload: full=%+v again=%+v", full.Usage, again.Usage)
	}
}

func TestPersistRejectsBadVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := NewTreeCache(8 << 20)
	if _, err := InspectWith(context.Background(), root, cache, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cache.v1")
	if err := cache.Save(path); err != nil {
		t.Fatal(err)
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	buf[4] = 99
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	kept := cache.Bytes()
	if err := cache.Load(path); err == nil {
		t.Fatal("expected version error")
	}
	if cache.Bytes() != kept {
		t.Fatalf("failed load changed the cache: %d -> %d", kept, cache.Bytes())
	}
}

func TestLoadLargeCacheHeapIsBounded(t *testing.T) {
	src := NewTreeCache(DefaultCacheMaxBytes)
	src.commit("/vol", wideTree(30000, 160))
	src.finish("/vol")
	if src.Bytes() < 4<<20 {
		t.Fatalf("fixture cache = %d, want a multi-megabyte tree", src.Bytes())
	}
	path := filepath.Join(t.TempDir(), "cache.v1")
	if err := src.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		var m runtime.MemStats
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				runtime.ReadMemStats(&m)
				for {
					old := peak.Load()
					if m.HeapAlloc <= old || peak.CompareAndSwap(old, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	dst := NewTreeCache(DefaultCacheMaxBytes)
	if err := dst.Load(path); err != nil {
		t.Fatal(err)
	}
	close(stop)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	retained := after.HeapAlloc - before.HeapAlloc
	// The decoded tree is one copy. A second full copy of the file, or of
	// every string, would push the retained heap well past the estimate.
	if retained > uint64(dst.Bytes())+8<<20 {
		t.Fatalf("retained heap %d, cache %d, file %d", retained, dst.Bytes(), st.Size())
	}
	got := peak.Load()
	allow := uint64(dst.Bytes())*2 + 8<<20
	if got > before.HeapAlloc+allow {
		t.Fatalf("peak heap %d, base %d, allowance %d, file %d", got, before.HeapAlloc, allow, st.Size())
	}
	t.Logf("file=%d cache=%d retained=%d peak_over_base=%d", st.Size(), dst.Bytes(), retained, got-before.HeapAlloc)

	again, err := InspectWith(context.Background(), t.TempDir(), dst, time.Now(), 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if again.Usage.Entries < 1 {
		t.Fatal("loaded cache broke scanning")
	}
}

func TestLoadRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.v1")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(hardCacheMax*2 + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cache := NewTreeCache(DefaultCacheMaxBytes)
	if err := cache.Load(path); err == nil {
		t.Fatal("expected oversized cache to be rejected")
	}
	if cache.Bytes() != 0 {
		t.Fatalf("rejected load changed the cache: %d", cache.Bytes())
	}
}

func TestPersistMissingFileIsEmpty(t *testing.T) {
	cache := NewTreeCache(8 << 20)
	if err := cache.Load(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	if cache.Bytes() != 0 {
		t.Fatalf("bytes = %d", cache.Bytes())
	}
}
