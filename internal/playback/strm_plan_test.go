package playback

import (
	"strconv"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// A placeholder normally reaches the planner with real metadata, because the
// playback probe path resolves it and probes the stream first. But that depends
// on a provider being reachable, and refusing to play because a scrape failed
// is a worse answer than playing conservatively.
func TestPlaceholderWithoutMetadataStillPlans(t *testing.T) {
	if !isPlaceholderSourceV3(&models.MediaFile{
		FilePath: "/library/movies/Title (2024) [tmdb-1]/Title (2024) [1080p].strm",
	}) {
		t.Error("a .strm path was not recognised as a placeholder")
	}
	// Some rows carry the marker in the container rather than the path.
	if !isPlaceholderSourceV3(&models.MediaFile{FilePath: "/x/y.mkv", Container: "strm"}) {
		t.Error("a strm container was not recognised as a placeholder")
	}
}

// The fallback must stay narrow. An ordinary file missing video metadata is a
// real problem, and quietly transcoding it at a guessed resolution would hide
// a broken probe behind a plausible-looking stream.
func TestOrdinaryFilesAreNotTreatedAsPlaceholders(t *testing.T) {
	for _, f := range []*models.MediaFile{
		nil,
		{FilePath: "/mnt/media/movies/The Matrix (1999)/The Matrix.mkv"},
		{FilePath: "/mnt/media/tv/Show/S01E01.mp4", Container: "mp4"},
		{FilePath: "/mnt/media/notes.strm.txt"},
	} {
		if isPlaceholderSourceV3(f) {
			path := "<nil>"
			if f != nil {
				path = f.FilePath
			}
			t.Errorf("%s was treated as a placeholder; a missing probe would be hidden", path)
		}
	}
}

// A placeholder resolves to a fresh URL every time. Keying the anchor on what
// it resolves to would make every key unique, defeating both the cache and the
// singleflight dedup that stops concurrent seeks probing the same thing twice.
func TestAnchorCacheKeyIsStableAcrossResolves(t *testing.T) {
	const path = "/library/movies/Title (2024) [tmdb-1]/Title (2024) [2160p].strm"
	a := copySeekAnchor{seconds: 1198.5, segment: 599}

	key := "ffprobe\x00" + path + "\x001200.000000\x002"
	storeAnchor(key, a)

	got, ok := lookupAnchor(key)
	if !ok {
		t.Fatal("anchor was not cached; every seek would re-probe the remote container")
	}
	if got.seconds != a.seconds || got.segment != a.segment {
		t.Errorf("cached anchor = %+v, want %+v", got, a)
	}
}

// An expired anchor must not be served: the placeholder may since have resolved
// to a different release, where that keyframe means nothing.
func TestAnchorCacheExpires(t *testing.T) {
	key := "expiring"
	anchorCacheMu.Lock()
	anchorCache[key] = cachedAnchor{
		anchor:  copySeekAnchor{seconds: 10},
		expires: time.Now().Add(-time.Second),
	}
	anchorCacheMu.Unlock()

	if _, ok := lookupAnchor(key); ok {
		t.Error("an expired anchor was served")
	}
}

// The key space is every position anyone has scrubbed to, which is unbounded.
func TestAnchorCacheIsBounded(t *testing.T) {
	anchorCacheMu.Lock()
	clear(anchorCache)
	anchorCacheMu.Unlock()

	for i := 0; i < anchorCacheMax*2; i++ {
		storeAnchor("k"+strconv.Itoa(i), copySeekAnchor{seconds: float64(i)})
	}

	anchorCacheMu.Lock()
	size := len(anchorCache)
	anchorCacheMu.Unlock()
	if size > anchorCacheMax {
		t.Errorf("cache holds %d entries, want at most %d", size, anchorCacheMax)
	}
}
