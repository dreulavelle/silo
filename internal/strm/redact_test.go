package strm

import (
	"strings"
	"testing"
)

// A resolved placeholder URL is a bearer credential: the path segment IS the
// debrid token. These reached logs three ways — the ffmpeg command line, the
// persisted per-session log store, and ffmpeg's own stderr banner.
func TestRedactRemovesTheTokenAndKeepsTheHost(t *testing.T) {
	const token = "75QWXRNXUNQPD3HCGRVK6YH63FQPALZWGZOHEY74BSLB"
	got := Redact("https://orionoid.com/stream/" + token)

	if strings.Contains(got, token) {
		t.Fatalf("the token survived redaction: %s", got)
	}
	// Host survives on purpose: knowing which provider served a stream is what
	// makes the log line worth reading, and it is not secret.
	if !strings.Contains(got, "orionoid.com") {
		t.Errorf("host was lost: %s", got)
	}
}

// Local paths are not credentials and must stay legible.
func TestRedactLeavesNonURLsAlone(t *testing.T) {
	for _, in := range []string{
		"/library/movies/Riddick (2013) [tmdb-87421]/Riddick (2013) [1080p].strm",
		"-hide_banner", "-i", "", "copy",
	} {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

// The argv is the first place the URL appears.
func TestRedactAllScrubsAnFFmpegCommandLine(t *testing.T) {
	args := []string{"-ss", "1200.000", "-i", "https://orionoid.com/stream/SECRETTOKEN", "-c:v", "copy"}
	got := strings.Join(RedactAll(args), " ")

	if strings.Contains(got, "SECRETTOKEN") {
		t.Fatalf("token survived: %s", got)
	}
	if !strings.Contains(got, "-ss 1200.000") || !strings.Contains(got, "-c:v copy") {
		t.Errorf("ordinary arguments were mangled: %s", got)
	}
}

// ffmpeg names its input in prose, so the URL has to be found inside a sentence.
func TestRedactLineScrubsFFmpegStderr(t *testing.T) {
	cases := []string{
		"[in#0 @ 0x55] Error opening input: https://orionoid.com/stream/SECRETTOKEN",
		"Input #0, mov,mp4, from 'https://orionoid.com/stream/SECRETTOKEN':",
		"two urls http://a.invalid/SECRETTOKEN and https://b.invalid/SECRETTOKEN here",
	}
	for _, line := range cases {
		got := RedactLine(line)
		if strings.Contains(got, "SECRETTOKEN") {
			t.Errorf("token survived in: %s", got)
		}
	}
}

// A line with nothing to redact must come back untouched, and must not hang.
func TestRedactLineTerminatesOnUnredactableInput(t *testing.T) {
	for _, line := range []string{
		"no urls here at all",
		"https://",
		"scheme-only http://",
		"Error opening input file /library/movies/A/A.strm.",
	} {
		if got := RedactLine(line); got != line {
			t.Errorf("RedactLine(%q) = %q, want it unchanged", line, got)
		}
	}
}
