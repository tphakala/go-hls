package hls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteAfterCloseWrapsErrClosed(t *testing.T) {
	t.Parallel()
	s, err := New(&Config{Codec: newFakeCodec(&fakeCodecOptions{}), SampleRate: 48000, Channels: 1})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	err = s.Write(silence(1024, 1), time.Unix(1, 0))
	require.ErrorIs(t, err, ErrClosed)
	require.NotErrorIs(t, err, ErrFailed)
}

func TestLatchingFailureWrapsErrFailed(t *testing.T) {
	t.Parallel()
	s, err := New(&Config{Codec: newFakeCodec(&fakeCodecOptions{failAfter: 1}), SampleRate: 48000, Channels: 1})
	require.NoError(t, err)

	// Four frames' worth of audio: the first access unit emits, the second
	// trips failAfter, so this Write is the one that latches the stream.
	ts := time.Unix(1, 0)
	err = s.Write(silence(4096, 1), ts)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrFailed)
	assert.True(t, s.Stats().Failed)

	// Every later Write reports the latch too.
	err = s.Write(silence(1024, 1), ts.Add(time.Second))
	require.ErrorIs(t, err, ErrFailed)
	require.NotErrorIs(t, err, ErrClosed)
}

func TestMisalignedPCMDoesNotLatch(t *testing.T) {
	t.Parallel()
	s, err := New(&Config{Codec: newFakeCodec(&fakeCodecOptions{}), SampleRate: 48000, Channels: 2})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ts := time.Unix(1, 0)
	err = s.Write(make([]byte, 3), ts)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrFailed)
	require.NotErrorIs(t, err, ErrClosed)
	assert.False(t, s.Stats().Failed)

	// The stream is still usable after the rejection.
	require.NoError(t, s.Write(silence(1024, 2), ts))
}

// TestWriteEmptyPCMIsNoOp documents that a zero-length write is accepted and
// changes nothing: a capture loop that hands over an empty buffer must not trip
// the frame-alignment check or advance the timeline.
func TestWriteEmptyPCMIsNoOp(t *testing.T) {
	t.Parallel()
	s, err := New(&Config{Codec: newFakeCodec(&fakeCodecOptions{}), SampleRate: 48000, Channels: 1})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	require.NoError(t, s.Write(nil, time.Unix(1, 0)))
	require.NoError(t, s.Write([]byte{}, time.Unix(1, 0)))
	assert.Zero(t, s.Stats().Segments, "an empty write must not cut a segment")
}

// TestNewClosesEncoderWhenContainerInitFails proves the half-built-stream
// cleanup path: when the container rejects the writer config after the encoder
// has already been constructed, New must Close that encoder rather than leak it,
// and surface the failure.
func TestNewClosesEncoderWhenContainerInitFails(t *testing.T) {
	t.Parallel()
	var enc *fakeEncoder
	_, err := New(&Config{
		Codec:      newFakeCodec(&fakeCodecOptions{badWriterConfig: true, captured: &enc}),
		SampleRate: 48000,
		Channels:   1,
	})
	require.Error(t, err)
	require.NotNil(t, enc, "the encoder should have been built before the container rejected the config")
	assert.True(t, enc.closed, "New must Close the encoder it built when a later step fails")
}

// TestNewReportsBothErrorsWhenEncoderCloseAlsoFails covers the doubly-unhappy
// path: a later step fails, and closing the encoder to clean up fails too. Both
// causes must appear in the returned error rather than one masking the other.
func TestNewReportsBothErrorsWhenEncoderCloseAlsoFails(t *testing.T) {
	t.Parallel()
	_, err := New(&Config{
		Codec:      newFakeCodec(&fakeCodecOptions{writerErr: errFakeEncoder, closeErr: errFakeClose}),
		SampleRate: 48000,
		Channels:   1,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errFakeEncoder, "the original failure must be reported")
	require.ErrorIs(t, err, errFakeClose, "the cleanup failure must also be reported")
}
