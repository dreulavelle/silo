package playback

import (
	"testing"

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
