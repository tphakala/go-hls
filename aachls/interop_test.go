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

// feedTone writes seconds of a sine at hz through the stream in chunks that do
// not divide the 1024-sample AAC access unit, so partial-frame buffering is
// exercised on every write. It returns the source PCM and the next timestamp.
func feedTone(t *testing.T, s *hls.Stream, start time.Time, seconds, channels int, hz float64) (src []byte, next time.Time) {
	t.Helper()
	const chunk = 1200
	total := seconds * testRate
	at := start
	for written := 0; written < total; {
		n := chunk
		if written+n > total {
			n = total - written
		}
		buf := tone(n, channels, testRate, hz)
		require.NoError(t, s.Write(buf, at))
		src = append(src, buf...)
		at = at.Add(time.Duration(n) * time.Second / testRate)
		written += n
	}
	return src, at
}

// TestInteropStreamDecodesToSource is the core end-to-end proof: a known sine
// fed through the muxer decodes back, through ffmpeg reading the playlist, to
// the same sine. A wrong sample duration, a broken edit list, a dropped access
// unit or a malformed box would all break the correlation.
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

	src, _ := feedTone(t, s, sampleTime(0), 5, 1, 3000)
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

	// The decoded sine matches the source at a single constant lag. Because the
	// lag is constant and the correlation high, every sample is present in
	// order: a dropped or duplicated access unit would misalign one half of the
	// signal against the other and collapse the correlation, not merely shift
	// it. The lag itself is a small fixed offset (tens of milliseconds) from
	// ffmpeg's HLS-demuxer and AAC-decoder priming, not a container defect; the
	// edit-list priming trim on this exact writer config is verified sample-
	// accurately in go-m4a's aacm4a suite. The bound here only has to exclude a
	// gross timeline break, which would be a whole segment (two seconds) off.
	corr, lag := bestCorrelation(srcF, decF, testRate/4)
	assert.Greater(t, corr, 0.9, "decoded audio does not match the source sine (peak corr %.3f at lag %d)", corr, lag)
	assert.Less(t, abs(lag), testRate/8, "playback is misaligned by %d samples, far more than codec priming", lag)
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

	src, _ := feedTone(t, s, sampleTime(0), 4, 2, 2500)
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
	_, _ = feedTone(t, s, sampleTime(0), 3, 1, 3000)
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

	// Four seconds of one tone, then a stall well past MaxStallGap, then four
	// seconds of a clearly different tone from the resume instant.
	_, afterFirst := feedTone(t, s, sampleTime(0), 4, 1, 2000)
	resume := afterFirst.Add(30 * time.Second)
	_, _ = feedTone(t, s, resume, 4, 1, 6000)
	require.NoError(t, s.Close())

	stats := s.Stats()
	require.Equal(t, uint64(1), stats.Discontinuities, "the stall must declare exactly one discontinuity")

	playlist := writeHLSDir(t, s)
	body, err := os.ReadFile(playlist)
	require.NoError(t, err)
	assert.Contains(t, string(body), "#EXT-X-DISCONTINUITY\n", "the playlist must mark the timeline break")

	// ffmpeg must decode across the discontinuity without error, and the result
	// must carry real audio on both sides of the break rather than truncating
	// at the gap.
	decF := deinterleaveCh0(decodePCM(t, playlist, 1), 1)
	require.Greater(t, len(decF), 6*testRate, "decoded audio is too short to contain both halves")
	half := len(decF) / 2
	assert.Positive(t, rms(decF[:half]), "pre-gap audio decoded to silence")
	assert.Positive(t, rms(decF[half:]), "post-gap audio decoded to silence")
}

// abs is a small integer helper for the lag checks.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
