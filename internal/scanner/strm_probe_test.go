package scanner

import (
	"testing"
	"time"

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

// A placeholder is repaired by resolving it and probing the remote stream: a
// scrape plus an HTTP read. Affordable once, when someone opens the title;
// ruinous on every press of play. Once it carries what playback plans from, it
// must stop asking.
func TestPlaceholderStopsNeedingRepairOnceProbed(t *testing.T) {
	const path = "/library/movies/Riddick (2013) [tmdb-87421]/Riddick (2013) [1080p].strm"
	probed := time.Now().UTC()

	repaired := &models.MediaFile{
		FilePath:       path,
		ProbeSource:    strm.ProbeSourcePlaceholder,
		ProbeUpdatedAt: &probed,
		Duration:       7607,
		Container:      "matroska,webm",
		CodecAudio:     "dts",
		CodecVideo:     "h264",
		Resolution:     "1080p",
		AudioTracks:    []models.AudioTrack{{Codec: "dts"}},
		VideoTracks:    []models.VideoTrack{{Codec: "h264"}},
		// Chapters round-trips through JSON and can come back nil. That must not
		// re-arm a network round trip.
		Chapters: nil,
	}
	if NeedsCriticalProbeRepair(repaired) {
		t.Error("a fully probed placeholder still wants repair; every playback would re-scrape it")
	}
}

// Before it is probed it must ask, or the duration never lands — and without a
// duration the HLS manifest cannot declare a total length, so the player treats
// the transcode as a growing live stream with nothing to seek against.
func TestUnprobedPlaceholderNeedsRepair(t *testing.T) {
	const path = "/library/movies/Title (2024) [tmdb-1]/Title (2024) [1080p].strm"

	// Straight off a scan: marker recorded, nothing else known.
	probed := time.Now().UTC()
	fresh := &models.MediaFile{
		FilePath:       path,
		ProbeSource:    strm.ProbeSourcePlaceholder,
		ProbeUpdatedAt: &probed,
	}
	if !NeedsCriticalProbeRepair(fresh) {
		t.Fatal("a freshly scanned placeholder does not want repair; its duration would stay 0")
	}

	// Each missing piece playback plans from must also trigger repair.
	for name, mutate := range map[string]func(*models.MediaFile){
		"no duration":     func(f *models.MediaFile) { f.Duration = 0 },
		"no container":    func(f *models.MediaFile) { f.Container = "" },
		"no audio codec":  func(f *models.MediaFile) { f.CodecAudio = "" },
		"no audio tracks": func(f *models.MediaFile) { f.AudioTracks = nil },
	} {
		f := &models.MediaFile{
			FilePath: path, ProbeSource: strm.ProbeSourcePlaceholder, ProbeUpdatedAt: &probed,
			Duration: 7607, Container: "matroska,webm", CodecAudio: "dts",
			AudioTracks: []models.AudioTrack{{Codec: "dts"}},
		}
		mutate(f)
		if !NeedsCriticalProbeRepair(f) {
			t.Errorf("%s: placeholder does not want repair but is missing playback-critical metadata", name)
		}
	}
}
