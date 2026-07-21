package scanner

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/strm"
)

// A placeholder has no bytes to describe, but it must still record WHY it was
// not probed.
//
// This is the bug that broke playback: probeFile returns (nil, "placeholder"),
// and applyProbeData used to be reached only when probe was non-nil — so the
// marker was silently dropped and probe_source stayed empty. An empty
// probe_source is indistinguishable from "the probe has not run yet", which
// sent the repair loop after the file forever and left playback with no codec
// information, so it fell back to transcoding and handed ffmpeg a .strm.
func TestApplyProbeDataRecordsPlaceholderMarkerWithoutProbeData(t *testing.T) {
	var mf models.MediaFile
	applyProbeData(&mf, nil, strm.ProbeSourcePlaceholder)

	if mf.ProbeSource != strm.ProbeSourcePlaceholder {
		t.Errorf("ProbeSource = %q, want %q", mf.ProbeSource, strm.ProbeSourcePlaceholder)
	}
	if mf.ProbeUpdatedAt == nil {
		t.Error("ProbeUpdatedAt was not stamped; the row looks never-probed")
	}
	// Nothing may be invented about a file we deliberately did not open.
	if mf.CodecVideo != "" || mf.CodecAudio != "" || mf.Duration != 0 || mf.Bitrate != 0 {
		t.Errorf("fabricated media details for a placeholder: video=%q audio=%q duration=%v bitrate=%v",
			mf.CodecVideo, mf.CodecAudio, mf.Duration, mf.Bitrate)
	}
}

// The marker exists so the repair path can tell "deliberately not probed" from
// "probe failed". If those ever collapse, placeholders get re-probed on every
// scan: a self-inflicted resolution storm against the upstream provider.
func TestPlaceholderMarkerIsDistinctFromAFailedProbe(t *testing.T) {
	if strm.ProbeSourcePlaceholder == "" {
		t.Fatal("the placeholder marker must not be empty; empty means 'never probed'")
	}
	if strm.ProbeSourcePlaceholder == "local" {
		t.Fatal("the placeholder marker must not collide with the local-probe source")
	}
}

// probeFile must never shell out to ffprobe for a placeholder. Doing so reaches
// the resolver from inside a library scan, turning a full scan into a
// resolution storm — and ffprobe would fail on the .strm anyway.
func TestProbeFileSkipsPlaceholdersEntirely(t *testing.T) {
	// An ffprobe path that would fail loudly if it were ever executed.
	s := &Scanner{ffprobePath: "/nonexistent/ffprobe-must-not-run"}

	probe, source := s.probeFile(t.Context(), "/library/movies/Title (2024) [tmdb-1]/Title (2024) [1080p].strm")
	if probe != nil {
		t.Error("a placeholder produced probe data; it has no bytes to probe")
	}
	if source != strm.ProbeSourcePlaceholder {
		t.Errorf("probe source = %q, want %q", source, strm.ProbeSourcePlaceholder)
	}
}
