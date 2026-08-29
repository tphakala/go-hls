package hls

import "testing"

// FuzzParseSegmentName drives the media-segment name parser with arbitrary
// input. The parser is the whole of path validation an HTTP handler applies to
// a segment request (see hlshttp), so it must never panic and must accept only
// the exact canonical spelling: whatever it accepts has to round-trip back to
// the same string through SegmentName, or two distinct request paths could name
// one segment and a client could mint unlimited cache keys for it.
func FuzzParseSegmentName(f *testing.F) {
	for _, seed := range []string{
		"", "segment0.m4s", "segment.m4s", "segment00.m4s", "segment01.m4s",
		"init.mp4", "segment-1.m4s", "segment+1.m4s", "SEGMENT0.m4s",
		"segment18446744073709551615.m4s", "segment18446744073709551616.m4s",
		"../segment0.m4s", "segment0.m4s\x00", "segment 0.m4s",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		seq, ok := ParseSegmentName(name)
		if ok && SegmentName(seq) != name {
			t.Fatalf("ParseSegmentName(%q) accepted seq %d but SegmentName reproduces %q", name, seq, SegmentName(seq))
		}
	})
}

// FuzzSegmentNameRoundTrip drives the inverse: every sequence number a stream
// can reach must produce a name the parser recovers exactly, so a segment the
// playlist advertises is always fetchable.
func FuzzSegmentNameRoundTrip(f *testing.F) {
	for _, seed := range []uint64{0, 1, 9, 10, 999, 1000, 1<<32 - 1, 1<<63 - 1, 1<<64 - 1} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seq uint64) {
		got, ok := ParseSegmentName(SegmentName(seq))
		if !ok || got != seq {
			t.Fatalf("SegmentName(%d) = %q did not round-trip: ParseSegmentName returned (%d, %v)", seq, SegmentName(seq), got, ok)
		}
	})
}
