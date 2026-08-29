// Package hls turns a live PCM stream into HLS media segments and the playlist
// that indexes them, entirely in memory.
//
// The package is deliberately pure: interleaved 16-bit PCM goes in through
// [Stream.Write], fragmented-MP4 (CMAF) segments and an m3u8 media playlist
// come out of the accessors, and nothing in here touches the filesystem, the
// network or a subprocess. That is what makes the segment cutting, the
// timeline arithmetic and the playlist shape table-testable without a live
// audio source. Serving the result over HTTP is the caller's job; the hlshttp
// subpackage provides a ready-made handler for the common case.
//
// The codec is a parameter, not a hardcode. A [Codec] value supplies the
// encoder constructor and the go-m4a container description, and nothing else
// in this package names a codec, so any codec go-m4a can mux (AAC-LC, Opus,
// FLAC) can be plugged in, including from outside this module. The aachls
// subpackage provides AAC-LC, which is the codec to use for HLS unless every
// client is known not to be Apple's; see its documentation.
//
// One [Stream] serves one live source. Write is called from the audio feed
// goroutine; the read side (Playlist, PlaylistAndStats, Segment, Ready, Stats
// and InitSegment) is called from HTTP handlers, is safe for any number of
// concurrent callers, and takes no lock at all, so a large audience can never
// delay the goroutine encoding for it.
package hls
