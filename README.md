# go-hls

[![CI](https://github.com/tphakala/go-hls/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-hls/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-hls.svg)](https://pkg.go.dev/github.com/tphakala/go-hls)
[![codecov](https://codecov.io/gh/tphakala/go-hls/branch/main/graph/badge.svg)](https://codecov.io/gh/tphakala/go-hls)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-hls)](go.mod)
[![Latest tag](https://img.shields.io/github/v/tag/tphakala/go-hls?sort=semver&label=release)](https://github.com/tphakala/go-hls/tags)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/tphakala/go-hls/badge)](https://scorecard.dev/viewer/?uri=github.com/tphakala/go-hls)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor)](https://github.com/sponsors/tphakala)

A pure-Go live HLS packager for audio. Interleaved PCM and a capture timestamp
go in through one `Write` call; fragmented-MP4 (CMAF) media segments and the
RFC 8216 media playlist indexing them come out of lock-free accessors. Everything
is held in memory: no cgo, no FFmpeg subprocess, no filesystem, no network. The
library cuts segments on access-unit boundaries, keeps a bounded sliding window,
anchors `EXT-X-PROGRAM-DATE-TIME` to wall clock, absorbs source clock drift, and
declares `EXT-X-DISCONTINUITY` when the source actually stalls, so what a player
reports as stream time is time that really happened.

go-hls is the serving half of a family of pure-Go audio libraries. It muxes with
[go-m4a](https://github.com/tphakala/go-m4a) and encodes through codec bridges
such as [go-aac](https://github.com/tphakala/go-aac); on the ingest side,
[go-audio-stream](https://github.com/tphakala/go-audio-stream) is the matching
library that pulls audio off the network (including an HLS source). It shares
conventions and audio vocabulary with
[go-opus](https://github.com/tphakala/go-opus),
[go-flac](https://github.com/tphakala/go-flac) and
[go-wav](https://github.com/tphakala/go-wav).

## Install

```bash
go get github.com/tphakala/go-hls
```

## Packages

| Package | What it is | Pulls in |
|---------|------------|----------|
| `hls` (root) | The codec-agnostic muxer: segment cutting, timeline arithmetic, playlist rendering | go-m4a only |
| `aachls` | The AAC-LC codec, backed by go-aac | go-aac |
| `hlshttp` | An `http.Handler` that serves a stream's playlist, init segment and media segments | stdlib only |

The root package never names a codec. A `Codec` value supplies the encoder
constructor and the container description, so any codec go-m4a can mux (AAC-LC,
Opus, FLAC) can be plugged in, including from outside this module. `aachls` is
the one shipped here because AAC-LC is the codec HLS players actually play; see
its documentation for why Opus, despite being cheaper and better sounding, is
not an option for HLS specifically.

## Quick start

Encode a live 48 kHz mono source to AAC-LC and serve it at `/live/live.m3u8`:

```go
package main

import (
	"log"
	"net/http"
	"time"

	hls "github.com/tphakala/go-hls"
	"github.com/tphakala/go-hls/aachls"
	"github.com/tphakala/go-hls/hlshttp"
)

func main() {
	mux, err := hls.New(&hls.Config{
		Codec:       aachls.AACLC(),
		SampleRate:  48000,
		Channels:    1,
		BitrateKbps: 96,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer mux.Close()

	// Feed the muxer from your capture loop: interleaved 16-bit
	// little-endian PCM plus the wall-clock capture time of its first
	// sample. Everything else is derived from these two arguments.
	go func() {
		for buf := range captureLoop() {
			if err := mux.Write(buf.PCM, buf.Time); err != nil {
				log.Printf("hls write: %v", err)
				return
			}
		}
	}()

	http.Handle("/live/", http.StripPrefix("/live/", hlshttp.NewHandler(mux)))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

A browser needs [hls.js](https://github.com/video-dev/hls.js) (or Safari, which
plays HLS natively) pointed at the playlist URL. Poll `mux.Ready(n)` before
advertising the stream: players hold off starting playback until a few segments
exist, so handing out the URL immediately costs the first listener a reload
cycle.

## Design

**One muxer per source, any number of listeners.** `Write` is called from the
capture goroutine; the read side (`Playlist`, `Segment`, `InitSegment`, `Ready`,
`Stats`) is called from HTTP handlers and takes no lock at all. Readers load an
immutable snapshot that is republished once per segment cut, so an audience of
any size can never delay the goroutine encoding for it, and a poll between cuts
costs one atomic load instead of a render.

**Exact timeline arithmetic.** Segment durations are derived from sample counts
with remainder-carrying arithmetic, so consecutive `EXTINF` durations sum to
exactly the duration of the audio, forever, without drift or overflow. The
playlist advertises `EXT-X-TARGETDURATION` as a true upper bound: segments are
cut before they would exceed it, never after.

**Wall-clock honesty.** Each `Write` carries the capture time of its first
sample. Small divergence between the source's sample clock and wall clock is
absorbed gradually (a commodity 100 ppm crystal would otherwise look like a
stall within hours); a divergence beyond `MaxStallGap` means real audio is
missing, so the stream closes out the segment, declares `EXT-X-DISCONTINUITY`
with a stable `EXT-X-DISCONTINUITY-SEQUENCE`, and re-anchors
`EXT-X-PROGRAM-DATE-TIME` to the time that actually arrived. A monitoring
system replaying the stream sees the gap instead of a silently shifted
timeline.

**Failures latch.** If the encoder or container ever rejects audio, the
timeline can no longer be reconciled with what was consumed, so the stream
refuses further writes (wrapping `hls.ErrFailed`) rather than continuing with a
silently corrupted timeline. Tear it down and build a new one; `Close` is
idempotent and safe on every path.

**Bounded memory.** The playlist advertises a sliding window (six segments by
default) and evicted segments are dropped; memory use is constant no matter how
long the stream runs. At the default 2-second segments and 96 kbps, a stream
retains on the order of 150 kB.

## Limits

- Input is interleaved 16-bit little-endian PCM, 1 or 2 channels. `Config`
  carries no bit depth; feeding wider samples is undetectable and produces
  garbage audio, so the contract is on the caller.
- Output is a live media playlist. There is no master playlist (and therefore
  no `CODECS` attribute, which RFC 8216 gives no home in a media playlist), no
  multi-variant switching, no VOD mode beyond `EXT-X-ENDLIST` on `Close`, and
  no LL-HLS partial segments.
- Segments live in memory and are served from memory. Nothing is written to
  disk.

## License

MIT
