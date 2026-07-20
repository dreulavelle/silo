package strm

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	if !strings.HasPrefix(u.Path, pluginRoutePrefix) {
		return false
	}

	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

	case resp.StatusCode == http.StatusServiceUnavailable:
		// Conventional "not ready yet / temporarily unavailable" from a
		// resolver. Surface it as-is so the caller can map it to a retryable
		// response rather than a hard failure.
		return "", &ResolverUnavailableError{Status: resp.StatusCode}

	default:
		return "", fmt.Errorf("strm: resolver returned unexpected status %d", resp.StatusCode)
	}
}

// ResolverUnavailableError reports that a resolver was reached but had nothing
// to serve yet. It is distinct from a transport failure because it is expected
// and retryable: the title may simply not be available right now.
type ResolverUnavailableError struct {
	Status int
}

func (e *ResolverUnavailableError) Error() string {
	return fmt.Sprintf("strm: resolver is temporarily unable to serve this title (status %d)", e.Status)
}
