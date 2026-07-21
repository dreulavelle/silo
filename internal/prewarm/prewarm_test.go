package prewarm

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

type stubEnsurer struct {
	calls   atomic.Int32
	release chan struct{} // when non-nil, Ensure blocks until it is closed
	// liveAtCall records whether the context was still usable at the moment
	// Ensure was entered. Checking after the fact is meaningless: the warm
	// cancels its own context on the way out.
	liveAtCall chan bool
}

func (s *stubEnsurer) Ensure(ctx context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	s.calls.Add(1)
	if s.liveAtCall != nil {
		select {
		case s.liveAtCall <- ctx.Err() == nil:
		default:
		}
	}
	if s.release != nil {
		<-s.release
	}
	return file, nil
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func eventually(t *testing.T, want int32, get func() int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("wanted %d, got %d", want, get())
}

// Warming must never block the caller: it runs while an HTTP response is being
// built, and the whole point is that the page does not wait for a scrape.
func TestWarmDoesNotBlockTheCaller(t *testing.T) {
	e := &stubEnsurer{release: make(chan struct{})}
	defer close(e.release)
	w := New(e, discard())

	done := make(chan struct{})
	go func() {
		w.Warm(&models.MediaFile{ID: 1})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Warm blocked on the resolve; the detail response would stall")
	}
}

// A season page lists many episodes and can ask for the same file repeatedly.
// Each duplicate would otherwise start its own scrape of the same stream.
func TestWarmDeduplicatesInFlightFiles(t *testing.T) {
	e := &stubEnsurer{release: make(chan struct{})}
	w := New(e, discard())

	for i := 0; i < 10; i++ {
		w.Warm(&models.MediaFile{ID: 7})
	}
	// Give any (incorrect) extra goroutines a chance to call through.
	time.Sleep(50 * time.Millisecond)
	if got := e.calls.Load(); got != 1 {
		t.Errorf("ran %d resolves for one file, want 1", got)
	}
	close(e.release)
}

// Once a warm finishes, the same file may be warmed again — the guard is
// "in flight", not "ever seen", or a file that failed could never retry.
func TestWarmAllowsARepeatAfterCompletion(t *testing.T) {
	e := &stubEnsurer{}
	w := New(e, discard())

	w.Warm(&models.MediaFile{ID: 3})
	eventually(t, 1, e.calls.Load)
	w.Warm(&models.MediaFile{ID: 3})
	eventually(t, 2, e.calls.Load)
}

// A library of placeholders must not turn one page view into a hundred
// simultaneous scrapes. Excess warms are DROPPED, not queued: they are
// speculative, and a backlog would still be running long after the viewer left.
func TestWarmDropsWorkBeyondTheConcurrencyCeiling(t *testing.T) {
	e := &stubEnsurer{release: make(chan struct{})}
	w := New(e, discard())

	for i := 1; i <= defaultConcurrency*5; i++ {
		w.Warm(&models.MediaFile{ID: i})
	}
	time.Sleep(80 * time.Millisecond)

	if got := e.calls.Load(); got > int32(defaultConcurrency) {
		t.Errorf("%d resolves running at once, want at most %d", got, defaultConcurrency)
	}
	close(e.release)
}

// The caller's context belongs to a response that is about to be written.
// Inheriting it would cancel every warm the instant the page finished loading —
// exactly when the work becomes useful.
func TestWarmSurvivesTheCallersContextEnding(t *testing.T) {
	e := &stubEnsurer{liveAtCall: make(chan bool, 1)}
	w := New(e, discard())

	_, cancel := context.WithCancel(context.Background())
	w.Warm(&models.MediaFile{ID: 5})
	cancel() // the response has been written

	select {
	case live := <-e.liveAtCall:
		if !live {
			t.Error("the warm ran with an already-cancelled context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("warm never ran")
	}
}

// A deployment without probe repair, or a call with nothing to warm, must be a
// no-op rather than a panic.
func TestWarmIsSafeWithNothingToDo(t *testing.T) {
	var nilWarmer *Warmer
	nilWarmer.Warm(&models.MediaFile{ID: 1})

	New(nil, discard()).Warm(&models.MediaFile{ID: 1})

	e := &stubEnsurer{}
	w := New(e, discard())
	w.Warm(nil)
	w.Warm(&models.MediaFile{}) // no id: nothing to dedupe on
	time.Sleep(30 * time.Millisecond)
	if got := e.calls.Load(); got != 0 {
		t.Errorf("ran %d resolves for nothing", got)
	}
}

// Concurrent warms of many files must not race on the in-flight map.
func TestWarmIsSafeUnderConcurrentCallers(t *testing.T) {
	e := &stubEnsurer{}
	w := New(e, discard())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w.Warm(&models.MediaFile{ID: i%7 + 1})
		}(i)
	}
	wg.Wait()
}
