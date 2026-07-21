package strm

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetResolvedCache clears the package-global resolve state so a test starts
// from a known baseline. The cache is process-global by design (it is shared by
// every ffmpeg launch), so tests that exercise it must not inherit each other's
// entries.
func resetResolvedCache(t *testing.T) {
	t.Helper()
	resolvedMu.Lock()
	resolvedCache = make(map[string]cachedResolution, resolvedCacheMax)
	freshPending = make(map[string]time.Time)
	resolveGen = 0
	resolvedMu.Unlock()
	// A prior test may have pinned the followable port; loopback resolvers here
	// bind to a random one, so unpin for the duration of this test.
	allowedPort.Store(nil)
}

// armedFresh reports whether path is currently armed for a fresh re-resolve.
func armedFresh(path string) bool {
	fresh, _ := loadResolveState(path)
	return fresh
}

// countingResolver returns a loopback plugin-route server that answers every
// request with a 302 to final, counting how many times it was actually hit and
// recording the last fresh query value it saw.
func countingResolver(t *testing.T, final string) (*resolverProbe, string) {
	t.Helper()
	probe := &resolverProbe{}
	srv := newLoopbackServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probe.hits, 1)
		probe.mu.Lock()
		probe.lastFresh = r.URL.Query().Get(freshQueryParam)
		probe.mu.Unlock()
		if probe.gate != nil {
			<-probe.gate
		}
		w.Header().Set("Location", final)
		w.WriteHeader(http.StatusFound)
	})
	t.Cleanup(srv.Close)
	return probe, srv.URL + pluginRoutePrefix + "8/resolve/movie/tmdb:603"
}

type resolverProbe struct {
	hits      int32
	gate      chan struct{}
	mu        sync.Mutex
	lastFresh string
}

func (p *resolverProbe) count() int32 {
	return atomic.LoadInt32(&p.hits)
}

func (p *resolverProbe) fresh() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastFresh
}

// A second resolve within the TTL must reuse the first result without hitting
// the resolver again — this is the whole point of the cache: a seek restart
// seconds after the initial resolve pays nothing.
func TestResolveFileForInputCachesWithinTTL(t *testing.T) {
	resetResolvedCache(t)
	const final = "https://cdn.example.invalid/stream.mkv?t=1"
	probe, route := countingResolver(t, final)

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", route)

	for i := 0; i < 3; i++ {
		got, err := ResolveFileForInput(context.Background(), path)
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if got != final {
			t.Fatalf("resolve %d = %q, want %q", i, got, final)
		}
	}
	if n := probe.count(); n != 1 {
		t.Errorf("resolver hit %d times, want 1 (cache should have served the rest)", n)
	}
}

// Once an entry expires the next resolve must go back to the plugin. Expiry is
// forced by rewinding the cached entry rather than by waiting out the real TTL.
func TestResolveFileForInputReResolvesAfterExpiry(t *testing.T) {
	resetResolvedCache(t)
	const final = "https://cdn.example.invalid/stream.mkv?t=2"
	probe, route := countingResolver(t, final)

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", route)

	if _, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	resolvedMu.Lock()
	entry := resolvedCache[path]
	entry.expires = time.Now().Add(-time.Second)
	resolvedCache[path] = entry
	resolvedMu.Unlock()

	if _, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if n := probe.count(); n != 2 {
		t.Errorf("resolver hit %d times, want 2 (expired entry should re-resolve)", n)
	}
}

// A placeholder rewritten to point somewhere new must not be served the old
// resolved URL for the rest of the TTL — the rewrite-in-place guarantee that
// ReadTarget exists for has to survive the cache.
func TestResolveFileForInputReResolvesWhenTargetRewritten(t *testing.T) {
	resetResolvedCache(t)
	probe, route := countingResolver(t, "https://cdn.example.invalid/first.mkv")

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", route)
	if _, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Rewrite the placeholder in place to a different (external) target.
	const rewritten = "https://cdn.example.invalid/rewritten.mkv"
	writePlaceholder(t, dir, "movie.strm", rewritten)

	got, err := ResolveFileForInput(context.Background(), path)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got != rewritten {
		t.Errorf("got %q, want the rewritten target %q (stale cache masked a rewrite)", got, rewritten)
	}
	if n := probe.count(); n != 1 {
		t.Errorf("resolver hit %d times, want 1 (rewrite points external, no hop)", n)
	}
}

