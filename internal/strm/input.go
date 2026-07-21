package strm

import (
	"context"
	"fmt"
)

// ResolveFileForInput turns a placeholder file into a URL ffmpeg can open.
//
// Direct play answers a placeholder with a redirect and never touches the
// stream itself. Transcoding cannot: ffmpeg has to read the actual bytes, and
// handing it the .strm file gets "Invalid data found when processing input" —
// a .strm is a media-server convention, not a container format, and ffmpeg has
// never heard of it.
//
// So the placeholder is resolved here and ffmpeg is given the resulting URL.
// That is a real capability rather than a workaround: ffmpeg speaks HTTP with
// range requests, so seeking within a transcoded remote stream works the same
// as it would against a local file.
//
// Callers pass every input path through this. Non-placeholders are returned
// untouched, so the ordinary local-file case costs one extension comparison.
func ResolveFileForInput(ctx context.Context, path string) (string, error) {
	if !IsPlaceholderPath(path) {
		return path, nil
	}

	target, err := ReadTarget(path)
	if err != nil {
		return "", fmt.Errorf("strm: read placeholder %s: %w", path, err)
	}
	if err := ValidateTarget(target); err != nil {
		return "", err
	}

	// A placeholder written by a resolver plugin points at that plugin's route
	// on this host, which ffmpeg could technically open — but then ffmpeg would
	// be following the redirect itself, and its redirect handling is the thing
	// the 302-not-301 rule exists to work around. Resolving here keeps that
	// behaviour in one place and hands ffmpeg a URL that is already final.
	resolved, err := ResolveTarget(ctx, target)
	if err != nil {
		return "", err
	}
	return resolved, nil
}
