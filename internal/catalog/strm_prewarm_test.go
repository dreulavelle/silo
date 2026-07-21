package catalog

import (
	"context"
	"slices"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// preparePlaybackFiles is the single choke point every playable detail response
// flows through, on BOTH surfaces:
//
//	native  : api router -> DetailService.GetItemDetail
//	compat  : jellycompat HandleItem / HandlePlaybackInfo
//	          -> directContentService.GetItemDetail -> DetailService.GetItemDetail
//
// GetItemDetail -> buildMediaItemDetail / buildEpisodeDetail -> preparePlaybackFiles.
// So a placeholder opened or PlaybackInfo'd through the compat API on 8099 warms
// exactly as it does natively, provided the DetailService carries a prewarmer —
// which cmd/silo wires onto the compat DetailService from the same
// deps.ProbeEnsurer the native path uses. These tests pin that contract at the
// choke point so it cannot silently regress on either surface.
//
// Browse/list endpoints (jellycompat BrowseItems) do NOT reach this function, so
// a 300-episode season listing fires zero warms by construction — there is no
// call site on that path to test.

type recordingPrewarmer struct {
	warmed []int
}

func (r *recordingPrewarmer) Warm(file *models.MediaFile) {
	if file != nil {
		r.warmed = append(r.warmed, file.ID)
	}
}

type recordingEnsurer struct {
	ensured []int
}

func (r *recordingEnsurer) Ensure(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	if file != nil {
		r.ensured = append(r.ensured, file.ID)
	}
	return file, nil
}

// A placeholder is handed to the background prewarmer so its URL resolves ahead
// of play; a regular file is probed inline and never warmed. Both survive into
// the prepared set either way.
func TestPreparePlaybackFilesWarmsPlaceholdersOnly(t *testing.T) {
	warmer := &recordingPrewarmer{}
	ensurer := &recordingEnsurer{}
	s := &DetailService{}
	s.SetPlaceholderPrewarmer(warmer)
	s.SetProbeEnsurer(ensurer)

	files := []*models.MediaFile{
		{ID: 1, FilePath: "/lib/Movie (2024)/Movie (2024) [Bluray-2160p].strm"},
		{ID: 2, FilePath: "/lib/Movie (2024)/Movie (2024) [Bluray-1080p].mkv"},
	}

	out := s.preparePlaybackFiles(context.Background(), files)

	if len(out) != 2 {
		t.Fatalf("prepared %d files, want 2 (every version must survive)", len(out))
	}
	if want := []int{1}; !slices.Equal(warmer.warmed, want) {
		t.Errorf("warmed = %v, want %v (only the placeholder)", warmer.warmed, want)
	}
	if want := []int{2}; !slices.Equal(ensurer.ensured, want) {
		t.Errorf("inline-probed = %v, want %v (only the regular file; a placeholder must not block the response on a probe)", ensurer.ensured, want)
	}
}

// Without a prewarmer, a placeholder has to fall back to resolving inline — the
// warm is an optimisation, not the only path to metadata.
func TestPreparePlaybackFilesResolvesInlineWithoutPrewarmer(t *testing.T) {
	ensurer := &recordingEnsurer{}
	s := &DetailService{}
	s.SetProbeEnsurer(ensurer)

	files := []*models.MediaFile{
		{ID: 7, FilePath: "/lib/Show/S01E01 [WEBDL-1080p].strm"},
	}

	s.preparePlaybackFiles(context.Background(), files)

	if want := []int{7}; !slices.Equal(ensurer.ensured, want) {
		t.Errorf("inline-probed = %v, want %v (no prewarmer must fall back to inline resolve)", ensurer.ensured, want)
	}
}
