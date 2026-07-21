package strm

import (
	"net/url"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// resolvedCacheTTL bounds how long a resolved stream URL is reused without
// asking the plugin again.
//
// A resolve is a provider scrape plus one or more host-local hops — 1 to 4
// seconds a viewer waits through. Without this, every ffmpeg launch pays it
// again: a seek restart, the keyframe-anchor probe that precedes a copy-video
// seek (which resolves, then the restart resolves again), and a stall loop that
// restarts every ten-odd seconds. Sixty seconds is short enough that a rewritten
// placeholder or an expired link corrects itself within a breath, and long
// enough that a burst of restarts around one seek shares a single resolve.
const resolvedCacheTTL = 60 * time.Second

// resolvedCacheMax bounds the cache. A resolved URL is small, but the key space
// is every placeholder anyone has played, which is unbounded. The eviction
// policy mirrors the copy-seek anchor cache: sweep expired first, then drop the
// soonest-to-expire — never empty wholesale, because each miss is a scrape.
const resolvedCacheMax = 256

// freshMarkerTTL bounds how long a path stays armed for a fresh re-resolve after
// a failure. A marker is consumed by the next successful resolve, so this only
// matters when the failed session is never retried; five minutes is long enough
// to cover a crash-teardown-then-reconstruct and short enough that a stale
// marker cannot make a much-later ordinary resolve needlessly fresh.
const freshMarkerTTL = 5 * time.Minute

// freshQueryParam is appended to a placeholder target on a fresh resolve. The
// resolver plugin reads it as "the URL you last issued for this item just
// failed — bypass your reuse window and probe memory rather than handing the
// same dead link back". It is set on the first resolve after an ffmpeg error
// exit (see InvalidateResolved), and only for host-local plugin routes: an
// external direct-CDN target must reach ffmpeg byte-for-byte or a signed URL's
// signature breaks.
const freshQueryParam = "fresh"

type cachedResolution struct {
	// target is the placeholder's contents at the time this URL was resolved.
	// A placeholder is rewritable in place (see ReadTarget), so a cache hit is
	// only valid while the file still points where it did — otherwise a rewrite
	// would be masked for the whole TTL. Comparing the freshly-read target on
	// lookup keeps the rewrite-in-place guarantee that ReadTarget exists for.
	target  string
	url     string
	expires time.Time
}

var (
	resolvedMu sync.Mutex
	// resolvedCache maps a placeholder file path to its last resolved URL.
	// Keyed on the path, NOT the resolved URL, so a seek restart and the
	// keyframe probe that precedes it — both handed the same .strm path — share
	// one entry.
	resolvedCache = make(map[string]cachedResolution, resolvedCacheMax)
	// freshPending maps a path to the time its fresh-resolve arming expires. A
	// path lands here when its ffmpeg process died on an error the resolved URL
	// is the likely cause of; the entry is cleared once a fresh resolve succeeds
	// (see commitResolved) or when its TTL lapses. It is deliberately NOT under
	// the cache's count-based eviction: an armed marker is the failure-recovery
	// guarantee, and a burst of unrelated failures must not silently drop it.
	freshPending = make(map[string]time.Time)
	// resolveGen counts invalidations. A resolve reads it (together with the
	// fresh flag, under one lock) when its flight begins and its result is cached
	// only if it has not advanced since — an invalidation mid-flight means the
	// URL the flight fetched is the one just implicated, so a slow ordinary
	// flight must not clobber the fresh entry that replaced it. A single global
	// counter (rather than per-path) can make an unrelated in-flight resolve skip
	// one cache write during the rare window of a crash; that only costs a
	// re-resolve next time and never serves a stale URL, which is the trade we
	// want.
	resolveGen uint64
	// resolveGroup coalesces concurrent resolves of the same path+target so a
	// stall storm — many segment requests arriving while a session is down —
	// costs one scrape, not one per request.
	resolveGroup singleflight.Group
)

// loadResolveState reports, atomically, whether path's next resolve must go out
// fresh and the invalidation generation to carry from flight start to store.
//
// Reading both under one lock is load-bearing: taken separately, an invalidation
// landing between them could let a resolve run WITHOUT fresh=1 yet still cache
// its result under the new generation, defeating the recovery the invalidation
// armed.
func loadResolveState(path string) (fresh bool, gen uint64) {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	exp, ok := freshPending[path]
	return ok && time.Now().Before(exp), resolveGen
}

// lookupResolved returns a live cached URL for a placeholder path, but only if
// the placeholder still points at target — a rewritten placeholder is a miss.
func lookupResolved(path, target string) (string, bool) {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	got, ok := resolvedCache[path]
	if !ok || got.target != target || time.Now().After(got.expires) {
		return "", false
	}
	return got.url, true
}

// commitResolved caches a resolved URL and, for a fresh resolve, clears the
// path's arming — but only if no invalidation has landed since the flight read
// gen. If one has, the URL this flight carries is the one that failure
// implicated: the write is dropped and the marker left armed so the next resolve
// is fresh too.
//
// Room is made by dropping expired entries first, then the soonest-to-expire —
// the same policy as storeAnchor, and for the same reason: this cache holds only
// placeholders, where a miss is a provider scrape, so emptying it wholesale
// would make every active viewer re-resolve at once.
func commitResolved(path, target, resolved string, gen uint64, fresh bool) {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()

	if resolveGen != gen {
		return
	}

	if _, exists := resolvedCache[path]; !exists && len(resolvedCache) >= resolvedCacheMax {
		now := time.Now()
		for k, v := range resolvedCache {
			if now.After(v.expires) {
				delete(resolvedCache, k)
			}
		}
		if len(resolvedCache) >= resolvedCacheMax {
			var oldestKey string
			var oldest time.Time
			for k, v := range resolvedCache {
				if oldestKey == "" || v.expires.Before(oldest) {
					oldestKey, oldest = k, v.expires
				}
			}
			delete(resolvedCache, oldestKey)
		}
	}

	resolvedCache[path] = cachedResolution{
		target:  target,
		url:     resolved,
		expires: time.Now().Add(resolvedCacheTTL),
	}
	if fresh {
		delete(freshPending, path)
	}
}

// InvalidateResolved drops the cached URL for a placeholder and arms the next
// resolve to go out fresh — but only if the cache still holds the URL usedURL
// that the failing session actually played.
//
// It is called when an ffmpeg process reading this source exits on an error the
// resolved URL is the likely cause of — an expired debrid link, a provider that
// rotated the stream. The usedURL guard matters because invalidation is keyed by
// path: a stale session dying on URL A must not evict a newer URL B that another
// session already re-resolved and is playing happily. When the cache has moved
// on, B is not implicated, so the entry is left and no fresh marker is armed. An
// empty cache (the entry aged out) is not a contradiction, so it still arms.
//
// Dropping the entry alone would send the next resolve back to the plugin, but
// within the plugin's own reuse window it would hand back the same dead URL;
// arming fresh=1 tells it not to. The pairing guarantees the "error exit -> next
// resolve is fresh" sequence holds however the next resolve is reached (a
// restart, or a crash-teardown followed by a reconstruct).
func InvalidateResolved(path, usedURL string) {
	if !IsPlaceholderPath(path) {
		return
	}
	resolvedMu.Lock()
	defer resolvedMu.Unlock()

	if entry, ok := resolvedCache[path]; ok && entry.url != usedURL {
		return
	}
	delete(resolvedCache, path)

	// Advance the generation so any resolve already in flight — including a slow
	// ordinary one racing the fresh re-resolve this arms — is barred from caching
	// the URL it is about to return.
	resolveGen++

	// Markers expire on their own clock, not by count, so a live one is never
	// evicted by unrelated failures. Sweep the lapsed ones opportunistically
	// here, the only place that grows the map.
	now := time.Now()
	for k, exp := range freshPending {
		if now.After(exp) {
			delete(freshPending, k)
		}
	}
	freshPending[path] = now.Add(freshMarkerTTL)
}

// appendFreshParam adds fresh=1 to a host-local plugin-route target's query.
//
// Only ever called for a target isHostLocalPluginRoute has already accepted, so
// it is always our own plugin's URL — never a signed external CDN link, whose
// query must not be touched. Set (not add) so a target that already carries the
// parameter is not duplicated, and so a retry of a retry stays idempotent.
func appendFreshParam(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	q := u.Query()
	q.Set(freshQueryParam, "1")
	u.RawQuery = q.Encode()
	return u.String()
}
