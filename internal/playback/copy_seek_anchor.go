package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Silo-Server/silo-server/internal/strm"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	maxConcurrentCopySeekProbes = 4
	copySeekProbeTimeout        = 15 * time.Second

	// placeholderProbeTimeout applies when the input is a .strm.
	//
	// The local budget is a disk seek and a short read. A placeholder is a
	// provider scrape followed by ffmpeg demuxing into a remote container over
	// HTTP to find a keyframe — measured just under twelve seconds on a 4K
	// title, which leaves almost nothing before the local ceiling.
	placeholderProbeTimeout = 45 * time.Second

	// anchorCacheTTL bounds how long a computed anchor is reused.
	//
	// The anchor is a property of the media, not of the URL, so it stays
	// correct as long as the placeholder resolves to the same release. That is
	// stable within a viewing session and not guaranteed beyond one, so this is
	// deliberately short: long enough that scrubbing around a film is fast,
	// short enough that a changed release corrects itself quickly.
	anchorCacheTTL = 10 * time.Minute

	// anchorCacheMax bounds the cache. Anchors are tiny, but the key space is
	// every (title, position) anyone has scrubbed to, which is unbounded.
	anchorCacheMax = 512
)

var (
	copySeekProbeGroup singleflight.Group
	copySeekProbeSlots = make(chan struct{}, maxConcurrentCopySeekProbes)

	anchorCacheMu sync.Mutex
	anchorCache   = make(map[string]cachedAnchor, anchorCacheMax)
)

type cachedAnchor struct {
	anchor  copySeekAnchor
	expires time.Time
}

// lookupAnchor returns a cached anchor for a placeholder, if one is live.
func lookupAnchor(key string) (copySeekAnchor, bool) {
	anchorCacheMu.Lock()
	defer anchorCacheMu.Unlock()
	got, ok := anchorCache[key]
	if !ok || time.Now().After(got.expires) {
		return copySeekAnchor{}, false
	}
	return got.anchor, true
}

// storeAnchor caches an anchor, making room when the cache is full.
//
// Room is made by dropping expired entries first, and only then by dropping the
// soonest-to-expire. Clearing wholesale would be simpler, and was wrong: this
// cache holds ONLY placeholders, where a miss costs a provider scrape plus a
// remote demux. Emptying it takes every active viewer's anchors at once, so
// everyone scrubbing at that moment pays that cost together.
//
// Expired entries are also swept here rather than by a timer: nothing else
// visits this map, so without a sweep they occupy the bound and bring the
// eviction forward.
func storeAnchor(key string, a copySeekAnchor) {
	anchorCacheMu.Lock()
	defer anchorCacheMu.Unlock()

	if len(anchorCache) >= anchorCacheMax {
		now := time.Now()
		for k, v := range anchorCache {
			if now.After(v.expires) {
				delete(anchorCache, k)
			}
		}
		// Still full: every entry is live, so drop the one closest to expiring.
		// One eviction per insertion keeps the cache at its bound without ever
		// emptying it.
		if len(anchorCache) >= anchorCacheMax {
			var oldestKey string
			var oldest time.Time
			for k, v := range anchorCache {
				if oldestKey == "" || v.expires.Before(oldest) {
					oldestKey, oldest = k, v.expires
				}
			}
			delete(anchorCache, oldestKey)
		}
	}

	anchorCache[key] = cachedAnchor{anchor: a, expires: time.Now().Add(anchorCacheTTL)}
}

type copySeekProbePacket struct {
	PTSSeconds string `json:"pts_time"`
	DTSSeconds string `json:"dts_time"`
	Flags      string `json:"flags"`
}

type copySeekProbeOutput struct {
	Packets []copySeekProbePacket `json:"packets"`
}

type copySeekAnchor struct {
	seconds float64
	segment int
}

