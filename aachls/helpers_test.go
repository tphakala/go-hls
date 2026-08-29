package aachls

import "time"

// Shared test fixtures for the aachls package. These mirror constants and
// helpers that live in go-hls's own test files; they are duplicated here
// because a test in one package cannot see another package's test scope.
const (
	testRate     = 48000
	testChannels = 1
	testBitrate  = 128
	aacFrame     = 1024

	// bytesPerSample is the width of one 16-bit PCM sample of one channel.
	bytesPerSample = 2
)

// testEpoch is a fixed wall-clock origin so PDT assertions are exact.
var testEpoch = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

// sampleTime returns the wall-clock time of sample n at testRate.
func sampleTime(n int) time.Time {
	return testEpoch.Add(time.Duration(n) * time.Second / testRate)
}

// silence returns samples worth of interleaved 16-bit PCM for channels.
func silence(samples, channels int) []byte {
	return make([]byte, samples*channels*bytesPerSample)
}
