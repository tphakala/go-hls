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
