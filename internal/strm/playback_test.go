package strm

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServePlaceholderRedirects(t *testing.T) {
	path := writeStrm(t, "https://cdn.example.com/movie.mkv?token=abc")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/1", nil)

	if err := ServePlaceholder(rec, req, path); err != nil {
		t.Fatalf("ServePlaceholder() error = %v", err)
	}

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "https://cdn.example.com/movie.mkv?token=abc" {
		t.Errorf("Location = %q, want the resolved target verbatim", got)
	}
}

// Guards the single most consequential constant in this package. FFmpeg caches
// 301 and 308 for the life of its HTTPContext; a cached redirect to an expired
// target cannot recover without restarting playback. If someone "tidies" this
// to a permanent redirect, playback breaks in a way that is miserable to debug.
func TestRedirectStatusIsNotCacheableByFFmpeg(t *testing.T) {
	if RedirectStatus == http.StatusMovedPermanently || RedirectStatus == http.StatusPermanentRedirect {
		t.Fatalf("RedirectStatus = %d: FFmpeg caches 301/308 indefinitely; use 302 or 307", RedirectStatus)
	}
	if RedirectStatus != http.StatusFound && RedirectStatus != http.StatusTemporaryRedirect {
		t.Errorf("RedirectStatus = %d, want 302 or 307", RedirectStatus)
	}
}

// Targets are short-lived. A cached redirect anywhere in the chain hands a
// client a stale target with no way to recover.
func TestServePlaceholderForbidsCaching(t *testing.T) {
	path := writeStrm(t, "https://cdn.example.com/movie.mkv")

	rec := httptest.NewRecorder()
	if err := ServePlaceholder(rec, httptest.NewRequest(http.MethodGet, "/s", nil), path); err != nil {
		t.Fatalf("ServePlaceholder() error = %v", err)
	}

	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want it to contain no-store", cc)
	}
}

func TestServePlaceholderRejectsHostileTargets(t *testing.T) {
	for _, target := range []string{"/etc/passwd", "file:///etc/passwd", "javascript:alert(1)"} {
		t.Run(target, func(t *testing.T) {
			path := writeStrm(t, target)

			rec := httptest.NewRecorder()
			err := ServePlaceholder(rec, httptest.NewRequest(http.MethodGet, "/s", nil), path)
			if err == nil {
				t.Fatal("ServePlaceholder() unexpectedly succeeded")
			}
			if rec.Code != http.StatusBadGateway {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
			}
			// The rejected target must not be echoed anywhere a client can see.
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("Location = %q, want empty on a rejected target", loc)
			}
			if body := rec.Body.String(); strings.Contains(body, target) {
				t.Errorf("body leaked the rejected target: %q", body)
			}
		})
	}
}

// A requested-but-unresolved placeholder is a normal state, not an error. It
// must be distinguishable from a hard failure so clients can retry.
func TestServePlaceholderNotReady(t *testing.T) {
	path := writeStrm(t, "#EXTM3U\n")

	rec := httptest.NewRecorder()
	if err := ServePlaceholder(rec, httptest.NewRequest(http.MethodGet, "/s", nil), path); err == nil {
		t.Fatal("ServePlaceholder() unexpectedly succeeded")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestServePlaceholderMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.strm")

	rec := httptest.NewRecorder()
	if err := ServePlaceholder(rec, httptest.NewRequest(http.MethodGet, "/s", nil), path); err == nil {
		t.Fatal("ServePlaceholder() unexpectedly succeeded")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// HEAD is what probing clients send first; it must redirect exactly like GET
// rather than falling through to a body-serving path.
func TestServePlaceholderHandlesHEAD(t *testing.T) {
	path := writeStrm(t, "https://cdn.example.com/movie.mkv")

	rec := httptest.NewRecorder()
	if err := ServePlaceholder(rec, httptest.NewRequest(http.MethodHead, "/s", nil), path); err != nil {
		t.Fatalf("ServePlaceholder() error = %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
}

// Rewriting a placeholder must change where the next play goes, with no cache
// to invalidate and no rescan. This is the property the whole design rests on.
func TestServePlaceholderFollowsRewrites(t *testing.T) {
	path := writeStrm(t, "https://cdn.example.com/first.mkv")

	first := httptest.NewRecorder()
	if err := ServePlaceholder(first, httptest.NewRequest(http.MethodGet, "/s", nil), path); err != nil {
		t.Fatalf("first ServePlaceholder() error = %v", err)
	}

	if err := os.WriteFile(path, []byte("https://cdn.example.com/second.mkv"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := httptest.NewRecorder()
	if err := ServePlaceholder(second, httptest.NewRequest(http.MethodGet, "/s", nil), path); err != nil {
		t.Fatalf("second ServePlaceholder() error = %v", err)
	}

	if got := second.Header().Get("Location"); got != "https://cdn.example.com/second.mkv" {
		t.Errorf("Location after rewrite = %q, want the new target", got)
	}
}

// A title that is not released, not cached, or briefly unavailable is the most
// common way playback fails, and it is not a fault. Answering 500 tells a
// viewer the server is broken and tells a client not to retry — both wrong.
func TestServePlaceholderReportsUnavailableAsRetryable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.strm")

	// A resolver that is reachable but has nothing to serve.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	target := srv.URL + pluginRoutePrefix + "8/resolve/movie/tmdb:1"
	if err := os.WriteFile(path, []byte(target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	SetHostPort(portOf(t, srv.URL))
	t.Cleanup(func() { allowedPort.Store(nil) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	_ = ServePlaceholder(rec, req, path)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; a title that is not out yet is not a server fault", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(strings.ToLower(body), "internal server error") {
		t.Errorf("body = %q, want something a viewer can act on", strings.TrimSpace(body))
	}
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}