// A burst of concurrent resolves for the same path — the stall storm a downed
// session produces — must collapse to a single scrape.
func TestResolveFileForInputCoalescesConcurrent(t *testing.T) {
	resetResolvedCache(t)
	const final = "https://cdn.example.invalid/stream.mkv?t=3"
	probe, route := countingResolver(t, final)
	probe.gate = make(chan struct{})

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", route)

	const callers = 8
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = ResolveFileForInput(context.Background(), path)
		}(i)
	}

	// Give every caller time to enter the singleflight before the one in-flight
	// resolver is allowed to answer, so they all attach to it.
	time.Sleep(100 * time.Millisecond)
	close(probe.gate)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != final {
			t.Errorf("caller %d = %q, want %q", i, results[i], final)
		}
	}
	if n := probe.count(); n != 1 {
		t.Errorf("resolver hit %d times, want 1 (concurrent resolves must coalesce)", n)
	}
}

// InvalidateResolved must drop the cached entry so the next resolve goes back to
// the plugin — and, because invalidation only ever follows a failure, that next
// resolve must carry fresh=1.
func TestInvalidateResolvedForcesFreshReResolve(t *testing.T) {
	resetResolvedCache(t)
	const final = "https://cdn.example.invalid/stream.mkv?t=4"
	probe, route := countingResolver(t, final)

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", route)

	if _, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if probe.fresh() != "" {
		t.Errorf("first resolve carried fresh=%q, want none", probe.fresh())
	}

	InvalidateResolved(path, final)

	if _, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("post-invalidate resolve: %v", err)
	}
	if n := probe.count(); n != 2 {
		t.Errorf("resolver hit %d times, want 2 (invalidate must force a re-resolve)", n)
	}
	if probe.fresh() != "1" {
		t.Errorf("post-invalidate resolve carried fresh=%q, want \"1\"", probe.fresh())
	}
}

// After a successful fresh re-resolve the arming clears: an ordinary resolve
// that follows must not keep sending fresh=1, or the cache would never settle.
func TestFreshArmingClearsAfterSuccess(t *testing.T) {
	resetResolvedCache(t)
	const final = "https://cdn.example.invalid/stream.mkv?t=5"
	probe, route := countingResolver(t, final)

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", route)

	InvalidateResolved(path, "")
	if _, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("fresh resolve: %v", err)
	}
	if probe.fresh() != "1" {
		t.Fatalf("armed resolve carried fresh=%q, want \"1\"", probe.fresh())
	}
	if armedFresh(path) {
		t.Error("fresh arming did not clear after a successful resolve")
	}

	// The very next resolve is a cache hit, so the resolver is not touched and
	// its recorded fresh value stays "1" from the previous call. Force a miss by
	// expiring the entry, then confirm the re-resolve is no longer fresh.
	resolvedMu.Lock()
	entry := resolvedCache[path]
	entry.expires = time.Now().Add(-time.Second)
	resolvedCache[path] = entry
	resolvedMu.Unlock()

	if _, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("settled resolve: %v", err)
	}
	if probe.fresh() != "" {
		t.Errorf("settled resolve carried fresh=%q, want none", probe.fresh())
	}
}

