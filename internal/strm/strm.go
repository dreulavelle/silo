// Package strm implements on-demand playback backed by .strm placeholder files.
//
// A .strm file is a small text file whose contents are a URL. It carries no
// media bytes, so there is nothing to probe at scan time and nothing to open at
// play time. Instead the scanner records the item as a placeholder and playback
// resolves the URL at the moment a user presses play.
//
// This package is fork-owned. Everything on-demand lives here so that the edits
// to upstream files stay small enough to rebase indefinitely. See FORK.md.
package strm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Extension is the file extension that marks a placeholder media file.
const Extension = ".strm"

// ProbeSourcePlaceholder is the probe_source value recorded for placeholder
// rows. It distinguishes "deliberately not probed" from "probe failed", which
// matters because the scanner's repair path treats an empty probe_source as
// damage to be fixed.
const ProbeSourcePlaceholder = "placeholder"

// maxFileSize caps how much of a .strm file we will read. A .strm holds a
// single URL; anything larger is a mistake or an attack, and we would rather
// fail than stream an arbitrary file into memory during a library scan.
const maxFileSize = 64 << 10 // 64 KiB

// allowedSchemes restricts what a .strm may point at.
//
// This is a security control, not a convenience. Jellyfin shipped .strm without
// scheme validation and it became the arbitrary-file-read primitive in an RCE
// chain (CVE-2026-35031 / GHSA-j2hf-x4q5-47j3, CVSS 9.9): a .strm containing a
// local path or a file:// URL let an attacker read anything the server process
// could read. Placeholders address remote streams; a local path is always
// either a mistake or an attack.
var allowedSchemes = map[string]bool{
	"http":  true,
	"https": true,
	"rtsp":  true,
	"rtp":   true,
}

// ErrNotPlaceholder is returned when a path is not a .strm file.
var ErrNotPlaceholder = errors.New("strm: not a placeholder file")

// ErrEmpty is returned when a .strm file contains no usable URL line.
var ErrEmpty = errors.New("strm: file contains no target URL")

// InvalidSchemeError reports a .strm target that resolved to a disallowed
// scheme. It is deliberately a distinct type so callers can log it loudly:
// in normal operation it should never happen, and when it does it is either
// misconfiguration or an attempt at local file disclosure.
type InvalidSchemeError struct {
	Scheme string
	Target string
}

func (e *InvalidSchemeError) Error() string {
	if e.Scheme == "" {
		return fmt.Sprintf("strm: target %q has no scheme (only remote URLs are allowed)", e.Target)
	}
	return fmt.Sprintf("strm: target scheme %q is not allowed (want http, https, rtsp, or rtp)", e.Scheme)
}

// IsPlaceholderPath reports whether a path names a .strm placeholder.
//
// This is the single predicate the rest of the fork branches on. It is a pure
// path check by design: it must be callable from the scanner's hot loop without
// touching the filesystem, and it must agree with the scanner's extension
// allowlist.
func IsPlaceholderPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), Extension)
}

// ReadTarget reads a .strm file and returns its validated target URL.
//
// The file is re-read on every call rather than cached. That is deliberate: it
// is what makes a placeholder rewritable in place. Whatever wrote the file can
// change where it points at any time, and the next playback picks it up with no
// rescan, no metadata refresh, and no mtime games.
func ReadTarget(path string) (string, error) {
	if !IsPlaceholderPath(path) {
		return "", ErrNotPlaceholder
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("strm: open %s: %w", path, err)
	}
	defer f.Close()

	raw, err := parseTarget(io.LimitReader(f, maxFileSize))
	if err != nil {
		return "", fmt.Errorf("strm: %s: %w", path, err)
	}
	return raw, nil
}

// parseTarget extracts the first usable URL from .strm contents.
//
// The format is conventional rather than specified: one URL per file, with '#'
// comment lines tolerated because players in this lineage accept M3U-flavoured
// headers such as #EXTINF. We take the first non-blank, non-comment line.
func parseTarget(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), maxFileSize)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := ValidateTarget(line); err != nil {
			return "", err
		}
		return line, nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return "", ErrEmpty
}

// ValidateTarget reports whether a .strm target is a remote URL we will follow.
//
// Exported so that anything writing placeholders can reject a bad target at
// write time rather than discovering it at play time.
func ValidateTarget(target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return &InvalidSchemeError{Target: target}
	}
	scheme := strings.ToLower(u.Scheme)
	if !allowedSchemes[scheme] {
		return &InvalidSchemeError{Scheme: scheme, Target: target}
	}
	// A scheme alone is not enough: "http:///path" parses with an empty host
	// and would send us at ourselves.
	if u.Host == "" {
		return &InvalidSchemeError{Scheme: scheme, Target: target}
	}
	return nil
}
