package strm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlaceholder(t *testing.T, dir, name, target string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// An ordinary media file must pass through untouched. Every transcode goes
// through this call, so the local-file case has to stay free of surprises.
func TestResolveFileForInputPassesThroughOrdinaryPaths(t *testing.T) {
	for _, path := range []string{
		"/mnt/media/movies/The Matrix (1999)/The Matrix.mkv",
		"/mnt/media/tv/Show/S01E01.mp4",
		"",
	} {
		got, err := ResolveFileForInput(context.Background(), path)
		if err != nil {
			t.Errorf("ResolveFileForInput(%q) error = %v", path, err)
		}
		if got != path {
			t.Errorf("ResolveFileForInput(%q) = %q, want it unchanged", path, got)
		}
	}
}

// An already-external placeholder needs no hop: the URL in the file is what
// ffmpeg should open.
func TestResolveFileForInputReturnsExternalTargetDirectly(t *testing.T) {
	dir := t.TempDir()
	want := "https://cdn.example.invalid/movie.mkv?token=abc"
	path := writePlaceholder(t, dir, "movie.strm", want)

	got, err := ResolveFileForInput(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveFileForInput() error = %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The case that was broken: a placeholder addressing a plugin route on this
// host. ffmpeg cannot be handed the file, and it should not be handed the
// host-local URL either — the hop is made here so ffmpeg gets a final URL.
func TestResolveFileForInputFollowsHostLocalPluginRoute(t *testing.T) {
	final := "https://cdn.example.invalid/real-stream.mkv?t=xyz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, pluginRoutePrefix) {
			t.Errorf("resolver hit at unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Location", final)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", srv.URL+pluginRoutePrefix+"8/resolve/movie/tmdb:603")

	got, err := ResolveFileForInput(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveFileForInput() error = %v", err)
	}
	if got != final {
		t.Errorf("got %q, want the resolved stream %q", got, final)
	}
}

// The scheme allowlist is a security control, not a convenience — a .strm
// pointing at file:// was the arbitrary-file-read primitive in Jellyfin's RCE.
// It must hold on the transcode path exactly as it does on direct play.
func TestResolveFileForInputRejectsDisallowedSchemes(t *testing.T) {
	dir := t.TempDir()
	for i, target := range []string{
		"file:///etc/passwd",
		"file:///proc/self/environ",
		"javascript:alert(1)",
		"data:text/plain,hello",
		"gopher://example.invalid/",
	} {
		path := writePlaceholder(t, dir, "bad"+string(rune('a'+i))+".strm", target)
		if _, err := ResolveFileForInput(context.Background(), path); err == nil {
			t.Errorf("target %q was accepted; want it rejected", target)
		}
	}
}

// A resolver that answers with a disallowed scheme is as dangerous as a
// placeholder that names one directly, so the check must apply after the hop
// too.
func TestResolveFileForInputRejectsDisallowedSchemeFromResolver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := writePlaceholder(t, dir, "movie.strm", srv.URL+pluginRoutePrefix+"8/resolve/movie/tmdb:603")

	if _, err := ResolveFileForInput(context.Background(), path); err == nil {
		t.Error("a resolver redirecting to file:// was accepted")
	}
}

// An empty or unreadable placeholder must fail loudly rather than handing
// ffmpeg something that produces a confusing decode error.
func TestResolveFileForInputRejectsEmptyPlaceholder(t *testing.T) {
	dir := t.TempDir()
	path := writePlaceholder(t, dir, "empty.strm", "")
	if _, err := ResolveFileForInput(context.Background(), path); err == nil {
		t.Error("an empty placeholder was accepted")
	}

	missing := filepath.Join(dir, "gone.strm")
	if _, err := ResolveFileForInput(context.Background(), missing); err == nil {
		t.Error("a missing placeholder was accepted")
	}
}
