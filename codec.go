package hls

import (
	m4a "github.com/tphakala/go-m4a"
)

// Codec bundles everything the muxer needs to know about an audio codec: how
// to build an encoder for it, and how to describe it to the container.
//
// The function fields are exported so codecs can be defined outside this
// module: any codec go-m4a can mux is usable. The aachls subpackage provides
// AAC-LC; see its documentation for why that is the codec to serve over HLS
// unless every client is known not to be Apple's.
//
// Note what is deliberately absent: an RFC 6381 codec string. That belongs to
// the CODECS attribute of EXT-X-STREAM-INF, which exists only in a master
// playlist. This package emits a media playlist, where RFC 8216 gives CODECS
// no home, so a player learns the codec by probing the init segment instead.
// Add the field when and if a master playlist is added; carrying it unused
// would only rot.
type Codec struct {
	// Name identifies the codec in configuration and in logs, for example
	// "aac-lc". It never appears in the playlist or in a segment.
	Name string

	// MaxFrameSamples is the largest per-channel sample count one access unit
	// can carry, 1024 for AAC-LC. It exists so New can reject a segment target
	// smaller than a single access unit, a configuration that would otherwise
	// build successfully and then refuse every frame the encoder produced.
	// Zero means the codec does not declare a bound, and the check is skipped.
	MaxFrameSamples int

	// NewEncoder builds the per-stream encoder. One Stream owns the encoder it
	// returns and Closes it on teardown.
	NewEncoder func(EncoderConfig) (FrameEncoder, error)

	// WriterConfig builds the go-m4a configuration describing this codec's
	// sample entry. It runs after the encoder exists, because the container
	// needs the encoder's decoder configuration and priming delay.
	WriterConfig func(EncoderConfig, FrameEncoder) (m4a.WriterConfig, error)
}

// valid reports whether c is usable rather than zero valued. A zero Codec
// would otherwise fail later with a nil dereference inside New.
func (c *Codec) valid() bool {
	return c.NewEncoder != nil && c.WriterConfig != nil
}
