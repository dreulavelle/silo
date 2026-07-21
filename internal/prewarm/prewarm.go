// Package prewarm resolves placeholder metadata ahead of playback, in the
// background, so opening an item stays fast.
//
// A .strm placeholder has no metadata until something resolves it and probes
// the stream behind it. Doing that while building a detail response makes
// opening an item wait for a provider scrape plus a remote container read —
// measured at fifteen seconds for one episode, and it is paid per quality tier,
// sequentially. That is the wrong trade for a page whose job is to appear.
//
// So the detail path asks for a warm instead of performing one: the response
// goes out immediately with whatever is known, and the resolve happens behind
// it. By the time somebody presses play the metadata is usually already there,
// and playback — which genuinely cannot proceed without it — still resolves
// synchronously as a fallback.
//
// Three properties matter more than speed here, because this is unattended work
// against someone else's provider:
//
//   - One warm per file at a time. Opening a season page lists many episodes,
//     and each would otherwise start its own scrape for the same file.
//   - A hard ceiling on concurrent warms. A library of placeholders must not be
//     able to turn one page view into a hundred simultaneous scrapes.
//   - Warms are dropped, never queued, once that ceiling is reached. A warm is
//     an optimisation; a backlog of them would still be running long after the
//     viewer has gone, spending provider quota on pages nobody is looking at.
//
// This package is fork-owned. See FORK.md.
package prewarm

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// warmTimeout bounds one background warm. Generous, because nobody is waiting
// on it — but bounded, so a wedged provider cannot hold a slot forever.
const warmTimeout = 90 * time.Second

// defaultConcurrency caps simultaneous warms.
//
// Small on purpose. These are scrapes against a third-party provider, and the
// work is speculative: the viewer may never press play. Four is enough to warm
// the handful of files behind a page without looking like abuse.
const defaultConcurrency = 4

// Ensurer repairs a file's probe metadata. Satisfied by the scanner's
// PlaybackProbeEnsurer.
type Ensurer interface {
	Ensure(ctx context.Context, file *models.MediaFile) (*models.MediaFile, error)
}

// Warmer resolves placeholder metadata in the background.
type Warmer struct {
	ensurer Ensurer
	log     *slog.Logger
	sem     chan struct{}

	mu       sync.Mutex
	inFlight map[int]struct{}
}

// New returns a warmer. A nil ensurer makes every Warm a no-op, so a deployment
// without probe repair simply does not pre-warm.
func New(ensurer Ensurer, log *slog.Logger) *Warmer {
	if log == nil {
		log = slog.Default()
	}
	return &Warmer{
		ensurer:  ensurer,
		log:      log,
		sem:      make(chan struct{}, defaultConcurrency),
		inFlight: make(map[int]struct{}),
	}
}

// Warm starts a background resolve for a file, if one is not already running
// and there is capacity. It never blocks the caller.
//
// The caller's context is deliberately NOT propagated. It belongs to an HTTP
// response that is about to be written, and cancelling on that would abort
// every warm the moment the page finished loading — which is precisely when the
// work becomes useful.
func (w *Warmer) Warm(file *models.MediaFile) {
	if w == nil || w.ensurer == nil || file == nil || file.ID == 0 {
		return
	}

	w.mu.Lock()
	if _, running := w.inFlight[file.ID]; running {
		w.mu.Unlock()
		return
	}
	w.inFlight[file.ID] = struct{}{}
	w.mu.Unlock()

	select {
	case w.sem <- struct{}{}:
	default:
		// At capacity. Dropped rather than queued: a warm is speculative, and a
		// backlog would still be scraping long after the viewer left.
		w.release(file.ID)
		return
	}

	// Copied because the caller keeps using its own pointer to build a response
	// while this runs.
	target := *file

	go func() {
		defer func() {
			<-w.sem
			w.release(target.ID)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
		defer cancel()

		started := time.Now()
		if _, err := w.ensurer.Ensure(ctx, &target); err != nil {
			// Not an error anyone is waiting on: playback will resolve
			// synchronously and report properly if it still cannot.
			w.log.Info("prewarm: could not resolve ahead of playback",
				"file_id", target.ID, "err", err)
			return
		}
		w.log.Info("prewarm: resolved ahead of playback",
			"file_id", target.ID, "took_ms", time.Since(started).Milliseconds())
	}()
}

func (w *Warmer) release(id int) {
	w.mu.Lock()
	delete(w.inFlight, id)
	w.mu.Unlock()
}
