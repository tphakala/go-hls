// Package aachls provides the AAC-LC codec for go-hls, backed by go-aac.
//
// AAC-LC is the codec to serve over HLS. The choice is compatibility, not
// merit: Opus is roughly three times cheaper to encode and sounds better at
// streaming bitrates, but it is not a codec in Apple's HLS Authoring
// Specification and Apple's HLS stack will not play it, so an Opus HLS stream
// is silent on every iPhone. (Safari plays Opus in other containers; it is
// HLS specifically that rules it out.) go-hls keeps the codec a parameter so
// that judgement can be revisited per deployment.
package aachls

import (
	"fmt"

	aac "github.com/tphakala/go-aac"
	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"

	hls "github.com/tphakala/go-hls"
)

const (
	// aacCodecName identifies AAC-LC in configuration and in logs.
	aacCodecName = "aac-lc"

	// bitDepth16 is the PCM sample width go-hls assumes and go-aac is told.
	bitDepth16 = 16

	// aacFrameSamples is the per-channel sample count of one access unit, read
	// from go-aac rather than restated, because it is what this encoder emits
	// rather than what the format permits (AAC-LC also admits 960-sample
	// framing). New checks a segment target against it, so a segment too short
	// to hold a single access unit is rejected at construction rather than by
	// every frame the encoder produces.
	aacFrameSamples = aac.FrameSize

	// bitsPerKilobit converts the codec-independent BitrateKbps carried by
	// EncoderConfig to the bits per second go-aac's Config takes.
	bitsPerKilobit = 1000
)

// AACLC returns the AAC-LC codec, backed by go-aac for encoding and go-m4a's
// mp4a sample entry for the container. See the package documentation for why
// AAC-LC is the codec to serve over HLS.
func AACLC() hls.Codec {
	return hls.Codec{
		Name:            aacCodecName,
		MaxFrameSamples: aacFrameSamples,
		NewEncoder:      newAACEncoder,
		WriterConfig:    aacWriterConfig,
	}
}

// aacEncoder adapts go-aac's pcm.FrameEncoder to the FrameEncoder interface.
//
// The adaptation is thin but not omittable, for three independent reasons, of
// which the first two are the load-bearing ones: go-aac's encoder has no Close
// at all, and it names the decoder configuration AudioSpecificConfig rather
// than DecoderConfig (this package renames it because the same interface has to
// carry a FLAC STREAMINFO and an Opus dOps). Additionally, interface
// satisfaction demands identical method signatures, and go-aac spells the emit
// parameter as an unnamed func type where this package declares EmitFunc.
//
// Note that last point constrains the method set only, not the call: EmitFunc
// is assignable to go-aac's unnamed parameter type, so the methods below pass
// emit straight through. Wrapping it in a closure would be worse than
// redundant, because it would mask go-aac's own nil-emit check and turn a
// clean error into a nil-func-call panic inside the dependency.
type aacEncoder struct {
	enc *aacpcm.FrameEncoder
}

// newAACEncoder builds the per-stream AAC-LC encoder.
func newAACEncoder(cfg hls.EncoderConfig) (hls.FrameEncoder, error) {
	// Cutoff is deliberately left at zero, selecting go-aac's tuned
	// rate-dependent default: roughly 18.7 kHz at 128 kbps, falling to 14 kHz
	// at 32 kbps and about 8 kHz at 16 kbps. Changing it changes what listeners
	// hear, so it wants measurement rather than a value picked while wiring the
	// codec up.
	enc, err := aacpcm.NewFrameEncoder(aacpcm.Config{
		SampleRate: cfg.SampleRate,
		BitDepth:   bitDepth16,
		Channels:   cfg.Channels,
		Bitrate:    cfg.BitrateKbps * bitsPerKilobit,
	})
	if err != nil {
		return nil, fmt.Errorf("aachls: build AAC-LC encoder: %w", err)
	}
	return &aacEncoder{enc: enc}, nil
}

// aacWriterConfig describes the AAC-LC track to go-m4a.
func aacWriterConfig(cfg hls.EncoderConfig, enc hls.FrameEncoder) (m4a.WriterConfig, error) {
	asc := enc.DecoderConfig()
	if len(asc) == 0 {
		return m4a.WriterConfig{}, fmt.Errorf("aachls: AAC-LC encoder produced an empty AudioSpecificConfig")
	}

	// Map the encoder's reported priming onto go-m4a's EncoderDelay, whose zero
	// value means "use the codec default" (1024 for AAC-LC) rather than "no
	// priming". Passing a reported zero straight through would therefore trim a
	// frame that was never added, shifting the whole timeline by 1024 samples.
	// NoEdit is the sentinel that suppresses the edit list, which is what an
	// encoder reporting no priming actually wants.
	encoderDelay := enc.Delay()
	if encoderDelay == 0 {
		encoderDelay = m4a.NoEdit
	}

	// MediaLength is deliberately left unset. It pins the edit list to an exact
	// source length, which a live stream does not have, and go-m4a's fragmented
	// constructors reject any non-zero value rather than ignore it.
	return m4a.WriterConfig{
		Codec:        m4a.CodecAACLC,
		SampleRate:   cfg.SampleRate,
		Channels:     cfg.Channels,
		ASC:          asc,
		EncoderDelay: encoderDelay,
	}, nil
}

// EncodeInterleaved consumes pcm and reports each complete access unit.
func (a *aacEncoder) EncodeInterleaved(pcm []byte, emit hls.EmitFunc) error {
	return a.enc.EncodeInterleaved(pcm, emit)
}

// Flush drains the encoder lookahead so the final frame is not lost.
func (a *aacEncoder) Flush(emit hls.EmitFunc) error {
	return a.enc.Flush(emit)
}

// DecoderConfig returns the MPEG-4 AudioSpecificConfig for the esds box.
func (a *aacEncoder) DecoderConfig() []byte {
	return a.enc.AudioSpecificConfig()
}

// Delay is the encoder priming in samples, trimmed by the container edit list.
func (a *aacEncoder) Delay() int {
	return a.enc.Delay()
}

// Close releases the encoder. go-aac holds nothing that needs releasing, so
// this is a no-op; the method exists so a pooled or cgo-backed encoder can be
// substituted later without changing the interface.
func (a *aacEncoder) Close() error {
	return nil
}