// ResolveCopySeekAnchor returns the keyframe timestamp FFmpeg's input seek will
// actually use for a copy-video restart. FFmpeg cannot discard the pre-roll
// between that keyframe and requestedSeekSeconds while preserving -c:v copy,
// so callers need both timestamps: requestedSeekSeconds remains the -ss input,
// while the returned anchor defines the stream's real timeline origin.
//
// The tiny read interval is intentional. ffprobe asks the demuxer to seek to
// requestedSeekSeconds, which lands on the same preceding key packet FFmpeg
// uses, then emits only the packets at that seek point instead of scanning the
// media from the beginning.
func ResolveCopySeekAnchor(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {
	if requestedSeekSeconds <= 0 {
		return 0, 0, nil
	}
	if strings.TrimSpace(inputPath) == "" {
		return 0, 0, fmt.Errorf("resolve copy seek anchor: empty input path")
	}
	if segmentDuration <= 0 {
		segmentDuration = DefaultSegmentDuration
	}

	// Keyed on the path as given, NOT on what it resolves to. A placeholder
	// resolves to a fresh URL every time, so keying on the resolved value would
	// make every key unique — defeating both the cache and the singleflight
	// dedup that stops concurrent seeks probing the same thing twice.
	isPlaceholder := strm.IsPlaceholderPath(inputPath)
	key := strings.Join([]string{
		ffprobePathFromFFmpeg(ffmpegPath),
		inputPath,
		strconv.FormatFloat(requestedSeekSeconds, 'f', 6, 64),
		strconv.Itoa(segmentDuration),
	}, "\x00")

	// Scrubbing revisits positions constantly, and for a placeholder each miss
	// is a scrape plus a remote demux. Local files skip the cache: their probe
	// is a disk seek, so caching would add bookkeeping for no gain.
	if isPlaceholder {
		if a, ok := lookupAnchor(key); ok {
			return a.seconds, a.segment, nil
		}
	}
	timeout := copySeekProbeTimeout
	if isPlaceholder {
		timeout = placeholderProbeTimeout
	}

	resultCh := copySeekProbeGroup.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		select {
		case copySeekProbeSlots <- struct{}{}:
			defer func() { <-copySeekProbeSlots }()
		case <-probeCtx.Done():
			return copySeekAnchor{}, probeCtx.Err()
		}

		// A placeholder holds a URL, not media, so ffprobe is given the stream
		// behind it. Resolved inside the singleflight so concurrent seeks to
		// the same position share one scrape as well as one probe.
		probeInput := inputPath
		if isPlaceholder {
			resolved, rerr := strm.ResolveFileForInput(probeCtx, inputPath)
			if rerr != nil {
				return copySeekAnchor{}, fmt.Errorf("resolve copy seek anchor: %w", rerr)
			}
			probeInput = resolved
		}

		seconds, segment, err := resolveCopySeekAnchor(probeCtx, ffmpegPath, probeInput, requestedSeekSeconds, segmentDuration)
		if err != nil {
			return copySeekAnchor{}, err
		}
		a := copySeekAnchor{seconds: seconds, segment: segment}
		if isPlaceholder {
			storeAnchor(key, a)
		}
		return a, nil
	})

	select {
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return 0, 0, result.Err
		}
		anchor := result.Val.(copySeekAnchor)
		return anchor.seconds, anchor.segment, nil
	}
}

func resolveCopySeekAnchor(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {

	interval := strconv.FormatFloat(requestedSeekSeconds, 'f', 6, 64) + "%+0.001"
	cmd := exec.CommandContext(ctx, ffprobePathFromFFmpeg(ffmpegPath),
		"-v", "error",
		"-select_streams", "v:0",
		"-read_intervals", interval,
		"-show_entries", "packet=pts_time,dts_time,flags",
		"-of", "json",
		inputPath,
	)
	var stdout bytes.Buffer
	stderr := newBoundedTailBuffer(stderrTailMaxBytes)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if tail := truncateStderr(stderr.String()); tail != "" {
			return 0, 0, fmt.Errorf("resolve copy seek anchor: ffprobe failed: %w (stderr: %s)", err, tail)
		}
		return 0, 0, fmt.Errorf("resolve copy seek anchor: ffprobe failed: %w", err)
	}

	var output copySeekProbeOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return 0, 0, fmt.Errorf("resolve copy seek anchor: decode ffprobe output: %w", err)
	}
	for _, packet := range output.Packets {
		if !strings.Contains(packet.Flags, "K") {
			continue
		}
		timestamp := packet.PTSSeconds
		if timestamp == "" || strings.EqualFold(timestamp, "N/A") {
			timestamp = packet.DTSSeconds
		}
		anchor, err := strconv.ParseFloat(timestamp, 64)
		if err != nil || math.IsNaN(anchor) || math.IsInf(anchor, 0) {
			continue
		}
		anchor = math.Max(0, anchor)
		return anchor, int(anchor / float64(segmentDuration)), nil
	}

	return 0, 0, fmt.Errorf("resolve copy seek anchor: ffprobe returned no key packet")
}
