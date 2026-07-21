package scanner

import (
	"context"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/strm"
)

// placeholderProbeTimeout bounds resolving and probing a placeholder's stream.
//
// It covers a resolver scrape plus an HTTP container read, and sits inside the
// wait before playback starts — long enough to succeed on a cold scrape, short
// enough that a dead provider fails visibly instead of hanging the player.
const placeholderProbeTimeout = 45 * time.Second

// NeedsCriticalProbeRepair reports whether playback-critical probe metadata is
// missing and the file should be reprobed before making playback decisions.
func NeedsCriticalProbeRepair(file *models.MediaFile) bool {
	if file == nil {
		return true
	}
	// Ebook/comic files (epub, pdf, cbz, cbr — including manga chapters, which
	// are BaseType "ebook") are read directly by the reader and never go through
	// the transcode/playback probe pipeline. ffprobe yields nothing useful for
	// them, so requiring probe metadata re-ran ffprobe on every detail/watch
	// load and never converged.
	if file.BaseType == "ebook" {
		return false
	}
	// A placeholder is repaired by resolving it and probing the remote stream —
	// a scrape plus an HTTP read. That is affordable once, when someone opens
	// the title, and ruinous on every playback. So once it has the duration and
	// codecs playback actually plans from, it is done: the remaining criteria
	// below (chapters in particular, which round-trips through JSON and can come
	// back nil) must not re-arm a network round trip on every press of play.
	if strm.IsPlaceholderPath(file.FilePath) {
		// Converged: it has been probed and carries what playback plans from.
		// This escape has to come first and has to be unconditional, because
		// the audio checks below can never be satisfied by a title that simply
		// has no audio — a silent film, a music video with a stripped track, or
		// anything whose audio stream ffprobe cannot name. Without it those
		// re-scrape on every page load and every press of play, forever, which
		// is the exact storm this guard exists to prevent.
		probed := file.ProbeUpdatedAt != nil &&
			strings.TrimSpace(file.ProbeSource) == strm.ProbeSourcePlaceholder
		if probed && file.Duration > 0 && strings.TrimSpace(file.Container) != "" {
			return false
		}
		return file.Duration <= 0 ||
			strings.TrimSpace(file.Container) == "" ||
			strings.TrimSpace(file.CodecAudio) == "" ||
			len(file.AudioTracks) == 0
	}
	if strings.TrimSpace(file.ProbeSource) == "" || file.ProbeUpdatedAt == nil {
		return true
	}
	if file.Duration <= 0 {
		return true
	}
	// Legacy probes could turn malformed multi-day container timestamps into a
	// few seconds by treating ffprobe's seconds as microseconds. Reprobe the
	// narrow, physically implausible shape produced by that conversion.
	if needsLegacyDurationRepair(file) {
		return true
	}
	if strings.TrimSpace(file.Container) == "" {
		return true
	}
	if strings.TrimSpace(file.CodecAudio) == "" {
		return true
	}
	if len(file.AudioTracks) == 0 {
		return true
	}
	// Video metadata is playback-critical only for files that actually carry a
	// video stream. Audio-only files (audiobooks, music) legitimately probe to
	// zero video tracks and an empty video codec/resolution; treating that as
	// "needs repair" re-ran ffprobe on every playback decision (applyProbeData
	// only populates video fields under a "video" stream), so an audio-only
	// file would never satisfy the check. Only demand video fields when the
	// file was found to have a video stream.
	if len(file.VideoTracks) > 0 {
		if strings.TrimSpace(file.CodecVideo) == "" || strings.TrimSpace(file.Resolution) == "" {
			return true
		}
	}
	if file.Chapters == nil {
		return true
	}
	return false
}

// PlaybackProbeEnsurer repairs missing playback-critical probe metadata on
// demand by running a local ffprobe and persisting the result.
type PlaybackProbeEnsurer struct {
	fileRepo    *FileRepository
	ffprobePath string
	timeout     time.Duration
}

