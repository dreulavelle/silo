package strm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsHostLocalPluginRoute(t *testing.T) {
	cases := map[string]bool{
		// Host-local AND a plugin route: followable.
		"http://127.0.0.1:8080/api/v1/plugins/20/resolve/movie/tt1": true,
		"http://localhost:8080/api/v1/plugins/3/resolve/movie/tt1":  true,
		"http://[::1]:8080/api/v1/plugins/3/resolve/movie/tt1":      true,
		"https://127.0.0.1/api/v1/plugins/1/x":                      true,

		// Loopback but NOT a plugin route: must never be followed, or a
		// placeholder becomes an SSRF primitive against localhost services.
		"http://127.0.0.1:9090/metrics":         false,
		"http://localhost:5432/":                false,
		"http://127.0.0.1:8080/api/v1/users":    false,
		"http://127.0.0.1:8080/../etc/passwd":   false,
		"http://169.254.169.254/latest/meta":    false,
		"http://127.0.0.1:8080/api/v1/plugin/1": false, // near-miss prefix

		// External: returned as-is, never fetched server-side.
		"https://cdn.example.com/movie.mkv":                     false,
		"https://example.com/api/v1/plugins/20/resolve/movie/x": false,

		// Not a URL we would ever follow.
		"file:///etc/passwd": false,
		"":                   false,
	}

	for target, want := range cases {
		if got := isHostLocalPluginRoute(target); got != want {
			t.Errorf("isHostLocalPluginRoute(%q) = %v, want %v", target, got, want)
		}
	}
}

// External targets must pass through with no server-side request at all —
// that is the fast path and it must stay free.
func TestResolveTargetPassesThroughExternal(t *testing.T) {
	const target = "https://cdn.example.com/movie.mkv?token=x"

	got, err := ResolveTarget(context.Background(), target)
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if got != target {
		t.Errorf("ResolveTarget() = %q, want the target unchanged", got)
	}
}

func TestResolveTargetFollowsPluginHop(t *testing.T) {
	const final = "https://cdn.debrid.example/unrestricted/movie.mkv"

	var gotPath string
	srv := newLoopbackServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Location", final)
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()

	target := srv.URL + "/api/v1/plugins/20/resolve/movie/tt0133093"
	got, err := ResolveTarget(context.Background(), target)
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if got != final {
		t.Errorf("ResolveTarget() = %q, want %q", got, final)
	}
	if want := "/api/v1/plugins/20/resolve/movie/tt0133093"; gotPath != want {
		t.Errorf("resolver saw path %q, want %q", gotPath, want)
	}
}

// The resolver's answer is as attacker-influenceable as the placeholder, so it
// gets the same scheme validation. Otherwise a compromised resolver could hand
// back file:// and reintroduce exactly the bug scheme validation exists to stop.
func TestResolveTargetValidatesResolverAnswer(t *testing.T) {
	for _, hostile := range []string{"file:///etc/passwd", "javascript:alert(1)", "gopher://x/"} {
		t.Run(hostile, func(t *testing.T) {
			srv := newLoopbackServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", hostile)
				w.WriteHeader(http.StatusFound)
			})
			defer srv.Close()

			_, err := ResolveTarget(context.Background(), srv.URL+"/api/v1/plugins/1/resolve/movie/tt1")
			if err == nil {
				t.Fatalf("ResolveTarget() accepted a %q answer from the resolver", hostile)
			}
			var schemeErr *InvalidSchemeError
			if !errors.As(err, &schemeErr) {
				t.Errorf("error = %v, want *InvalidSchemeError", err)
			}
		})
	}
}

func TestResolveTargetRejectsRedirectLoop(t *testing.T) {
	var srv *httptest.Server
	srv = newLoopbackServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Point straight back at ourselves.
		w.Header().Set("Location", srv.URL+"/api/v1/plugins/1/resolve/movie/tt1")
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()

	_, err := ResolveTarget(context.Background(), srv.URL+"/api/v1/plugins/1/resolve/movie/tt1")
	if err == nil {
		t.Fatal("ResolveTarget() followed a redirect loop indefinitely")
	}
	if !strings.Contains(err.Error(), "hops") {
		t.Errorf("error = %v, want a hop-limit error", err)
	}
}

func TestResolveTargetRelativeLocation(t *testing.T) {
	srv := newLoopbackServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resolve/movie/tt1") {
			// Relative Location pointing at another plugin route.
			w.Header().Set("Location", "./final")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Location", "https://cdn.example.com/final.mkv")
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()

	got, err := ResolveTarget(context.Background(), srv.URL+"/api/v1/plugins/1/resolve/movie/tt1")
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if got != "https://cdn.example.com/final.mkv" {
		t.Errorf("ResolveTarget() = %q, want the final external URL", got)
	}
}

// "Not available right now" is a normal, retryable state for a resolver and
// must be distinguishable from a hard failure.
func TestResolveTargetSurfacesUnavailable(t *testing.T) {
	srv := newLoopbackServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	})
	defer srv.Close()

	_, err := ResolveTarget(context.Background(), srv.URL+"/api/v1/plugins/1/resolve/movie/tt1")

	var unavailable *ResolverUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *ResolverUnavailableError", err)
	}
}

func TestResolveTargetRedirectWithoutLocation(t *testing.T) {
	srv := newLoopbackServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound) // no Location
	})
	defer srv.Close()

	if _, err := ResolveTarget(context.Background(), srv.URL+"/api/v1/plugins/1/resolve/movie/tt1"); err == nil {
		t.Error("ResolveTarget() accepted a redirect with no Location")
	}
}

func TestResolveTargetUnexpectedStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError} {
		srv := newLoopbackServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})
		if _, err := ResolveTarget(context.Background(), srv.URL+"/api/v1/plugins/1/resolve/movie/tt1"); err == nil {
			t.Errorf("status %d: ResolveTarget() unexpectedly succeeded", status)
		}
		srv.Close()
	}
}

func TestResolveTargetRespectsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := newLoopbackServer(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
	})
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ResolveTarget(ctx, srv.URL+"/api/v1/plugins/1/resolve/movie/tt1"); err == nil {
		t.Error("ResolveTarget() ignored a cancelled context")
	}
}

// End-to-end through the HTTP handler: a placeholder pointing at a plugin route
// must produce a client-facing 302 to the *final* external URL, never to the
// unreachable host-local address.
func TestServePlaceholderResolvesPluginRouteEndToEnd(t *testing.T) {
	const final = "https://cdn.debrid.example/movie.mkv?token=abc"

	srv := newLoopbackServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", final)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()

	path := writeStrm(t, srv.URL+"/api/v1/plugins/20/resolve/movie/tt0133093?quality=2160p")

	rec := httptest.NewRecorder()
	if err := ServePlaceholder(rec, httptest.NewRequest(http.MethodGet, "/stream/1", nil), path); err != nil {
		t.Fatalf("ServePlaceholder() error = %v", err)
	}

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != final {
		t.Errorf("Location = %q, want the final external URL %q", got, final)
	}
	if strings.Contains(rec.Header().Get("Location"), "127.0.0.1") {
		t.Error("client was redirected at a host-local address it cannot reach")
	}
}

// newLoopbackServer starts a test server bound to loopback so that
// isHostLocalPluginRoute treats it as followable.
func newLoopbackServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	if !strings.Contains(srv.URL, "127.0.0.1") {
		t.Fatalf("test server is not on loopback: %s", srv.URL)
	}
	return srv
}
