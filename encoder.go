package hls

// EmitFunc receives one coded access unit and the per-channel sample count it
// decodes to.
//
// The au slice is borrowed: it is valid only until EmitFunc returns, so an
// implementation that needs to retain the bytes must copy them. This lets an
// encoder reuse a single scratch buffer for the whole stream. go-m4a's
// FragmentWriter copies on WriteFrameDuration, so the muxer in this package
// can pass the borrowed slice straight through.
//
// EmitFunc runs with the stream's lock already held, so it must not call back
// into any Stream method. Go mutexes are not reentrant and doing so would
// deadlock the audio goroutine.
type EmitFunc func(au []byte, samples int) error

// FrameEncoder turns interleaved PCM into coded access units, reporting each
// unit through a callback rather than writing a self-framing stream. A muxer
// needs the units and their boundaries separately; a framed stream such as
// ADTS would have to be re-split to recover them.
//
// One live stream owns one FrameEncoder. Implementations are not required to
// be safe for concurrent use.
type FrameEncoder interface {
	// EncodeInterleaved consumes pcm, buffering any partial trailing frame,
	// and calls emit once per complete access unit. A short write is
	// therefore normal and emits nothing.
	EncodeInterleaved(pcm []byte, emit EmitFunc) error

	// Flush drains the encoder lookahead so the final samples of a stream are
	// emitted rather than lost. Encoders that delay output by a frame (AAC-LC
	// does) would otherwise drop the tail on every stream teardown.
	Flush(emit EmitFunc) error

	// DecoderConfig returns the codec-specific decoder configuration the
	// container must carry: the MPEG-4 AudioSpecificConfig for AAC-LC, the
	// STREAMINFO block for FLAC, or nil for a codec that needs none. The
	// caller owns the returned bytes.
	DecoderConfig() []byte

	// Delay is the number of leading priming samples the muxer trims with an
	// edit list. Zero means the codec has no priming.
	Delay() int

	// Close releases whatever the encoder holds. Pure-Go encoders have
	// nothing to release and can return nil, but the method keeps a pooled or
	// cgo-backed implementation from having to retrofit the interface.
	Close() error
}

// EncoderConfig is the codec-independent description of the audio an encoder
// is being asked to produce. Anything codec-specific belongs in the Codec that
// builds the encoder, not here.
type EncoderConfig struct {
	// SampleRate is the output sample rate in Hz.
	SampleRate int

	// Channels is the output channel count.
	Channels int

	// BitrateKbps is the target bitrate in kilobits per second. Encoders for
	// lossless codecs ignore it.
	BitrateKbps int
}
