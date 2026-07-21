package strm

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

// maxInternalHops bounds how many host-local redirects we will follow before
// giving up. A resolver plugin should answer in one hop; more than a couple
// means a misconfiguration or a loop, and each hop is latency a user is
// waiting through.
const maxInternalHops = 3

// internalResolveTimeout bounds a single host-local hop.
//
// This sits inside a user's time-to-first-frame, on top of whatever the
// resolver itself spends talking to its upstream. It is generous enough for a
// scrape-and-unrestrict round trip but short enough that a hung resolver fails
// visibly instead of hanging the player.
const internalResolveTimeout = 30 * time.Second

// pluginRoutePrefix is the path prefix under which Silo mounts plugin HTTP
// routes. A placeholder that points at a host-local address is only followed
// when it also addresses a plugin route.
const pluginRoutePrefix = "/api/v1/plugins/"

// userAgent identifies host-local resolver hops.
const userAgent = "silo-strm/1.0"

// internalClient performs host-local hops. Redirects are explicitly NOT
// followed: we need the Location header, not the body behind it, because the
// whole point is to hand that location to the client rather than proxy it.
var internalClient = &http.Client{
	Timeout: internalResolveTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// isHostLocalPluginRoute reports whether a target addresses a plugin route on
// this host.
//
// Both conditions are required. Loopback alone is not enough: following any
// loopback URL would turn a placeholder into a server-side request forgery
// primitive against every other service bound to localhost. Restricting to the
// plugin route prefix keeps the followable surface to Silo's own plugin
// dispatch, which is a controlled endpoint.
func isHostLocalPluginRoute(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}

	// The prefix check runs against the path as it will be SENT, and Go
	// transmits RequestURI verbatim without normalising it. A target of
	// /api/v1/plugins/../../../admin/secrets therefore satisfies a naive prefix
	// test while the origin server resolves it back to /admin/secrets — which
	// turns a placeholder into a blind GET against anything on loopback.
	// Rejecting any path that is not already in its cleaned form closes that
	// without having to reason about how each server normalises.
	if u.Path != path.Clean(u.Path) || !strings.HasPrefix(path.Clean(u.Path), pluginRoutePrefix) {
		return false
	}
	// An escaped traversal survives Clean because it is still encoded here, and
	// is decoded by the origin. EscapedPath is what actually goes on the wire.
	if escaped := u.EscapedPath(); strings.Contains(escaped, "%2e") || strings.Contains(escaped, "%2E") ||
		strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%2F") {
		return false
	}

	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}

	// Loopback alone is a very large surface: a database, a cache, an admin UI
	// and this server all sit on it. Pinning to the port Silo itself listens on
	// narrows the followable target to this process.
	return portIsAllowed(u.Port())
}

// allowedPort is the port host-local placeholder targets may address. Empty
// means "any loopback port", which is the safe-but-broad default used when the
// server has not told this package where it listens.
var allowedPort atomic.Pointer[string]

// SetHostPort pins which loopback port a placeholder may be followed to.
// Called once at startup with the port this server listens on.
func SetHostPort(port string) {
	port = strings.TrimSpace(port)
	if port == "" {
		return
	}
	allowedPort.Store(&port)
}

func portIsAllowed(port string) bool {
	want := allowedPort.Load()
	if want == nil {
		return true
	}
	if port == "" {
		// No explicit port means the scheme default; it is not this server's.
		return false
	}
	return port == *want
}

