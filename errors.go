package hls

import "errors"

// ErrClosed reports a Write against a stream whose Close has already begun.
// Nothing is wrong with the stream's output; teardown simply won the race.
// Write returns it, so test with errors.Is.
var ErrClosed = errors.New("hls: stream is closed")

// ErrFailed reports that a failure has latched the stream unusable: the
// encoder or the container rejected audio, so the published timeline can no
// longer be reconciled with the audio that was consumed. It is wrapped into
// the error returned by the Write that latches the stream and returned by
// every later Write. A caller seeing it (test with errors.Is) must tear the
// stream down and build a new one; Stats().Failed reports the same condition.
var ErrFailed = errors.New("hls: stream has failed")
