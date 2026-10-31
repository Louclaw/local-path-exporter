package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestReadV2AndReclaim(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "memory.current", "120000000\n")
	write(t, dir, "memory.max", "134217728\n")
	write(t, dir, "memory.stat", "anon 5000000\nfile 40000000\nslab_reclaimable 50000000\nslab_unreclaimable 1000\nkernel 50001000\n")
	write(t, dir, "memory.reclaim", "")
	SetRootForTest(dir)
	t.Cleanup(func() { SetRootForTest("") })

	s := Read()
	if !s.OK || s.Current != 120000000 || s.Anon != 5000000 || s.File != 40000000 || s.SlabReclaimable != 50000000 || s.SlabUnreclaimable != 1000 || s.Kernel != 50001000 || s.Max != 134217728 {
		t.Fatalf("%+v", s)
	}
	if s.Heap == 0 {
		t.Fatal("heap not read")
	}
	Note(4096)
	got := strings.TrimSpace(read(t, dir, "memory.reclaim"))
	// current is above 5/8 of 128Mi; the walk trims down to 9/16.
	if got != "44502528" {
		t.Fatalf("reclaim wrote %q", got)
	}
}

func TestReclaimSkipsLooseLimit(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "memory.current", "2000000000\n")
	write(t, dir, "memory.max", "8000000000\n")
	write(t, dir, "memory.stat", "anon 1\nfile 1\nkernel 1\n")
	write(t, dir, "memory.reclaim", "")
	SetRootForTest(dir)
	t.Cleanup(func() { SetRootForTest("") })

	Release()
	if strings.TrimSpace(read(t, dir, "memory.reclaim")) != "" {
		t.Fatal("a multi-gigabyte limit must not be reclaimed")
	}
}

func TestReadV1(t *testing.T) {
	s := readV1(writeV1(t), Snapshot{})
	if !s.OK || s.Current != 42 || s.Anon != 7 || s.File != 9 || s.Kernel != 3 || s.Max != 128 {
		t.Fatalf("%+v", s)
	}
}

func TestParseProc(t *testing.T) {
	v2, v1 := parseProc("0::/kubepods/burstable/pod\n")
	if v2 != "/kubepods/burstable/pod" || v1 != "" {
		t.Fatalf("v2 %q v1 %q", v2, v1)
	}
	v2, v1 = parseProc("12:memory:/system.slice/pod\n")
	if v1 != "/system.slice/pod" || v2 != "" {
		t.Fatalf("v2 %q v1 %q", v2, v1)
	}
}

func TestCollectorV2(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "memory.current", "10\n")
	write(t, dir, "memory.max", "max\n")
	write(t, dir, "memory.stat", "anon 1\nfile 2\nslab_reclaimable 3\nslab_unreclaimable 4\nkernel 7\n")
	SetRootForTest(dir)
	t.Cleanup(func() { SetRootForTest("") })

	err := testutil.CollectAndCompare(NewCollector(), strings.NewReader(`
# HELP local_path_exporter_cgroup_anon_bytes Anonymous memory charged to the process cgroup.
# TYPE local_path_exporter_cgroup_anon_bytes gauge
local_path_exporter_cgroup_anon_bytes 1
# HELP local_path_exporter_cgroup_current_bytes memory.current of the process cgroup.
# TYPE local_path_exporter_cgroup_current_bytes gauge
local_path_exporter_cgroup_current_bytes 10
# HELP local_path_exporter_cgroup_file_bytes File page cache charged to the process cgroup.
# TYPE local_path_exporter_cgroup_file_bytes gauge
local_path_exporter_cgroup_file_bytes 2
# HELP local_path_exporter_cgroup_kernel_bytes Kernel memory charged to the process cgroup.
# TYPE local_path_exporter_cgroup_kernel_bytes gauge
local_path_exporter_cgroup_kernel_bytes 7
# HELP local_path_exporter_cgroup_slab_reclaimable_bytes Reclaimable kernel slab charged to the process cgroup (dentries and inodes).
# TYPE local_path_exporter_cgroup_slab_reclaimable_bytes gauge
local_path_exporter_cgroup_slab_reclaimable_bytes 3
# HELP local_path_exporter_cgroup_slab_unreclaimable_bytes Unreclaimable kernel slab charged to the process cgroup.
# TYPE local_path_exporter_cgroup_slab_unreclaimable_bytes gauge
local_path_exporter_cgroup_slab_unreclaimable_bytes 4
`), "local_path_exporter_cgroup_anon_bytes", "local_path_exporter_cgroup_current_bytes", "local_path_exporter_cgroup_file_bytes", "local_path_exporter_cgroup_kernel_bytes", "local_path_exporter_cgroup_slab_reclaimable_bytes", "local_path_exporter_cgroup_slab_unreclaimable_bytes")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(NewCollector()); err != nil {
		t.Fatal(err)
	}
}

func writeV1(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "memory.usage_in_bytes", "42\n")
	write(t, dir, "memory.limit_in_bytes", "128\n")
	write(t, dir, "memory.kmem.usage_in_bytes", "3\n")
	write(t, dir, "memory.stat", "rss 7\ncache 9\n")
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
