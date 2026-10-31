package stats

import "testing"

func TestAvailableBytes(t *testing.T) {
	const gi = int64(1) << 30
	cases := []struct {
		name           string
		capacity, used int64
		fsFree, want   int64
	}{
		{name: "under request, disk plenty", capacity: 2 * gi, used: 1 * gi, fsFree: 100 * gi, want: 1 * gi},
		{name: "over request", capacity: 2 * gi, used: 23 * gi, fsFree: 100 * gi, want: 0},
		{name: "disk tighter than request", capacity: 2 * gi, used: 1 * gi, fsFree: 100 << 20, want: 100 << 20},
		{name: "disk full", capacity: 2 * gi, used: 0, fsFree: 0, want: 0},
		{name: "unknown capacity", capacity: 0, used: 0, fsFree: 100, want: 0},
		{name: "equal", capacity: 100, used: 40, fsFree: 100, want: 60},
		{name: "negatives", capacity: -1, used: -5, fsFree: -3, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AvailableBytes(tc.capacity, tc.used, tc.fsFree)
			if got != tc.want {
				t.Fatalf("AvailableBytes(%d,%d,%d)=%d, want %d", tc.capacity, tc.used, tc.fsFree, got, tc.want)
			}
		})
	}
}
