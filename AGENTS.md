# AGENTS.md

Orientation for coding agents and automated reviewers working on this repository. README.md is the user-facing reference (quick start, design notes, limits); SECURITY.md has the vulnerability reporting policy. This file is the map for changing and reviewing the code.

## What this is

`github.com/tphakala/go-hls` (root package `hls`) is a pure-Go live HLS packager for audio. Interleaved 16-bit little-endian PCM plus the capture time of its first sample go in through `Stream.Write`; fragmented-MP4 (CMAF) media segments and an RFC 8216 media playlist come out of lock-free accessors. Everything is held in memory. It aims to:

1. **Build anywhere Go builds.** No cgo, no FFmpeg subprocess, no filesystem, no network. The root package depends only on go-m4a; a codec comes in through a `Codec` value, and the shipped AAC-LC codec lives in its own subpackage.
2. **Keep the timeline honest.** Segment durations come from sample counts with remainder-carrying arithmetic, so consecutive `EXTINF` values sum exactly to the audio duration. `EXT-X-PROGRAM-DATE-TIME` is anchored to wall clock, small clock drift is absorbed, and a real stall is declared with `EXT-X-DISCONTINUITY`.
3. **Serve any audience without slowing the writer.** One goroutine calls `Write`; any number of HTTP handlers read. The read side takes no lock, and memory is bounded by a sliding window no matter how long the stream runs. Consumers are long-running unattended services (BirdNET-Go is the main one, shipping on 32-bit ARM as well).

Packages:

| Package | Role |
|---|---|
| `hls` (root) | Codec-agnostic `Stream` (`New`, `Write`, `Close`, `Playlist`, `Segment`, `InitSegment`, `Ready`, `Stats`), `Config`, `Codec`, `FrameEncoder`, `EmitFunc`, `EncoderConfig`, `Segment`, `SegmentName`/`ParseSegmentName`, `ErrClosed`, `ErrFailed` |
| `aachls` | AAC-LC `Codec` (`AACLC()`) backed by go-aac |
| `hlshttp` | `http.Handler` (`NewHandler`) serving the playlist, init segment and media segments; stdlib plus the root `hls` package only |
| `internal/rules` | gocritic ruleguard matchers (build tag `ruleguard`, lint-only) |
| `tools` | `tools.go` anchoring the go-ruleguard DSL dependency for `go mod tidy` |

