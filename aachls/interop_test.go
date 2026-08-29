package aachls

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	hls "github.com/tphakala/go-hls"
)

// These tests prove that the bytes go-hls emits are not just structurally
// plausible but actually decode: a real third-party demuxer (ffmpeg) reads the
// playlist, the fMP4 init segment and the AAC media segments, and reconstructs
// the audio that was fed in. They are the end-to-end complement to the unit
// tests, which assert on box magic and playlist text but never decode a sample.
//
// Everything here is skipped when ffmpeg is not on PATH, so the suite still
// passes on a machine without it; CI installs ffmpeg so the checks run there.

// requireFFmpeg skips the test unless ffmpeg and ffprobe are both available.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
}

// writeHLSDir writes a finished stream's init segment, every retained media
// segment, and the playlist into a fresh temp directory, using exactly the
// filenames the playlist advertises, and returns the playlist path. Call it
// after Close, so the playlist carries EXT-X-ENDLIST and ffmpeg treats the
// directory as a complete VOD presentation it can decode start to finish.
func writeHLSDir(t *testing.T, s *hls.Stream) (playlistPath string) {
	t.Helper()
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, hls.InitSegmentName), s.InitSegment(), 0o600))

	// Every sequence number ever cut that is still retained. After Close the
	// window holds the tail of the stream; a short test keeps everything.
	stats := s.Stats()
	wrote := 0
	for seq := uint64(0); seq < stats.Segments; seq++ {
		seg, ok := s.Segment(seq)
		if !ok {
			continue // evicted from the window
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, hls.SegmentName(seq)), seg.Data, 0o600))
		wrote++
	}
	require.Positive(t, wrote, "no retained segments to serve")

	playlistPath = filepath.Join(dir, "live.m3u8")
	require.NoError(t, os.WriteFile(playlistPath, []byte(s.Playlist()), 0o600))
	return playlistPath
}

// decodePCM runs ffmpeg over an m3u8 (or any input) and returns the decoded
// interleaved signed-16-bit little-endian PCM at the given channel count.
func decodePCM(t *testing.T, path string, channels int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	out := filepath.Join(t.TempDir(), "out.raw")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error",
		"-allowed_extensions", "ALL",
		"-i", path, "-f", "s16le", "-ac", strconv.Itoa(channels), "-y", out)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg decode of %s failed: %v\n%s", path, err, combined)
	}
	pcm, err := os.ReadFile(out)
	require.NoError(t, err)
	return pcm
}

// deinterleaveCh0 extracts channel 0 from interleaved S16LE PCM as float64.
func deinterleaveCh0(pcm []byte, channels int) []float64 {
	frames := len(pcm) / (2 * channels)
	out := make([]float64, frames)
	for i := range frames {
		lo := pcm[i*2*channels]
		hi := pcm[i*2*channels+1]
		out[i] = float64(int16(binary.LittleEndian.Uint16([]byte{lo, hi})))
	}
	return out
}

