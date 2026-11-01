package scan

import (
	"context"
	"os"
	"path/filepath"
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

func TestPersistMissingFileIsEmpty(t *testing.T) {
	cache := NewTreeCache(8 << 20)
	if err := cache.Load(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	if cache.Bytes() != 0 {
		t.Fatalf("bytes = %d", cache.Bytes())
	}
}
