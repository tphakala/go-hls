package hlshttp_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	hls "github.com/tphakala/go-hls"
	"github.com/tphakala/go-hls/aachls"
	"github.com/tphakala/go-hls/hlshttp"
)

// newLiveStream builds a real AAC-LC stream and feeds it enough silence that
// at least segment 0 has been cut.
func newLiveStream(t *testing.T) *hls.Stream {
	t.Helper()
	s, err := hls.New(&hls.Config{
		Codec:       aachls.AACLC(),
		SampleRate:  48000,
		Channels:    1,
		BitrateKbps: 32,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	pcm := make([]byte, 48000*2) // one second of 16-bit mono silence
	ts := time.Unix(1_700_000_000, 0)
	for range 5 {
		require.NoError(t, s.Write(pcm, ts))
		ts = ts.Add(time.Second)
	}
	require.True(t, s.Ready(1))
	return s
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, http.NoBody))
	return rec
}

func TestServesPlaylist(t *testing.T) {
	t.Parallel()
	s := newLiveStream(t)
	h := hlshttp.NewHandler(s)

	rec := do(h, http.MethodGet, "/"+hlshttp.PlaylistName)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/vnd.apple.mpegurl", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, s.Playlist(), rec.Body.String())
}

func TestServesInitSegment(t *testing.T) {
	t.Parallel()
	s := newLiveStream(t)
	h := hlshttp.NewHandler(s)

	rec := do(h, http.MethodGet, "/"+hls.InitSegmentName)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "video/mp4", rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
	assert.Equal(t, s.InitSegment(), rec.Body.Bytes())
}

func TestServesMediaSegment(t *testing.T) {
	t.Parallel()
	s := newLiveStream(t)
	h := hlshttp.NewHandler(s)

	seg, ok := s.Segment(0)
	require.True(t, ok)
	rec := do(h, http.MethodGet, "/"+hls.SegmentName(0))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "video/iso.segment", rec.Header().Get("Content-Type"))
	assert.Equal(t, seg.Data, rec.Body.Bytes())
}

func TestUnknownPathsAre404(t *testing.T) {
	t.Parallel()
	s := newLiveStream(t)
	h := hlshttp.NewHandler(s)

	for _, path := range []string{
		"/",
		"/nope.mp3",
		"/segment01.m4s",     // non-canonical name must not alias segment 1
		"/segment999999.m4s", // valid name, but far ahead of the window
		"/../" + hlshttp.PlaylistName,
	} {
		rec := do(h, http.MethodGet, path)
		assert.Equal(t, http.StatusNotFound, rec.Code, "path %q", path)
	}
}

func TestNonGetIs405(t *testing.T) {
	t.Parallel()
	s := newLiveStream(t)
	h := hlshttp.NewHandler(s)

	rec := do(h, http.MethodPost, "/"+hlshttp.PlaylistName)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "GET, HEAD", rec.Header().Get("Allow"))
}

func TestHeadReturnsHeadersOnly(t *testing.T) {
	t.Parallel()
	s := newLiveStream(t)
	h := hlshttp.NewHandler(s)

	rec := do(h, http.MethodHead, "/"+hlshttp.PlaylistName)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, strconv.Itoa(len(s.Playlist())), rec.Header().Get("Content-Length"))
	assert.Zero(t, rec.Body.Len())
}
