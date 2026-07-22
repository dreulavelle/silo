package strm

import (
	"context"
	"errors"
	"testing"
)

// setResolver installs a by-name resolver for the duration of a test and
// restores the previous one (including nil) afterwards, so tests do not leak
// global resolver state into one another.
func setResolver(t *testing.T, fn func(string) (int, bool)) {
	t.Helper()
	prev := pluginIDResolver.Load()
	if fn == nil {
		pluginIDResolver.Store(nil)
	} else {
		pluginIDResolver.Store(&fn)
	}
	t.Cleanup(func() { pluginIDResolver.Store(prev) })
}

// The token lives in the query string and signs the resolve tuple, not the URL.
// A by-name rewrite that dropped or re-encoded the query would break auth at the
// origin, so the trailing path and the raw query must survive byte-for-byte —
// only the {pid} segment becomes a numeric id.
func TestRewriteByNamePreservesPathAndQuery(t *testing.T) {
	setResolver(t, func(pid string) (int, bool) {
		if pid == "wisp" {
			return 9, true
		}
		return 0, false
	})

	const in = "http://127.0.0.1:8080/api/v1/plugins/by-name/wisp/resolve/series/tmdb:1399/1/9?imdb=tt0944947&quality=1080p&t=abc%3D%3D"
	const want = "http://127.0.0.1:8080/api/v1/plugins/9/resolve/series/tmdb:1399/1/9?imdb=tt0944947&quality=1080p&t=abc%3D%3D"

	got, err := rewriteByNameTarget(in)
	if err != nil {
		t.Fatalf("rewriteByNameTarget() error = %v", err)
	}
	if got != want {
		t.Errorf("rewriteByNameTarget()\n got = %q\nwant = %q", got, want)
	}
}

// The whole point of ResolveTarget's up-front rewrite: a by-name placeholder is
// translated to numeric ONCE, and the result is what the hop loop would see.
// Numeric placeholders must pass through byte-identical.
func TestResolveTargetNumericPassthroughUnchanged(t *testing.T) {
	// A numeric target is external-shaped to the follower only in that it is a
	// host-local plugin route; with no server to hop to we cannot exercise the
	// hop here, so assert the rewrite step leaves it untouched by checking it is
	// not detected as by-name.
	const numeric = "http://127.0.0.1:8080/api/v1/plugins/9/resolve/movie/tmdb:603?imdb=tt0133093&t=xyz"
	if isByNameTarget(numeric) {
		t.Errorf("numeric target %q was misdetected as by-name", numeric)
	}
}

func TestIsByNameTarget(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8080/api/v1/plugins/by-name/wisp/resolve/movie/x": true,
		"http://127.0.0.1:8080/api/v1/plugins/9/resolve/movie/x":            false,
		"http://127.0.0.1:8080/api/v1/plugins/by-name":                      false, // no trailing slash, not the prefix
		"https://cdn.example.com/movie.mkv":                                 false,
		"":                                                                  false,
	}
	for target, want := range cases {
		if got := isByNameTarget(target); got != want {
			t.Errorf("isByNameTarget(%q) = %v, want %v", target, got, want)
		}
	}
}

// A nil resolver is a hard, non-retryable misconfiguration — never a guessed id.
// It must surface as ByNameUnresolvedError so ResolveTarget keeps it out of the
// transient/retry path.
func TestRewriteByNameNilResolver(t *testing.T) {
	setResolver(t, nil)

	_, err := rewriteByNameTarget("http://127.0.0.1:8080/api/v1/plugins/by-name/wisp/resolve/movie/x?t=1")
	var unresolved *ByNameUnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error = %v, want *ByNameUnresolvedError", err)
	}
	if unresolved.PluginID != "wisp" {
		t.Errorf("PluginID = %q, want %q", unresolved.PluginID, "wisp")
	}
}

// A ByNameUnresolvedError is NOT a ResolverUnavailableError: the retry path keys
// off the latter, and a missing/disabled/ambiguous plugin must not spin a doomed
// retry that hides the misconfiguration behind a spinner.
func TestByNameUnresolvedIsNotRetryable(t *testing.T) {
	var err error = &ByNameUnresolvedError{PluginID: "wisp", Reason: "disabled"}
	var retryable *ResolverUnavailableError
	if errors.As(err, &retryable) {
		t.Error("ByNameUnresolvedError must not classify as the retryable ResolverUnavailableError")
	}
}

// An id the resolver declines (not installed / disabled / ambiguous) is a hard
// failure, never a fallback id.
func TestRewriteByNameUnknownPlugin(t *testing.T) {
	setResolver(t, func(string) (int, bool) { return 0, false })

	_, err := rewriteByNameTarget("http://127.0.0.1:8080/api/v1/plugins/by-name/ghost/resolve/movie/x?t=1")
	var unresolved *ByNameUnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error = %v, want *ByNameUnresolvedError", err)
	}
	if unresolved.PluginID != "ghost" {
		t.Errorf("PluginID = %q, want %q", unresolved.PluginID, "ghost")
	}
}

// Traversal or injection smuggled into the {pid} segment must never become a
// followable route. Two layers stop it, and this asserts both:
//
//  1. Encoded traversal (%2e/%2f) in a by-name path is rejected by the SAME
//     host-local gate the numeric path uses, BEFORE any rewrite is attempted —
//     so ResolveTarget never even calls the resolver for such a target.
//  2. Traversal that survives decoding as a literal first segment ("..") is a
//     plugin id the resolver whitelist declines, so the rewrite fails hard.
func TestByNameRejectsInjectedPluginID(t *testing.T) {
	setResolver(t, func(pid string) (int, bool) {
		if pid == "wisp" {
			return 9, true
		}
		return 0, false
	})

	// Layer 1: the existing security gate rejects encoded traversal outright.
	for _, target := range []string{
		"http://127.0.0.1:8080/api/v1/plugins/by-name/wisp%2f..%2fadmin/x?t=1",
		"http://127.0.0.1:8080/api/v1/plugins/by-name/../../admin/x?t=1",
	} {
		if isHostLocalPluginRoute(target) {
			t.Errorf("isHostLocalPluginRoute(%q) = true, want traversal rejected", target)
		}
	}

	// Layer 2: a literal ".." first segment is not a known plugin id.
	if _, err := rewriteByNameTarget(
		"http://127.0.0.1:8080/api/v1/plugins/by-name/../resolve/movie/x?t=1"); err == nil {
		t.Error("rewriteByNameTarget with a traversal plugin id = nil error, want rejection")
	}
}

// End-to-end through ResolveTarget: a by-name target whose resolver declines
// short-circuits with the non-retryable error before any hop is attempted.
func TestResolveTargetByNameUnresolvedShortCircuits(t *testing.T) {
	setResolver(t, func(string) (int, bool) { return 0, false })

	_, err := ResolveTarget(context.Background(),
		"http://127.0.0.1:8080/api/v1/plugins/by-name/wisp/resolve/movie/x?t=1")
	var unresolved *ByNameUnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("ResolveTarget() error = %v, want *ByNameUnresolvedError", err)
	}
}
