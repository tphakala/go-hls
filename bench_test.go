package hls

import (
	"strconv"
	"testing"
)

// BenchmarkSegmentLookup measures the window's linear scan at the shipping
// window size and at two larger ones, to establish empirically where the scan
// stops being free rather than arguing about it.
//
// It looks up the NEWEST segment, which is both the worst case for a scan that
// starts at the oldest and what a client keeping up with the live edge actually
// requests.
func BenchmarkSegmentLookup(b *testing.B) {
	for _, size := range []int{DefaultWindowSize, 60, 600} {
		b.Run("window="+strconv.Itoa(size), func(b *testing.B) {
			r := newSegmentWindow(size)
			for i := range size {
				r.push(&Segment{Seq: uint64(i)}) //nolint:gosec // loop bound is a small positive literal
			}
			newest := uint64(size - 1) //nolint:gosec // as above

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, ok := findSegment(r.retained(), newest); !ok {
					b.Fatal("newest segment must be retained")
				}
			}
		})
	}
}