// ResolveTarget turns a placeholder target into a URL a client can actually
// open.
//
// A placeholder written by a resolver plugin points at that plugin's own route
// on this host — an address no client can reach. For those, we make the hop
// ourselves and take the Location the plugin answers with. Targets that are
// already external are returned untouched, so the common case costs nothing.
//
// The returned URL is always safe to hand to a client: external targets pass
// through, and host-local ones are replaced by whatever the resolver pointed us
// at.
func ResolveTarget(ctx context.Context, target string) (string, error) {
	current := target

	for hop := 0; hop < maxInternalHops; hop++ {
		if !isHostLocalPluginRoute(current) {
			// Already client-reachable.
			return current, nil
		}

		next, err := followInternalHop(ctx, current)
		if err != nil {
			return "", err
		}
		current = next
	}

	return "", fmt.Errorf("strm: exceeded %d host-local hops resolving placeholder (redirect loop?)", maxInternalHops)
}

// followInternalHop performs one host-local request and returns its Location.
func followInternalHop(ctx context.Context, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("strm: build resolver request: %w", err)
	}

	// Go's default User-Agent ("Go-http-client/...") is blocked outright by some
	// bot-protection layers in this ecosystem — Cloudflare in front of at least
	// one widely-used stream provider answers it with a 403 HTML page while
	// accepting any ordinary agent. Host-local plugin routes do not care, but
	// this hop is cheap insurance and makes traffic identifiable in logs.
	req.Header.Set("User-Agent", userAgent)

	resp, err := internalClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("strm: resolver request failed: %w", err)
	}
	defer func() {
		// Drain a bounded amount so the connection can be reused; a resolver
		// answering a redirect should have little or no body.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location == "" {
			return "", fmt.Errorf("strm: resolver returned %d with no Location header", resp.StatusCode)
		}
		// The resolver may answer with a relative Location; resolve it against
		// the request URL so the next loop iteration sees an absolute target.
		base, err := url.Parse(target)
		if err != nil {
			return "", fmt.Errorf("strm: parse resolver target: %w", err)
		}
		next, err := base.Parse(location)
		if err != nil {
			return "", fmt.Errorf("strm: parse resolver Location: %w", err)
		}
		resolved := next.String()

		// The resolver's answer is attacker-influenceable in the same way the
		// placeholder itself is, so it gets the same scheme check.
		if err := ValidateTarget(resolved); err != nil {
			return "", err
		}
		return resolved, nil

	case isTemporaryUpstream(resp.StatusCode):
		// The resolver is reachable and cannot serve right now. That covers
		// more than "no streams yet": the provider behind it may be down (502),
		// timing out (504), or throttling us (429). All are temporary, all are
		// worth retrying, and none of them mean this server is broken — which
		// is what a 500 would tell the viewer and the client.
		return "", &ResolverUnavailableError{
			Status:     resp.StatusCode,
			RetryAfter: strings.TrimSpace(resp.Header.Get("Retry-After")),
		}

	default:
		return "", fmt.Errorf("strm: resolver returned unexpected status %d", resp.StatusCode)
	}
}

// isTemporaryUpstream reports whether a resolver status means "try again".
//
// Deliberately a small, explicit set. A 4xx that is not 429 is a real fault —
// a malformed request, a missing route — and quietly retrying those would hide
// a bug behind a spinner.
func isTemporaryUpstream(status int) bool {
	switch status {
	case http.StatusServiceUnavailable, // nothing to serve yet
		http.StatusBadGateway,      // the provider behind the resolver is down
		http.StatusGatewayTimeout,  // ...or too slow
		http.StatusTooManyRequests: // ...or throttling us
		return true
	}
	return false
}

// ResolverUnavailableError reports that a resolver was reached but cannot serve
// right now. It is distinct from a transport failure because it is expected and
// retryable: the title may not be released, the provider may be down, or we may
// be throttled.
type ResolverUnavailableError struct {
	Status int

	// RetryAfter carries the upstream's own hint verbatim, when it gave one.
	// Passing it on lets a client back off by the provider's schedule instead
	// of guessing — which is the difference between waiting out a rate limit
	// and deepening it.
	RetryAfter string
}

func (e *ResolverUnavailableError) Error() string {
	return fmt.Sprintf("strm: resolver is temporarily unable to serve this title (status %d)", e.Status)
}