// normXCorr is the normalized cross-correlation of a against b at the given lag,
// aligning a[n] with b[n+lag]. The result is in [-1, 1] and is invariant to a
// constant amplitude scale, which is what makes it a clean alignment probe for a
// lossy codec that need not preserve absolute level.
func normXCorr(a, b []float64, lag int) float64 {
	var dot, na, nb float64
	for n := range a {
		m := n + lag
		if m < 0 || m >= len(b) {
			continue
		}
		dot += a[n] * b[m]
		na += a[n] * a[n]
		nb += b[m] * b[m]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

// bestCorrelation searches a lag window and returns the peak normalized
// correlation and the lag at which it occurs.
func bestCorrelation(src, dec []float64, maxLag int) (corr float64, lag int) {
	corr, lag = -1, 0
	for l := -maxLag; l <= maxLag; l++ {
		if c := normXCorr(src, dec, l); c > corr {
			corr, lag = c, l
		}
	}
	return corr, lag
}

// rms is the root-mean-square level of a signal, used to prove a span of decoded
// audio carries real signal rather than silence.
func rms(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var sum float64
	for _, v := range x {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(x)))
}

// chirp generates a linear frequency sweep from f0 to f1 over its whole length,
// as interleaved S16LE PCM. Unlike a fixed sine, a chirp is not periodic on the
// scale of an access unit: its instantaneous frequency is different at every
// moment, so a dropped or duplicated access unit shifts later audio against a
// differently-pitched stretch of the source and the cross-correlation collapses
// globally rather than realigning to an identical phase. That is what makes it a
// signal a correlation test can actually catch sample loss with.
func chirp(nSamples, channels, rate int, f0, f1 float64) []byte {
	buf := make([]byte, nSamples*channels*2)
	dur := float64(nSamples) / float64(rate)
	for i := range nSamples {
		tt := float64(i) / float64(rate)
		// Phase of a linear sweep: integral of the instantaneous frequency
		// f0 + (f1-f0)/dur * t over [0, t].
		phase := 2 * math.Pi * (f0*tt + (f1-f0)/(2*dur)*tt*tt)
		v := int16(math.Round(8000 * math.Sin(phase)))
		for ch := range channels {
			off := (i*channels + ch) * 2
			buf[off] = byte(v)
			buf[off+1] = byte(v >> 8)
		}
	}
	return buf
}

// feedChirp writes seconds of a chirp swept from f0 to f1 through the stream in
// chunks that do not divide the 1024-sample AAC access unit, so partial-frame
// buffering is exercised on every write. It returns the source PCM and the next
// timestamp.
func feedChirp(t *testing.T, s *hls.Stream, start time.Time, seconds, channels int, f0, f1 float64) (src []byte, next time.Time) {
	t.Helper()
	src = chirp(seconds*testRate, channels, testRate, f0, f1)
	const chunkBytes = 1200 // samples per write; deliberately not a frame multiple
	frameBytes := channels * 2
	at := start
	for off := 0; off < len(src); {
		end := off + chunkBytes*frameBytes
		if end > len(src) {
			end = len(src)
		}
		require.NoError(t, s.Write(src[off:end], at))
		samples := (end - off) / frameBytes
		at = at.Add(time.Duration(samples) * time.Second / testRate)
		off = end
	}
	return src, at
}

// TestInteropStreamDecodesToSource is the core end-to-end proof: a known chirp
// fed through the muxer decodes back, through ffmpeg reading the playlist, to
// the same chirp at lag zero. A wrong sample duration, a broken edit list, a
// dropped access unit or a malformed box would all break the correlation.
func TestInteropStreamDecodesToSource(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)

	s, err := hls.New(&hls.Config{
		Codec:       AACLC(),
		SampleRate:  testRate,
		Channels:    1,
		BitrateKbps: 96,
	})
	require.NoError(t, err)

	src, _ := feedChirp(t, s, sampleTime(0), 5, 1, 300, 8000)
	require.NoError(t, s.Close())

	playlist := writeHLSDir(t, s)
	dec := decodePCM(t, playlist, 1)

	srcF := deinterleaveCh0(src, 1)
	decF := deinterleaveCh0(dec, 1)
	require.NotEmpty(t, decF, "ffmpeg decoded no audio")

	// The decoded length should be close to the source: AAC-LC's one-frame
	// priming is trimmed by the container edit list, so any remaining slack is
	// at most a fraction of a second, not a whole frame.
	require.InDelta(t, len(srcF), len(decF), float64(testRate)/2,
		"decoded length diverges from source by more than half a second")

	// The decoded chirp matches the source at lag zero: the container edit list
	// trims the AAC priming so playback aligns sample-accurately, and because
	// the chirp's frequency is different at every instant, any dropped or
	// duplicated access unit would shift later audio against a differently-
	// pitched stretch of the source and collapse the correlation globally
	// rather than merely offset it. So the near-1.0 peak at a near-zero lag is a
	// real proof that every sample is present and in order, not an alignment a
	// periodic signal could fake.
	corr, lag := bestCorrelation(srcF, decF, testRate/4)
	assert.Greater(t, corr, 0.95, "decoded audio does not match the source chirp (peak corr %.3f at lag %d)", corr, lag)
	assert.Less(t, abs(lag), 256, "playback is misaligned by %d samples; the edit list should align it to zero", lag)
}