// The clobber race: a slow ordinary flight is in the air when the session
// crashes; the crash-armed fresh flight resolves the good URL and caches it; the
// ordinary flight then completes with the now-dead URL and must NOT overwrite
// the fresh entry. The interleaving is forced deterministically by holding the
// ordinary flight open inside the resolver.
func TestOrdinaryFlightDoesNotClobberFreshEntry(t *testing.T) {
	resetResolvedCache(t)
	const deadURL = "https://cdn.example.invalid/dead.mkv"
	const goodURL = "https://cdn.example.invalid/good.mkv"

	gate := make(chan struct{})
	var hits int32
	srv := newLoopbackServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// The first hit is the ordinary flight; hold it open so the crash and
		// the fresh re-resolve can complete while it is still in the air. Every
		// later hit is the fresh flight and answers immediately with the good URL.
		if atomic.AddInt32(&hits, 1) == 1 {
			<-gate
			w.Header().Set("Location", deadURL)
		} else {
			w.Header().Set("Location", goodURL)
		}
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()

	route := srv.URL + pluginRoutePrefix + "8/resolve/movie/tmdb:603"
	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", route)

	type outcome struct {
		url string
		err error
	}
	ordinary := make(chan outcome, 1)
	go func() {
		got, err := ResolveFileForInput(context.Background(), path)
		ordinary <- outcome{got, err}
	}()

	// Wait until the ordinary flight is parked inside the resolver.
	waitFor(t, func() bool { return atomic.LoadInt32(&hits) == 1 })

	// Crash: invalidate (arms fresh, advances the generation) while it is parked.
	// The cache is still empty at this point, so the used-URL guard is moot.
	InvalidateResolved(path, "")

	// The crash-armed re-resolve runs on a distinct singleflight key, so it does
	// not attach to the parked flight — it hits the resolver again and gets good.
	if got, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("fresh re-resolve: %v", err)
	} else if got != goodURL {
		t.Fatalf("fresh re-resolve = %q, want %q", got, goodURL)
	}
	if u, ok := lookupResolved(path, route); !ok || u != goodURL {
		t.Fatalf("after fresh resolve cache = (%q,%v), want %q cached", u, ok, goodURL)
	}

	// Release the ordinary flight. It returns the dead URL to its own caller but
	// must not cache it — the generation moved under it.
	close(gate)
	if out := <-ordinary; out.err != nil {
		t.Fatalf("ordinary flight errored: %v", out.err)
	}

	if u, ok := lookupResolved(path, route); !ok || u != goodURL {
		t.Errorf("ordinary flight clobbered the fresh entry: cache = (%q,%v), want %q", u, ok, goodURL)
	}
}

// waitFor polls cond until it holds or a short deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// appendFreshParam must add the flag whether or not the target already carries a
// query, and must not duplicate it on a retry of a retry.
func TestAppendFreshParam(t *testing.T) {
	cases := map[string]string{
		"https://host.invalid/api/v1/plugins/8/resolve/movie/tt1":         "fresh=1",
		"https://host.invalid/api/v1/plugins/8/resolve/movie/tt1?q=2160p": "fresh=1",
		"https://host.invalid/x?fresh=0":                                  "fresh=1",
	}
	for target, want := range cases {
		got := appendFreshParam(target)
		u, err := http.NewRequest(http.MethodGet, got, nil)
		if err != nil {
			t.Fatalf("appendFreshParam(%q) = %q, unparseable: %v", target, got, err)
		}
		if fresh := u.URL.Query().Get(freshQueryParam); fresh != "1" {
			t.Errorf("appendFreshParam(%q) = %q, fresh=%q, want %s", target, got, fresh, want)
		}
	}
}

// A non-placeholder path is invalidation-inert: InvalidateResolved must not arm
// a fresh marker for an ordinary local file, which is never cached.
func TestInvalidateResolvedIgnoresNonPlaceholders(t *testing.T) {
	resetResolvedCache(t)
	const local = "/mnt/media/movies/The Matrix (1999)/The Matrix.mkv"
	InvalidateResolved(local, "")
	if armedFresh(local) {
		t.Error("a non-placeholder path was armed for a fresh resolve")
	}
}

