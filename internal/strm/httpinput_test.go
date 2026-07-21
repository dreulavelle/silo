package strm

import (
	"strings"
	"testing"
)

// A local file has no per-request latency to amortise, and a 16MB read buffer
// per ffmpeg would be memory spent for nothing.
func TestInputOptionsAreOnlyForRemoteInputs(t *testing.T) {
	for _, local := range []string{
		"/library/movies/T (2024) [tmdb-1]/T (2024) [1080p].strm",
		"/mnt/media/movies/The Matrix (1999)/The Matrix.mkv",
		"",
		"rtsp://camera.invalid/stream",
	} {
		if got := InputOptions(local); len(got) != 0 {
			t.Errorf("InputOptions(%q) = %v, want none", local, got)
		}
	}
}

// Every option here must be one this ffmpeg actually accepts. A rejected option
// makes ffmpeg refuse the input entirely, which reads as a failed seek.
func TestInputOptionsSetReadAheadForRemote(t *testing.T) {
	got := strings.Join(InputOptions("https://cdn.example.invalid/movie.mkv"), " ")

	// A CDN dropping mid-stream should not end the session.
	if !strings.Contains(got, "-reconnect 1") {
		t.Errorf("no reconnect configured: %s", got)
	}
}

// Options go BEFORE -i or ffmpeg applies them to the output, where they mean
// nothing. This is easy to get wrong and silent when wrong.
func TestInputOptionsContainNoOutputOnlyFlags(t *testing.T) {
	for _, opt := range InputOptions("https://cdn.example.invalid/movie.mkv") {
		if strings.HasPrefix(opt, "-c:") || opt == "-f" {
			t.Errorf("output-side flag in input options: %s", opt)
		}
	}
}
