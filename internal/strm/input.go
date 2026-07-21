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
// The resolved URL is cached for a short window keyed by the placeholder path
// (see cache.go). Every ffmpeg launch for a session — the initial start, a seek
// restart, the keyframe probe a copy-video seek runs first, a restart-on-stall
// loop — passes through here, and without the cache each pays the full
// scrape-and-hop latency again. A path armed by InvalidateResolved resolves
// fresh instead, so a failed link is not served back from cache.
//
// Callers pass every input path through this. Non-placeholders are returned
// untouched, so the ordinary local-file case costs one extension comparison.
func ResolveFileForInput(ctx context.Context, path string) (string, error) {
	if !IsPlaceholderPath(path) {
		return path, nil
	}

	// The .strm is re-read every time, deliberately: it is what makes a
	// placeholder rewritable in place (see ReadTarget). The cache keys on the
	// path but stores the target it resolved, so a rewrite is detected here and
	// treated as a miss rather than masked for the TTL.
	target, err := ReadTarget(path)
	if err != nil {
		return "", fmt.Errorf("strm: read placeholder %s: %w", path, err)
	}
	if err := ValidateTarget(target); err != nil {
		return "", err
	}

	// Read fresh state and the invalidation generation together, once, so an
	// invalidation cannot slip between them and let an ordinary resolve cache a
	// non-fresh result under the new generation.
	fresh, gen := loadResolveState(path)
	if !fresh {
		if resolved, ok := lookupResolved(path, target); ok {
			return resolved, nil
		}
	}

	return resolveAndCache(ctx, path, target, fresh, gen)
}

// resolveAndCache performs the resolve behind a singleflight so concurrent
// callers for the same path+target share one hop, then caches the result.
func resolveAndCache(ctx context.Context, path, target string, fresh bool, gen uint64) (string, error) {
	// Key on the target as well as the path. ReadTarget runs on every call, so a
	// caller that read a rewritten placeholder must not join a flight resolving
	// the old target and be handed its URL. A fresh resolve is keyed apart too,
	// or a stall storm racing a crash-armed retry could hand the fresh caller the
	// stale URL the ordinary flight is about to cache.
	key := path + "\x00" + target
	if fresh {
		key += "\x00fresh"
	}

	ch := resolveGroup.DoChan(key, func() (any, error) {
		// Another flight may have filled the cache between our miss and here.
		if !fresh {
			if resolved, ok := lookupResolved(path, target); ok {
				return resolved, nil
			}
		}

		// fresh=1 is only meaningful to — and only safe to append to — our own
		// plugin route. An external direct-CDN target reaches ffmpeg untouched so
		// a signed URL's signature survives.
		resolveTarget := target
		if fresh && isHostLocalPluginRoute(target) {
			resolveTarget = appendFreshParam(target)
		}

		// Detach from the winning caller's cancellation so a single caller
		// walking away does not fail the resolve for everyone sharing this
		// flight; ResolveTarget stays bounded by internalResolveTimeout. Each
		// caller still abandons on its own ctx via the select below.
		resolved, err := ResolveTarget(context.WithoutCancel(ctx), resolveTarget)
		if err != nil {
			return "", err
		}

		// gen was captured with fresh at flight start; commitResolved drops the
		// write (and leaves the marker armed) if an invalidation landed since.
		commitResolved(path, target, resolved, gen, fresh)
		return resolved, nil
	})

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		return res.Val.(string), nil
	}
}
