package scan

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMeasureManyBatches(t *testing.T) {
	prev := direntBufSize
	direntBufSize = 256
	t.Cleanup(func() { direntBufSize = prev })

	dir := makeCacheTree(t, 2_000, 100)
	got, err := Measure(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := oracleUsage(t, dir)
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// TestMeasureFlatTreeMemoryBounded builds a cache-shaped tree of 100k files
// plus hard links and checks the walk from a fresh process. v0.1.0 kept every
// directory entry and every inode, about 35MiB for this tree, on top of the
// exporter's own footprint, which is what pushed a 128Mi pod into OOMKilled.
func TestMeasureFlatTreeMemoryBounded(t *testing.T) {
	if os.Getenv("LPE_MEM_CHILD") == "1" {
		runMemoryChild(t)
		return
	}

	small := makeCacheTree(t, 2_000, 50)
	large := makeCacheTree(t, 100_000, 2_000)
	for _, dir := range []string{small, large} {
		got, err := Measure(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		want := oracleUsage(t, dir)
		if got != want {
			t.Fatalf("%s usage = %+v, want %+v", dir, got, want)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMeasureFlatTreeMemoryBounded$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"LPE_MEM_CHILD=1",
		"LPE_MEM_SMALL="+small,
		"LPE_MEM_LARGE="+large,
	)
	out, err := cmd.CombinedOutput()
	t.Logf("child:\n%s", out)
	if err != nil {
		t.Fatalf("memory child: %v", err)
	}
	smallPeak, largePeak := parsePeaks(t, string(out))
	if largePeak > flatTreeHeapLimit {
		t.Fatalf("100k peak heap %d bytes exceeds %d", largePeak, flatTreeHeapLimit)
	}
	// 50x more files must not scale the live heap. A fixed slack covers GC noise.
	if largePeak > smallPeak+flatTreeHeapSlack {
		t.Fatalf("peak grew with file count: 2k=%d 100k=%d slack=%d", smallPeak, largePeak, flatTreeHeapSlack)
	}
}

func runMemoryChild(t *testing.T) {
	small := os.Getenv("LPE_MEM_SMALL")
	large := os.Getenv("LPE_MEM_LARGE")
	if small == "" || large == "" {
		t.Fatal("missing tree paths")
	}
	smallPeak, smallRSS := peakDuring(t, small)
	largePeak, largeRSS := peakDuring(t, large)
	again, _ := peakDuring(t, large)
	runtime.GC()
	debug.FreeOSMemory()
	var released runtime.MemStats
	runtime.ReadMemStats(&released)
	fmt.Printf("LPE_MEM small=%d large=%d again=%d smallRSS=%d largeRSS=%d released=%d\n",
		smallPeak, largePeak, again, smallRSS, largeRSS, released.HeapAlloc)
	if again > largePeak+flatTreeHeapSlack {
		t.Fatalf("second scan grew: first=%d second=%d", largePeak, again)
	}
}

func peakDuring(t *testing.T, root string) (heapDelta, rssDelta uint64) {
	t.Helper()
	runtime.GC()
	debug.FreeOSMemory()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	baseRSS := readStatusKB("VmRSS:")
	var peak atomic.Uint64
	var peakRSS atomic.Uint64
	peak.Store(base.HeapAlloc)
	peakRSS.Store(baseRSS)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				runtime.ReadMemStats(&m)
				storeMax(&peak, m.HeapAlloc)
				storeMax(&peakRSS, readStatusKB("VmRSS:"))
				return
			case <-tick.C:
				runtime.ReadMemStats(&m)
				storeMax(&peak, m.HeapAlloc)
				storeMax(&peakRSS, readStatusKB("VmRSS:"))
			}
		}
	}()
	if _, err := Measure(context.Background(), root); err != nil {
		close(stop)
		<-done
		t.Fatal(err)
	}
	close(stop)
	<-done
	heap := peak.Load()
	rss := peakRSS.Load()
	if heap > base.HeapAlloc {
		heapDelta = heap - base.HeapAlloc
	}
	if rss > baseRSS {
		rssDelta = (rss - baseRSS) * 1024
	}
	return heapDelta, rssDelta
}

func storeMax(dst *atomic.Uint64, v uint64) {
	for {
		old := dst.Load()
		if v <= old || dst.CompareAndSwap(old, v) {
			return
		}
	}
}

func parsePeaks(t *testing.T, out string) (small, large uint64) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "LPE_MEM ") {
			continue
		}
		fields := strings.Fields(line)
		for _, f := range fields[1:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			switch k {
			case "small":
				small = n
			case "large":
				large = n
			}
		}
		return small, large
	}
	t.Fatalf("no LPE_MEM line in:\n%s", out)
	return 0, 0
}

func makeCacheTree(tb testing.TB, files, links int) string {
	tb.Helper()
	if links > files {
		tb.Fatalf("links %d > files %d", links, files)
	}
	dir := filepath.Join(tb.TempDir(), "var/lib/rancher/k3s/storage/pvc-3f1a0c2e-7b4d-4e1a-9c88-0a1b2c3d4e5f_immich_immich/thumbs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("%040x.webp", i)
		fd, err := unix.Open(filepath.Join(dir, name), unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o644)
		if err != nil {
			tb.Fatal(err)
		}
		if err := unix.Close(fd); err != nil {
			tb.Fatal(err)
		}
	}
	linkDir := filepath.Join(dir, "hardlinks")
	if err := os.Mkdir(linkDir, 0o755); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < links; i++ {
		src := filepath.Join(dir, fmt.Sprintf("%040x.webp", i))
		dst := filepath.Join(linkDir, fmt.Sprintf("%040x.webp", i))
		if err := os.Link(src, dst); err != nil {
			tb.Fatal(err)
		}
	}
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		fd, err := unix.Open(filepath.Join(nested, fmt.Sprintf("n%040x.webp", i)), unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o644)
		if err != nil {
			tb.Fatal(err)
		}
		if err := unix.Close(fd); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

func oracleUsage(t *testing.T, root string) Usage {
	t.Helper()
	var rootStat unix.Stat_t
	if err := unix.Lstat(root, &rootStat); err != nil {
		t.Fatal(err)
	}
	seen := map[uint64]struct{}{}
	var usage Usage
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		usage.Entries++
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		if st.Dev != rootStat.Dev {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := seen[st.Ino]; ok {
			return nil
		}
		seen[st.Ino] = struct{}{}
		usage.Bytes += blocksToBytes(st.Blocks)
		usage.Inodes++
		if d.Type()&os.ModeSymlink != 0 && d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func readStatusKB(prefix string) uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, prefix) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}
			n, _ := strconv.ParseUint(fields[1], 10, 64)
			return n
		}
	}
	return 0
}

func BenchmarkMeasure100kFiles(b *testing.B) {
	dir := makeCacheTree(b, 100_000, 2_000)
	if _, err := Measure(context.Background(), dir); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Measure(context.Background(), dir); err != nil {
			b.Fatal(err)
		}
	}
}
