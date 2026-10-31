package scan

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
