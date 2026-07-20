package strm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsPlaceholderPath(t *testing.T) {
	cases := map[string]bool{
		"/library/Show/S01E01.strm": true,
		"/library/Show/S01E01.STRM": true, // case-insensitive: scanners see both
		"/library/Show/S01E01.mkv":  false,
		"/library/Show/strm":        false, // bare name, not an extension
		"/library/Show/x.strm.mkv":  false, // real media that merely mentions strm
		"":                          false,
	}
	for path, want := range cases {
		if got := IsPlaceholderPath(path); got != want {
			t.Errorf("IsPlaceholderPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestReadTarget(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"bare url", "https://example.com/a.mkv", "https://example.com/a.mkv"},
		{"trailing newline", "https://example.com/a.mkv\n", "https://example.com/a.mkv"},
		{"crlf", "https://example.com/a.mkv\r\n", "https://example.com/a.mkv"},
		{"surrounding whitespace", "  https://example.com/a.mkv  \n", "https://example.com/a.mkv"},
		{"leading blank lines", "\n\n\nhttps://example.com/a.mkv\n", "https://example.com/a.mkv"},
		{
			"m3u style comments are skipped",
			"#EXTM3U\n#EXTINF:-1,Title\nhttps://example.com/a.mkv\n",
			"https://example.com/a.mkv",
		},
		{
			"first url wins",
			"https://example.com/first.mkv\nhttps://example.com/second.mkv\n",
			"https://example.com/first.mkv",
		},
		{"rtsp", "rtsp://example.com/stream", "rtsp://example.com/stream"},
		{"query preserved", "https://e.com/a?token=x&y=1", "https://e.com/a?token=x&y=1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeStrm(t, tt.content)
			got, err := ReadTarget(path)
			if err != nil {
				t.Fatalf("ReadTarget() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ReadTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The security property this fork must not regress. A .strm is attacker-
// influenced input in any setup where requests create placeholders, so a
// local path must never resolve. See CVE-2026-35031.
func TestReadTargetRejectsNonRemoteSchemes(t *testing.T) {
	hostile := []string{
		"/etc/passwd",
		"file:///etc/passwd",
		"FILE:///etc/shadow",
		"../../../../etc/passwd",
		"C:\\Windows\\System32\\config\\SAM",
		"\\\\smb-host\\share\\secret",
		"gopher://example.com/",
		"ftp://example.com/a.mkv",
		"data:text/plain;base64,aGk=",
		"javascript:alert(1)",
		"http:///no-host",
		"https://",
	}

	for _, target := range hostile {
		t.Run(target, func(t *testing.T) {
			path := writeStrm(t, target)
			got, err := ReadTarget(path)
			if err == nil {
				t.Fatalf("ReadTarget(%q) unexpectedly succeeded, returning %q", target, got)
			}
			var schemeErr *InvalidSchemeError
			if !errors.As(err, &schemeErr) {
				t.Fatalf("ReadTarget(%q) error = %v, want *InvalidSchemeError", target, err)
			}
		})
	}
}

func TestReadTargetEmptyFile(t *testing.T) {
	for _, content := range []string{"", "\n\n", "#EXTM3U\n#only comments\n", "   \n\t\n"} {
		path := writeStrm(t, content)
		if _, err := ReadTarget(path); !errors.Is(err, ErrEmpty) {
			t.Errorf("ReadTarget(%q) error = %v, want ErrEmpty", content, err)
		}
	}
}

func TestReadTargetRejectsNonPlaceholder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("https://example.com/a.mkv"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTarget(path); !errors.Is(err, ErrNotPlaceholder) {
		t.Errorf("ReadTarget() error = %v, want ErrNotPlaceholder", err)
	}
}

func TestReadTargetMissingFile(t *testing.T) {
	if _, err := ReadTarget(filepath.Join(t.TempDir(), "nope.strm")); err == nil {
		t.Error("ReadTarget() on a missing file unexpectedly succeeded")
	}
}

// A .strm should never be large. Reading an oversized file must fail rather
// than pull it into memory during a scan.
func TestReadTargetBoundsFileSize(t *testing.T) {
	// No newline, so the scanner cannot find a line within the limit.
	path := writeStrm(t, strings.Repeat("A", maxFileSize+1024))
	if _, err := ReadTarget(path); err == nil {
		t.Error("ReadTarget() on an oversized file unexpectedly succeeded")
	}
}

// A placeholder's whole value is that rewriting it changes where it points,
// with no rescan and no cache to invalidate.
func TestReadTargetReflectsRewrites(t *testing.T) {
	path := writeStrm(t, "https://example.com/first.mkv")

	got, err := ReadTarget(path)
	if err != nil || got != "https://example.com/first.mkv" {
		t.Fatalf("first read = %q, %v", got, err)
	}

	if err := os.WriteFile(path, []byte("https://example.com/second.mkv"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err = ReadTarget(path)
	if err != nil {
		t.Fatalf("second read error = %v", err)
	}
	if got != "https://example.com/second.mkv" {
		t.Errorf("after rewrite = %q, want the new target", got)
	}
}

func writeStrm(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "item.strm")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
