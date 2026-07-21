package strm

import (
	"net/url"
	"strings"
)

// Redact replaces the secret-bearing part of a URL with a marker, keeping
// enough to be useful in a log.
//
// A resolved placeholder URL is a bearer credential: the path segment IS the
// debrid token, and anyone holding it can stream against the operator's
// account. These URLs reach logs three ways — the ffmpeg command line, the
// per-session ffmpeg log store, and ffmpeg's own stderr banner, which prints
// "Input #0, ... from '<url>'". All three are durable, so a resolved URL must
// be redacted before any of them, not after.
//
// Scheme and host survive on purpose: knowing a stream came from one provider
// rather than another is exactly what makes a log entry worth reading, and
// neither is secret.
func Redact(s string) string {
	if s == "" {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return s
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return s
	}
	return u.Scheme + "://" + u.Host + "/<redacted>"
}

// RedactAll redacts every URL-looking token in a slice, returning a copy.
// Used for an ffmpeg argv, where the input URL is one argument among many.
func RedactAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = Redact(a)
	}
	return out
}

// RedactLine redacts URLs embedded in a line of free text.
//
// ffmpeg's stderr names its input in prose — "Input #0, mov,mp4, from
// 'https://host/TOKEN':" — so the URL has to be found inside the sentence
// rather than parsed as a whole.
//
// The scan walks forward and never re-examines what it has emitted. Rewriting
// in place and re-scanning does not terminate: the replacement contains "<",
// which is itself a URL terminator, so the marker gets re-matched and re-
// redacted forever.
func RedactLine(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		scheme := schemeAt(line, i)
		if scheme == "" {
			b.WriteByte(line[i])
			i++
			continue
		}
		end := i + len(scheme)
		for end < len(line) && !isURLTerminator(line[end]) {
			end++
		}
		b.WriteString(Redact(line[i:end]))
		i = end
	}
	return b.String()
}

// schemeAt returns the URL scheme starting at i, or "".
func schemeAt(line string, i int) string {
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(line[i:], scheme) {
			return scheme
		}
	}
	return ""
}

// isURLTerminator reports whether a byte ends a URL inside prose. Quotes and
// whitespace delimit ffmpeg's own formatting; the rest are characters a URL
// cannot contain unescaped.
func isURLTerminator(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\'', '"', '`', '<', '>', ',', ';':
		return true
	}
	return false
}
