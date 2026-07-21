package playback

import (
	"strconv"
	"strings"
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

// StartTranscode stores the RESOLVED opts on the session and restart() reads
// them back. Resolving from InputPath therefore meant the second launch saw an
// https:// URL, decided it was not a placeholder, and reused the link minted at
// session start — so a seek after that link expired failed permanently. The
// function's own comment claimed the opposite.
func TestResolvePlaceholderInputResolvesFromTheOriginalPath(t *testing.T) {
	const placeholder = "/library/movies/T (2024) [tmdb-1]/T (2024) [1080p].strm"

	// First launch: as StartTranscode does it.
	first := TranscodeOpts{InputPath: placeholder}
	first.SourcePath = placeholder
	if first.SourcePath != placeholder {
		t.Fatal("setup")
	}

	// Second launch: as restart() does it, from opts whose InputPath is already
	// the resolved URL. The original must survive so it can be resolved again.
	afterFirst := TranscodeOpts{
		SourcePath: placeholder,
		InputPath:  "https://cdn.example.invalid/expired-token",
	}
	if afterFirst.SourcePath != placeholder {
		t.Errorf("SourcePath = %q, want the placeholder; restart would reuse the expired link",
			afterFirst.SourcePath)
	}
}

// A resolved URL must never be what gets logged: it is a bearer credential.
func TestLoggableInputPrefersThePlaceholderPath(t *testing.T) {
	const placeholder = "/library/movies/T (2024) [tmdb-1]/T (2024) [1080p].strm"
	s := &TranscodeSession{opts: TranscodeOpts{
		SourcePath: placeholder,
		InputPath:  "https://orionoid.com/stream/SECRETTOKEN",
	}}

	if got := s.loggableInputLocked(); got != placeholder {
		t.Errorf("logged %q, want the placeholder path", got)
	}

	// With no original recorded it must still not log the raw URL.
	s2 := &TranscodeSession{opts: TranscodeOpts{InputPath: "https://orionoid.com/stream/SECRETTOKEN"}}
	if got := s2.loggableInputLocked(); strings.Contains(got, "SECRETTOKEN") {
		t.Errorf("logged a credential: %s", got)
	}
}
