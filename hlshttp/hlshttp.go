// Package hlshttp serves a live hls.Stream over HTTP.
//
// The handler serves three kinds of resource relative to its mount point: the
// media playlist under PlaylistName, the initialization segment under
// hls.InitSegmentName, and the media segments under the names the playlist
// advertises. Mount it under the directory the playlist URL lives in:
//
//	http.Handle("/live/", http.StripPrefix("/live/", hlshttp.NewHandler(mux)))
//
// It deliberately does nothing else: no auth, no CORS, no logging, no range
// requests, no compression. Wrap it in middleware for any of those; every
// policy decision belongs to the application.
package hlshttp

import (
	"net/http"
	"strconv"
	"strings"

	hls "github.com/tphakala/go-hls"
)

// PlaylistName is the path, relative to the handler's mount point, that
// serves the media playlist. Point players at it.
const PlaylistName = "live.m3u8"

const (
	playlistContentType = "application/vnd.apple.mpegurl"
	initContentType     = "video/mp4"
	segmentContentType  = "video/iso.segment"

	// immutableCache is sent with the init segment and the media segments,
	// neither of which ever changes for a given URL: the init segment is
	// fixed for the life of the stream and a sequence number is never reused.
	immutableCache = "public, max-age=31536000, immutable"

	// playlistCache keeps caches out of the way of a resource that changes on
	// every segment cut.
	playlistCache = "no-store"
)

type handler struct {
	s *hls.Stream
}

// NewHandler returns a handler serving s's playlist, init segment and media
// segments. It is safe for any number of concurrent requests: the Stream read
// side is lock free, so requests never contend with each other or with the
// goroutine writing audio.
func NewHandler(s *hls.Stream) http.Handler {
	return handler{s: s}
}

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	switch name {
	case PlaylistName:
		reply(w, r, []byte(h.s.Playlist()), playlistContentType, playlistCache)
	case hls.InitSegmentName:
		reply(w, r, h.s.InitSegment(), initContentType, immutableCache)
	default:
		// ParseSegmentName accepts only the canonical spelling, so this is
		// the whole of path validation: anything else, including traversal
		// attempts and non-canonical zero-padded names, falls through to 404.
		seq, ok := hls.ParseSegmentName(name)
		if !ok {
			http.NotFound(w, r)
			return
		}
		seg, ok := h.s.Segment(seq)
		if !ok {
			// Evicted from the window, or not yet cut. A client that fell
			// behind treats the 404 as its signal to resync via the playlist.
			http.NotFound(w, r)
			return
		}
		reply(w, r, seg.Data, segmentContentType, immutableCache)
	}
}

// reply writes one complete in-memory resource.
func reply(w http.ResponseWriter, r *http.Request, body []byte, contentType, cacheControl string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	// A failed write means the client went away; there is nobody to tell.
	_, _ = w.Write(body)
}
