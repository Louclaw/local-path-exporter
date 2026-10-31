package scan

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMeasureForeign0700 checks that DAC_READ_SEARCH, and not merely UID 0,
// is what allows a walk of a 0700 directory owned by another user.
// The privileged setup runs in a child so the test process keeps its capabilities.
func TestMeasureForeign0700(t *testing.T) {
	switch os.Getenv("LPE_DAC_CHILD") {
	case "supervisor":
		runDACSupervisor(t)
	case "cap":
		runDACProbe(t, true)
	case "nocap":
		runDACProbe(t, false)
	default:
		if os.Geteuid() == 0 {
			runDACSupervisor(t)
			return
		}
		reexecDACAsRoot(t)
	}
}

func reexecDACAsRoot(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sudo"); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("sudo is required to test DAC_READ_SEARCH")
		}
		t.Skip("root or passwordless sudo is required")
	}
	cmd := exec.Command("sudo", "-n", "env", "LPE_DAC_CHILD=supervisor", os.Args[0],
		"-test.run=^TestMeasureForeign0700$", "-test.count=1", "-test.v")
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := string(out)
		if os.Getenv("GITHUB_ACTIONS") != "true" && (strings.Contains(text, "password is required") || strings.Contains(text, "a password is required") || strings.Contains(text, "not allowed")) {
			t.Skipf("sudo -n unavailable: %s", text)
		}
		t.Fatalf("sudo re-exec: %v\n%s", err, text)
	}
}

func runDACSupervisor(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("supervisor must be root")
	}
	dir := t.TempDir()
	pvc := filepath.Join(dir, "pgdata")
	if err := os.Mkdir(pvc, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 8192)
	data := filepath.Join(pvc, "PG_VERSION")
	if err := os.WriteFile(data, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	const other = 65534
	if err := os.Chown(pvc, other, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(data, other, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pvc, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"nocap", "cap"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMeasureForeign0700$", "-test.count=1", "-test.v")
			cmd.Env = envReplace(os.Environ(), "LPE_DAC_CHILD", mode, "LPE_DAC_PATH", pvc)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}

func runDACProbe(t *testing.T, withCap bool) {
	path := os.Getenv("LPE_DAC_PATH")
	if path == "" {
		t.Fatal("LPE_DAC_PATH is empty")
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid == uint32(os.Geteuid()) || st.Mode&0o777 != 0o700 {
		t.Fatalf("fixture uid=%d mode=%o, want another uid and 0700", st.Uid, st.Mode&0o777)
	}
	caps := []uint{}
	if withCap {
		caps = []uint{unix.CAP_DAC_READ_SEARCH}
	}
	if err := setCaps(caps...); err != nil {
		t.Fatal(err)
	}
	usage, err := Measure(context.Background(), path)
	if withCap {
		if err != nil {
			t.Fatal(err)
		}
		if usage.Inodes < 2 || usage.Bytes <= 0 {
			t.Fatalf("usage = %+v", usage)
		}
		return
	}
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, os.ErrPermission) {
		t.Fatalf("without DAC_READ_SEARCH got %v", err)
	}
}

func setCaps(caps ...uint) error {
	var bits [2]uint32
	for _, c := range caps {
		bits[c/32] |= 1 << (c % 32)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{
		{Effective: bits[0], Permitted: bits[0]},
		{Effective: bits[1], Permitted: bits[1]},
	}
	return unix.Capset(&hdr, &data[0])
}

func envReplace(base []string, kv ...string) []string {
	if len(kv)%2 != 0 {
		panic("envReplace: odd arguments")
	}
	drop := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		drop[kv[i]] = kv[i+1]
	}
	out := make([]string, 0, len(base)+len(drop))
	for _, e := range base {
		key, _, ok := strings.Cut(e, "=")
		if ok {
			if _, replaced := drop[key]; replaced {
				continue
			}
		}
		out = append(out, e)
	}
	for k, v := range drop {
		out = append(out, k+"="+v)
	}
	return out
}
