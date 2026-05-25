package metadata

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestRefreshDebtReasonsForItem(t *testing.T) {
	item := &models.MediaItem{
		Status:                    "matched",
		TmdbID:                    "123",
		EpisodeMetadataIncomplete: true,
		RefreshFailures:           2,
	}

	mask := refreshDebtReasonsForItem(item)
	if hasRefreshDebtReason(mask, RefreshDebtReasonEpisodeIncomplete) {
		t.Fatalf("expected episode incomplete reason to stay on episode targets, got mask %d", mask)
	}
	if !hasRefreshDebtReason(mask, RefreshDebtReasonRefreshFailure) {
		t.Fatalf("expected refresh failure reason in mask %d", mask)
	}
	if !hasRefreshDebtReason(mask, RefreshDebtReasonCoreMetadataIncomplete) {
		t.Fatalf("expected core metadata reason in mask %d", mask)
	}
}

func TestRefreshDebtReasonsForItemSkipsUnmatchedFailureOnly(t *testing.T) {
	item := &models.MediaItem{
		Status:          "pending",
		RefreshFailures: 1,
	}

	mask := refreshDebtReasonsForItem(item)
	if mask != 0 {
		t.Fatalf("expected no scheduled refresh debt for unmatched item, got %d", mask)
	}
}

func TestRefreshDebtReasonsForItemFlagsMissingTMDBWithOtherProviderIDs(t *testing.T) {
	item := &models.MediaItem{
		Type:   "series",
		Status: "matched",
		TvdbID: "420105",
		ImdbID: "tt18076310",
		TmdbID: "",
	}

	mask := refreshDebtReasonsForItem(item)
	if !hasRefreshDebtReason(mask, RefreshDebtReasonProviderIDIncomplete) {
		t.Fatalf("reason mask = %d, want provider id incomplete", mask)
	}
}

func TestRefreshDebtReasonsForItemDoesNotFlagProviderIDIncompleteWithoutAlternateIDs(t *testing.T) {
	item := &models.MediaItem{
		Type:   "series",
		Status: "matched",
		TmdbID: "",
	}

	mask := refreshDebtReasonsForItem(item)
	if hasRefreshDebtReason(mask, RefreshDebtReasonProviderIDIncomplete) {
		t.Fatalf("reason mask = %d, did not want provider id incomplete", mask)
	}
}

func TestNextRefreshDelayEpisodeSchedule(t *testing.T) {
	reasonMask := RefreshDebtReasonEpisodeIncomplete
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: 24 * time.Hour},
		{attempts: 1, want: 24 * time.Hour},
		{attempts: 2, want: 3 * 24 * time.Hour},
		{attempts: 3, want: 7 * 24 * time.Hour},
		{attempts: 4, want: 14 * 24 * time.Hour},
		{attempts: 5, want: 30 * 24 * time.Hour},
	}

	for _, tc := range cases {
		if got := nextRefreshDelay(reasonMask, tc.attempts); got != tc.want {
			t.Fatalf("attempts=%d delay=%s want %s", tc.attempts, got, tc.want)
		}
	}
}