Non-goals: a master playlist (so no `CODECS` attribute), multi-variant switching, LL-HLS partial segments, VOD beyond `EXT-X-ENDLIST` on `Close`, a bit depth other than 16, writing segments to disk, an Opus codec in this module (Apple's HLS stack will not play Opus; see `aachls` docs), and any HTTP policy in `hlshttp` (auth, CORS, logging, range requests, compression).

### API stability

The module is pre-1.0. Exported types, functions and fields may change in a minor release when a cleaner design calls for it. Prefer the better API over compatibility: do not add deprecated aliases, adapters or shims to keep an old signature working. A changed signature still needs every caller in this repo updated, plus the README and godoc. There is no CHANGELOG file; release notes are written at release time.

The design rules below are the library's contract and hold across every release.

## Design rules you must not break

A change that violates one of these is wrong even if tests pass.

1. **The root package never names a codec.** `hls` imports only go-m4a (plus stdlib; its tests also use testify). Never import go-aac, go-opus or go-flac into the root package, including from its tests; codec-specific code belongs in a subpackage such as `aachls`. `Codec` stays unsealed (exported function fields) so third parties can plug in any codec go-m4a can mux.
2. **No cgo, no filesystem, no network, no subprocess** in `hls`. That is what keeps segment cutting, timeline arithmetic and playlist shape table-testable.
3. **The read side is lock-free.** `Playlist`, `PlaylistAndStats`, `Segment`, `InitSegment`, `Ready` and `Stats` read an immutable `view` through `atomic.Pointer` (plus atomics for drift correction and the failed flag). `publishView` runs once in `New`, once per segment cut and on `Close`. Never take `Stream.mu` on a read path, and never mutate a published `view` or a published `Segment.Data`.
4. **`EmitFunc` runs under the stream mutex.** An encoder callback must not call back into any `Stream` method (Go mutexes are not reentrant). The `au` slice is borrowed and valid only until the callback returns; the muxer relies on go-m4a's `FragmentWriter` copying it.
5. **Failures latch.** If the encoder or container rejects audio, the stream sets the failed flag and every later `Write` returns `ErrFailed` (wrapped with the codec name on the latching write). Do not add a recovery path that continues with a timeline that no longer matches the consumed audio. `ErrClosed` means `Write` after `Close` began. Callers test both with `errors.Is`.
6. **`Close` is idempotent and always publishes the ended view.** The final `publishView(true)` is deferred so it runs even if codec code panics during flush; keep it deferred.
7. **Exact timeline arithmetic.** Durations are derived from sample counts through `sampleClock` (rate plus carry), never from float math or wall-clock subtraction. `EXT-X-TARGETDURATION` is a true upper bound: segments are cut before they would exceed it, never after. `EXTINF` keeps 6 fractional digits.
8. **Drift versus stall.** The stall threshold (`MaxStallGap`, default half the segment duration) is tested before the drift correction (`driftCorrectionShift`) is applied, so a genuine step change is still detected on the write that carries it. A stall closes the segment, sets `EXT-X-DISCONTINUITY`, advances the stable `EXT-X-DISCONTINUITY-SEQUENCE` and re-anchors `EXT-X-PROGRAM-DATE-TIME`.
9. **Config bounds are validated at `New`.** `SampleRate` 1 to `MaxSampleRate` (`1<<20 - 1`), `SegmentDuration` between `MinSegmentDuration` (100 ms) and `MaxSegmentDuration` (60 s), a non-negative `MaxStallGap`, and a segment target at least `Codec.MaxFrameSamples` long. A compile-time `int32` assertion in `stream.go` ties `MaxSegmentDuration` to `MaxSampleRate`; raising either bound must keep it compiling. Errors for bad config or bad `Write` input are plain validation errors, not `ErrFailed`.
10. **Input contract.** `Write` takes interleaved 16-bit little-endian PCM, 1 or 2 channels, a non-zero timestamp, and a length that is a whole number of sample frames. `Config` has no bit depth and wider samples cannot be detected reliably, so the contract stays on the caller and in the docs.
11. **Segment names are canonical.** `SegmentName` writes the sequence number in full (no zero padding, no wrap at 1000); `ParseSegmentName` accepts only that exact spelling. In `hlshttp` this is the whole of path validation, so traversal and non-canonical names fall through to 404. Keep the parser strict and fuzzed.
12. **`hlshttp` stays policy-free.** GET and HEAD only (405 with `Allow` otherwise). The playlist is `no-store`; the init segment and media segments are `public, max-age=31536000, immutable` because a URL's content never changes. Evicted or not-yet-cut segments return 404 so a lagging client resyncs from the playlist.
13. **Bounded memory.** The playlist window holds `Config.WindowSize` segments (default `DefaultWindowSize` 6, which must stay above hls.js's three-segment live sync count); evicted segments are dropped.

## Layout

```
doc.go          Package documentation
stream.go       Config, Stats, Stream, New, Write, Close, timeline sync, segment cutting, view
                publication, read-side accessors, limits (Default/Min/Max constants)
segment.go      Segment, SegmentName/ParseSegmentName, InitSegmentName, segmentWindow
playlist.go     renderPlaylist (EXT-X-VERSION 7, EXTINF, PDT, discontinuity tags)
codec.go        Codec
encoder.go      FrameEncoder, EmitFunc, EncoderConfig
errors.go       ErrClosed, ErrFailed

aachls/         aachls.go (AACLC, go-aac encoder adapter), interop_test.go (ffmpeg), bench_test.go
hlshttp/        hlshttp.go (PlaylistName, NewHandler)
internal/rules/ ruleguard matchers, one file per topic (build tag ruleguard)
tools/          tools.go
```

Root tests: `stream_test.go`, `segment_test.go`, `playlist_test.go`, `errors_test.go`, `fuzz_test.go`, `bench_test.go`, and `fake_test.go`, a test-only fake codec that lets the root package be tested without importing any real codec.

## Testing approach

Tests need no audio hardware; only the interop tests need an external tool.

- **Regression tests.** New behaviour gets a test. A bug fix gets a regression test that fails before the fix. A test must be able to fail: break the line it pins and confirm it fails.
- **Fake codec.** `fake_test.go` implements `FrameEncoder` and a `Codec` for the root tests. go-m4a rejects an all-zero or deferred FLAC STREAMINFO for fragmented output and cross-checks its sample rate against the writer config, so the fake must emit a populated 34-byte STREAMINFO matching the configured rate and channels.
- **Interop with ffmpeg.** `aachls/interop_test.go` decodes go-hls's own output with ffmpeg and ffprobe. They skip when ffmpeg is absent; the Linux CI leg installs it. Correlation checks use a chirp, not a fixed tone: a fixed tone is periodic on a scale much shorter than the 1024-sample AAC access unit, so a dropped frame would realign the phase and still pass. The lag search is bounded (`interopMaxLag`) because a full-rate sweep makes the `-race` run too slow.
- **Fuzzing.** `FuzzParseSegmentName` and `FuzzSegmentNameRoundTrip` (root), `FuzzHandlerPath` (`hlshttp`). A `-fuzz` pattern that matches nothing passes without running, so run a new target in the package that defines it. Crashers are committed as seeds under `testdata/fuzz`.
- **Architectures.** CI also runs the suite as a `GOARCH=386` binary (without `-race`, which is 64-bit only), because size and offset arithmetic must hold on 32-bit.
- **Benchmarks.** `bench_test.go` in the root and in `aachls`.

## Commands

The Go version in `go.mod` is a floor (`go 1.27.0`, forced by go-m4a); never pin a patch version. CI uses setup-go with `'1.27'` and `check-latest: true`, so it always runs the newest 1.27.x. The golangci-lint version is pinned in `.github/workflows/ci.yml` (`GOLANGCI_LINT_VERSION`).

```
task build      # go build ./...
task vet        # go vet for amd64 and linux/arm64
task fmt:check  # fail if any file is not gofmt-clean
task lint       # golangci-lint run
task test       # go test -race ./...
task check:all  # full pre-push gate: build, vet, fmt:check, lint, test
go test -race -tags noasm ./...   # scalar-path run, covered by its own CI step
```

Run `task check:all` before pushing. CI jobs: Test (ubuntu, macOS, Windows; race and coverage; a `-tags noasm` run), Test (386), Lint, Vet, Build, Fuzz smoke (20 seconds per target), plus separate govulncheck and OpenSSF Scorecard workflows. Dependabot keeps Go modules and GitHub Actions current.

## Conventions

- **Lint.** Config is `.golangci.yaml` (golangci-lint v2), shared in spirit with the sibling libraries. Enabled beyond the defaults: bodyclose, copyloopvar, dupl, durationcheck, errname, errorlint, exhaustive (`default` counts as exhaustive), fatcontext, gocognit (50), goconst, gocritic (style, diagnostic and performance tags; `hugeParam` threshold 128 bytes; ruleguard rules from `internal/rules/*.go`), gocyclo, iface, ineffassign, misspell, nilerr, nilnil, predeclared, prealloc, revive (with the `exported` rule), staticcheck, testifylint, thelper, unconvert, wastedassign. `unused` is disabled. Formatters: gofmt and goimports with local prefix `github.com/tphakala/go-hls`. Test files are excluded from dupl, gocognit, gocyclo and goconst.
- **Ruleguard.** The matchers carry `//go:build ruleguard` so the normal toolchain ignores them; `tools/tools.go` keeps the DSL dependency in `go.mod`. Cached analysis is not keyed on the rule files, so run `golangci-lint cache clean` after editing one.
- **Errors.** Error strings carry the `hls:` prefix. Wrap with `%w`; sentinels are `ErrClosed` and `ErrFailed` only. Do not add a sentinel for a condition callers cannot act on differently.
- **Comments.** Explain why: the RFC 8216 section where it is not obvious, the failure being avoided, the measurement behind a constant. Match the existing density of the constant blocks in `stream.go` and `playlist.go`.
- **Dependencies.** go-m4a, go-aac and testify are the only direct runtime and test dependencies; go-ruleguard/dsl is build-time only. A new dependency in the root package needs a strong reason.
- **Commits** use an imperative subject with a type prefix (`feat:`, `fix:`, `docs:`, `build:`, `ci:`, `chore:`, `refactor:`, `test:`), optionally scoped.

## Review guidance

For CodeRabbit, Copilot and human reviewers. Rate findings on what a caller of the library would see.

Flag these, they are real defects here:

- Any break of the design rules above: a codec import in the root package, cgo, a lock or blocking call on a read path, a mutation of a published view or segment, an `EmitFunc` that re-enters `Stream`.
- A timeline change that makes `EXTINF` durations stop summing to the audio duration, lets a segment exceed `EXT-X-TARGETDURATION`, or loses the sample carry.
- A stall that is not declared as a discontinuity, a healthy drifting source that is, or a discontinuity sequence that is not stable across window eviction.
- A latched stream that accepts more audio, or a `Close` that can skip the ended publish.
- Size or offset arithmetic that is wrong on 32-bit (`int` is 32 bits on 386 and arm).
- A path in `hlshttp` that serves anything other than the playlist, init segment or a canonical segment name.
- A test that cannot fail on the behaviour it names, or an interop check that a dropped frame would still pass.
- A doc sentence that is false for some codec.

Intentional, do not flag:

- 16-bit PCM only with no bit-depth field, and no detection of wider samples.
- No master playlist, no `CODECS` attribute, and no Opus codec in this module.
- `EXT-X-VERSION` 7 even though 6 would be legal; 7 is what Apple's authoring spec pairs with fragmented MP4.
- `hlshttp` having no auth, CORS, logging, range requests or compression; callers wrap it.
- A single `Codec` value holding function fields instead of a sealed interface.
- Breaking exported-API changes before v1.0.0 (see API stability), and the absence of deprecated aliases or shims.
- GitHub Actions pinned by release tag with Dependabot rather than by commit SHA.

## Things that do not belong in the repo

- Planning and design documents; `docs/` is gitignored and must stay that way.
- Local tooling and session scratch (`.serena/`, `.codegraph/`, `.superpowers/`, `scratchpad/`).
- Build outputs (`*.test`, `*.out`, `*.exe`), coverage and profile files (`coverage.*`, `*.cov`, `*.prof`, `*.pprof`), and raw audio captures.
