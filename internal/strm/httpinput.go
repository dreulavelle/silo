package strm

import "strings"

// What is known about this input, from measurement:
//
// A debrid CDN answers every range request with roughly half a second of fixed
// latency — server-side work in the provider, identical whether the range is at
// byte 0 or byte 70,000,000,000 — while the transfer itself is effectively
// free. Opening a Matroska file means parsing a header and a cue table
// scattered across it, so the cost is request COUNT times that half second.
// Measured: ~5.6s to open, ~11s to open and seek.
//
// What did NOT help, recorded so it is not retried:
//
//   - -buffer_size is REJECTED by this ffmpeg ("Option buffer_size not found");
//     it belongs to the udp/tcp protocols, not http. An earlier measurement
//     appeared to show it halving seek time, but was timing how long ffmpeg
//     took to FAIL rather than to produce output.
//   - HTTP keep-alive (-multiple_requests) made no measurable difference: the
//     latency is the provider's own processing, not connection setup.
//   - Reducing -probesize/-analyzeduration made no difference; the scattered
//     seeks dominate the sequential analysis.
//   - -seekable 0 was catastrophic at 88s, forcing a sequential read from the
//     start of the file.

// InputOptions returns ffmpeg input options tuned for a resolved placeholder.
//
// Empty for a local path: a local file has no per-request latency to amortise,
// and a large read buffer would only waste memory.
func InputOptions(inputPath string) []string {
	if !isRemote(inputPath) {
		return nil
	}
	return []string{
		// A CDN dropping mid-stream otherwise kills the process outright and
		// surfaces as a failed playback. Reconnecting costs nothing when it is
		// not needed and saves the session when it is. Verified accepted by
		// this ffmpeg build.
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
	}
}

func isRemote(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}