func NewPlaybackProbeEnsurer(fileRepo *FileRepository, ffprobePath string, timeout time.Duration) *PlaybackProbeEnsurer {
	return &PlaybackProbeEnsurer{
		fileRepo:    fileRepo,
		ffprobePath: ffprobePath,
		timeout:     timeout,
	}
}

func (e *PlaybackProbeEnsurer) Ensure(ctx context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	if file == nil || !NeedsCriticalProbeRepair(file) {
		return file, nil
	}
	if e == nil || e.fileRepo == nil || strings.TrimSpace(e.ffprobePath) == "" {
		return file, nil
	}

	timeout := e.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if reprobeMayScanPackets(file) && timeout < time.Minute {
		timeout = time.Minute
	}
	// Probing a placeholder means resolving it (a scrape) and then reading a
	// remote container over HTTP. The local-file default does not survive that.
	if strm.IsPlaceholderPath(file.FilePath) && timeout < placeholderProbeTimeout {
		timeout = placeholderProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// A placeholder has no local bytes, so probing the file itself fails. Probe
	// the stream it stands for instead.
	//
	// This is the ONE place a placeholder gets real metadata, and it has to
	// happen: without a duration the HLS manifest cannot declare a total
	// length, so a player treats the transcode as a live stream that grows as
	// it goes — the timeline reads 0:00 / 0:08 and there is nothing to seek
	// against. Codecs matter just as much, since without them playback cannot
	// tell whether it could direct play and transcodes everything.
	//
	// Doing it here rather than at scan time is deliberate: this runs once,
	// when someone actually opens the title, instead of resolving every
	// placeholder in the library on every scan.
	probeSource := "local"
	probePath := file.FilePath
	if strm.IsPlaceholderPath(file.FilePath) {
		resolved, resolveErr := strm.ResolveFileForInput(probeCtx, file.FilePath)
		if resolveErr != nil {
			return file, resolveErr
		}
		probePath = resolved
		// Keep the marker. It is what exempts the row from the scan-side repair
		// loop, and losing it here would re-arm that loop on the next scan.
		probeSource = strm.ProbeSourcePlaceholder
	}

	probe, err := ProbeFile(probeCtx, e.ffprobePath, probePath)
	if err != nil || probe == nil {
		return file, err
	}

	updated := *file
	applyProbeData(&updated, probe, probeSource)
	return e.fileRepo.Upsert(ctx, updated)
}

// reprobeMayScanPackets reports whether reprobing this file is likely to hit
// ProbeFile's packet-scan fallback, which demuxes the entire file and cannot
// finish inside the default metadata-probe timeout.
func reprobeMayScanPackets(file *models.MediaFile) bool {
	if file == nil || len(file.VideoTracks) == 0 {
		return false
	}
	return file.Duration <= 0 ||
		videoDurationImplausiblyShort(float64(file.Duration), file.FileSize, true)
}

// legacyProbeDurationFixTime is when the probe duration parser stopped
// treating large ffprobe durations as microseconds. Rows probed before this
// may carry the collapsed durations that conversion produced. Rows probed
// after it are authoritative: a still-short duration was re-derived from
// packet timestamps, and re-flagging it would reprobe genuinely short clips
// on every playback decision forever. Adjust if this fix ships later.
var legacyProbeDurationFixTime = time.Date(2026, time.July, 18, 0, 0, 0, 0, time.UTC)

func needsLegacyDurationRepair(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	return legacyDurationRepairNeeded(file.Duration, file.FileSize, len(file.VideoTracks) > 0, file.ProbeUpdatedAt)
}

func legacyDurationRepairNeeded(duration int, sizeBytes int64, hasVideo bool, probeUpdatedAt *time.Time) bool {
	if !videoDurationImplausiblyShort(float64(duration), sizeBytes, hasVideo) {
		return false
	}
	return probeUpdatedAt == nil || probeUpdatedAt.Before(legacyProbeDurationFixTime)
}