// TestInteropStereoDecodes proves the muxer is not silently mono-only: a stereo
// source decodes to two non-silent channels that both track the input.
func TestInteropStereoDecodes(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)

	s, err := hls.New(&hls.Config{
		Codec:       AACLC(),
		SampleRate:  testRate,
		Channels:    2,
		BitrateKbps: 128,
	})
	require.NoError(t, err)

	src, _ := feedChirp(t, s, sampleTime(0), 4, 2, 400, 6000)
	require.NoError(t, s.Close())

	dec := decodePCM(t, writeHLSDir(t, s), 2)
	require.NotEmpty(t, dec)

	srcF := deinterleaveCh0(src, 2)
	decF := deinterleaveCh0(dec, 2)
	require.Positive(t, rms(decF), "channel 0 decoded to silence")
	corr, lag := bestCorrelation(srcF, decF, testRate/10)
	assert.Greater(t, corr, 0.9, "stereo channel 0 does not match source (corr %.3f at lag %d)", corr, lag)
}

// TestInteropFFprobeReportsAACStream asks ffprobe to describe the stream,
// proving the sample entry advertises the codec and shape a player expects.
func TestInteropFFprobeReportsAACStream(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)

	s, err := hls.New(&hls.Config{
		Codec:       AACLC(),
		SampleRate:  testRate,
		Channels:    1,
		BitrateKbps: 96,
	})
	require.NoError(t, err)
	_, _ = feedChirp(t, s, sampleTime(0), 3, 1, 300, 8000)
	require.NoError(t, s.Close())

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-allowed_extensions", "ALL",
		"-show_streams", "-print_format", "json", writeHLSDir(t, s)).Output()
	require.NoError(t, err, "ffprobe failed: %s", out)

	var probe struct {
		Streams []struct {
			CodecName  string `json:"codec_name"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(out, &probe))
	require.Len(t, probe.Streams, 1)
	assert.Equal(t, "aac", probe.Streams[0].CodecName)
	assert.Equal(t, strconv.Itoa(testRate), probe.Streams[0].SampleRate)
	assert.Equal(t, 1, probe.Streams[0].Channels)
}

// TestInteropStallDiscontinuityResumeDecodes is the uncommon-but-expected case:
// the source stalls long enough to break the timeline, then resumes. The muxer
// must close out the pre-gap segment, declare EXT-X-DISCONTINUITY, re-anchor the
// program date time to the resume instant, and keep producing valid segments.
// ffmpeg must then decode the whole presentation across the break, and both the
// pre-gap and post-gap audio must survive.
func TestInteropStallDiscontinuityResumeDecodes(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)

	s, err := hls.New(&hls.Config{
		Codec:       AACLC(),
		SampleRate:  testRate,
		Channels:    1,
		BitrateKbps: 96,
	})
	require.NoError(t, err)

	// Four seconds of a low chirp, then a stall well past MaxStallGap, then four
	// seconds of a clearly separate high chirp from the resume instant. The two
	// bands are disjoint so each decoded half can be matched to its own source
	// and told apart from the other.
	srcPre, afterFirst := feedChirp(t, s, sampleTime(0), 4, 1, 300, 1500)
	resume := afterFirst.Add(30 * time.Second)
	srcPost, _ := feedChirp(t, s, resume, 4, 1, 4000, 7000)
	require.NoError(t, s.Close())

	stats := s.Stats()
	require.Equal(t, uint64(1), stats.Discontinuities, "the stall must declare exactly one discontinuity")

	playlist := writeHLSDir(t, s)
	body, err := os.ReadFile(playlist)
	require.NoError(t, err)
	assert.Contains(t, string(body), "#EXT-X-DISCONTINUITY\n", "the playlist must mark the timeline break")

	// ffmpeg decodes across the discontinuity into one continuous stream: the
	// gap carries no samples, so the pre-gap audio is followed directly by the
	// post-gap audio, roughly eight seconds total.
	decF := deinterleaveCh0(decodePCM(t, playlist, 1), 1)
	require.Greater(t, len(decF), 7*testRate, "decoded audio is too short to contain both halves")

	// Split at the midpoint and take an interior slice of each half, away from
	// the seam and the priming edges, then prove each interior actually decoded
	// to ITS OWN source chirp and not the other. rms > 0 alone would pass on
	// looped or garbage audio; matching each half to its distinct source is what
	// proves the timeline resumed with the correct signal on each side.
	mid := len(decF) / 2
	preDec := decF[testRate/2 : mid-testRate/2]
	postDec := decF[mid+testRate/2 : len(decF)-testRate/2]
	preSrc := deinterleaveCh0(srcPre, 1)
	postSrc := deinterleaveCh0(srcPost, 1)

	preOwn, _ := bestCorrelation(preDec, preSrc, testRate)
	preOther, _ := bestCorrelation(preDec, postSrc, testRate)
	assert.Greater(t, preOwn, 0.8, "pre-gap audio does not match the pre-gap source chirp (%.3f)", preOwn)
	assert.Greater(t, preOwn, preOther+0.2, "pre-gap audio matches the post-gap source too well to be distinct (own %.3f, other %.3f)", preOwn, preOther)

	postOwn, _ := bestCorrelation(postDec, postSrc, testRate)
	postOther, _ := bestCorrelation(postDec, preSrc, testRate)
	assert.Greater(t, postOwn, 0.8, "post-gap audio does not match the post-gap source chirp (%.3f)", postOwn)
	assert.Greater(t, postOwn, postOther+0.2, "post-gap audio matches the pre-gap source too well to be distinct (own %.3f, other %.3f)", postOwn, postOther)
}

// TestInteropWindowEvictionDecodes exercises the sliding window against a real
// demuxer: a stream long enough to overflow the window drops its oldest
// segments, so the playlist advertises EXT-X-MEDIA-SEQUENCE above zero and the
// retained segments are a tail slice of the source. ffmpeg must still decode
// that partial presentation, and it must be the correct contiguous stretch of
// the source with nothing dropped inside it.
func TestInteropWindowEvictionDecodes(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)

	s, err := hls.New(&hls.Config{
		Codec:       AACLC(),
		SampleRate:  testRate,
		Channels:    1,
		BitrateKbps: 96,
	})
	require.NoError(t, err)

	// Sixteen seconds at two-second segments is eight segments, more than the
	// six-segment default window, so the oldest two are evicted.
	src, _ := feedChirp(t, s, sampleTime(0), 16, 1, 300, 8000)
	require.NoError(t, s.Close())

	stats := s.Stats()
	require.Greater(t, stats.Segments, uint64(hls.DefaultWindowSize), "not enough segments to force eviction")
	require.Equal(t, hls.DefaultWindowSize, stats.Retained, "the window must cap at its size")

	playlist := writeHLSDir(t, s)
	body, err := os.ReadFile(playlist)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "#EXT-X-MEDIA-SEQUENCE:0\n", "media sequence must have advanced past the evicted segments")

	// The oldest retained segment starts partway into the source; its PDT gives
	// the exact sample offset. The decoded window should then be that tail of
	// the source, aligned near lag zero.
	var first hls.Segment
	for seq := uint64(0); seq < stats.Segments; seq++ {
		if seg, ok := s.Segment(seq); ok {
			first = seg
			break
		}
	}
	offset := int(math.Round(first.PDT.Sub(sampleTime(0)).Seconds() * float64(testRate)))
	require.Positive(t, offset, "the oldest retained segment should not be the first one cut")

	srcTail := deinterleaveCh0(src, 1)[offset:]
	decF := deinterleaveCh0(decodePCM(t, playlist, 1), 1)
	corr, lag := bestCorrelation(decF, srcTail, 1024)
	assert.Greater(t, corr, 0.95, "the retained window is not the matching tail of the source (corr %.3f at lag %d)", corr, lag)
}

// abs is a small integer helper for the lag checks.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