// A .strm may point directly at an external CDN, and .strm supports that. Such a
// target must reach ffmpeg byte-for-byte even on a fresh resolve — appending
// fresh=1 to a signed URL would break its signature. Only host-local plugin
// routes get the flag.
func TestFreshResolveLeavesExternalTargetUnchanged(t *testing.T) {
	resetResolvedCache(t)
	const external = "https://cdn.example.invalid/movie.mkv?token=SIGNED&exp=123"

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", external)

	// Arm fresh, then resolve: the external target must come back exactly as
	// written, with no fresh=1 grafted onto its query.
	InvalidateResolved(path, "")
	got, err := ResolveFileForInput(context.Background(), path)
	if err != nil {
		t.Fatalf("fresh resolve: %v", err)
	}
	if got != external {
		t.Errorf("fresh resolve mutated an external target: got %q, want %q", got, external)
	}
}

// Invalidation is keyed by path, so a stale session dying on the URL it used
// must not evict a newer URL another session already re-resolved. The used-URL
// guard makes invalidation a no-op when the cache has moved on.
func TestInvalidateResolvedSkipsWhenCacheMovedOn(t *testing.T) {
	resetResolvedCache(t)
	const current = "https://cdn.example.invalid/current.mkv"
	probe, route := countingResolver(t, current)

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", route)

	if _, err := ResolveFileForInput(context.Background(), path); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// A different (older) session reports its dead URL — not the one cached now.
	InvalidateResolved(path, "https://cdn.example.invalid/older-dead.mkv")

	if armedFresh(path) {
		t.Error("invalidation armed fresh for a URL the cache no longer holds")
	}
	got, err := ResolveFileForInput(context.Background(), path)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got != current {
		t.Errorf("got %q, want the still-cached URL %q", got, current)
	}
	if n := probe.count(); n != 1 {
		t.Errorf("resolver hit %d times, want 1 (stale invalidation must not evict the live entry)", n)
	}
}

// A placeholder rewritten while its old target is mid-resolve must not have a
// caller that read the NEW target join the OLD flight and be handed its URL. The
// singleflight key includes the target, so the two resolves stay distinct.
func TestSingleflightKeyIncludesTarget(t *testing.T) {
	resetResolvedCache(t)
	const oldFinal = "https://cdn.example.invalid/old.mkv"
	const newFinal = "https://cdn.example.invalid/new.mkv"

	gate := make(chan struct{})
	var oldHits int32
	srv := newLoopbackServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The OLD route is held open; the NEW route answers immediately. Which
		// one a request lands on is decided by the target the caller read.
		if strings.Contains(r.URL.Path, "/OLD") {
			atomic.AddInt32(&oldHits, 1)
			<-gate
			w.Header().Set("Location", oldFinal)
		} else {
			w.Header().Set("Location", newFinal)
		}
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()

	oldRoute := srv.URL + pluginRoutePrefix + "8/resolve/movie/OLD"
	newRoute := srv.URL + pluginRoutePrefix + "8/resolve/movie/NEW"

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", oldRoute)

	type outcome struct {
		url string
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		got, err := ResolveFileForInput(context.Background(), path)
		first <- outcome{got, err}
	}()

	// Wait until the first resolve is parked in the OLD route, then rewrite the
	// placeholder to the NEW route beneath it.
	waitFor(t, func() bool { return atomic.LoadInt32(&oldHits) == 1 })
	writePlaceholder(t, dir, "movie.strm", newRoute)

	// A caller reading the NEW target must not attach to the parked OLD flight.
	got, err := ResolveFileForInput(context.Background(), path)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got != newFinal {
		t.Errorf("second resolve = %q, want %q (it joined the stale flight)", got, newFinal)
	}

	close(gate)
	if out := <-first; out.err != nil {
		t.Fatalf("first resolve errored: %v", out.err)
	} else if out.url != oldFinal {
		t.Errorf("first resolve = %q, want %q", out.url, oldFinal)
	}
}
