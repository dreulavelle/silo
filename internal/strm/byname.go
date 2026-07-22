package strm

import (
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
)

// byNamePrefix is the path prefix of a "by-name" placeholder target.
//
// A resolver plugin can address itself by its STABLE manifest id
// (/api/v1/plugins/by-name/wisp/resolve/...) instead of its MUTABLE numeric
// installation id (/api/v1/plugins/9/resolve/...). The numeric id is minted
// fresh on every plugin upgrade, and a .strm is a durable file, so embedding the
// numeric id strands the whole library on the first upgrade. The by-name form
// carries the one thing that survives an upgrade — the manifest id — and this
// package translates it back to the current numeric route at resolve time.
const byNamePrefix = pluginRoutePrefix + "by-name/" // /api/v1/plugins/by-name/

// pluginIDResolver translates a plugin's stable manifest id ("wisp") into its
// current numeric installation id.
//
// It is injected from cmd/silo at startup precisely so this package never
// imports internal/plugins: the strm follower is a security-sensitive,
// dependency-free component (see isHostLocalPluginRoute), and it must stay that
// way. nil until wired — and nil is treated as a hard, non-retryable error, not
// a guess, because guessing an installation id is exactly the failure mode this
// whole change exists to remove.
var pluginIDResolver atomic.Pointer[func(string) (int, bool)]

// SetPluginIDResolver injects the stable-id to numeric-id lookup. Called once at
// startup with a closure that reads the plugin registry. Mirrors SetHostPort.
func SetPluginIDResolver(fn func(string) (int, bool)) {
	if fn == nil {
		return
	}
	pluginIDResolver.Store(&fn)
}

// ByNameUnresolvedError reports that a by-name placeholder could not be mapped
// to a numeric installation id.
//
// It is deliberately DISTINCT from ResolverUnavailableError. That one means
// "the resolver was reached and cannot serve right now" — reachable, transient,
// worth retrying. This one means the opposite: the plugin is not installed, is
// disabled, is ambiguous, or the resolver was never wired. Retrying cannot fix a
// misconfiguration, so this must never enter the retry path — a doomed retry
// would spin behind a spinner and hide the real cause from the operator.
type ByNameUnresolvedError struct {
	PluginID string
	Reason   string
}

func (e *ByNameUnresolvedError) Error() string {
	if e.PluginID == "" {
		return fmt.Sprintf("strm: cannot route by-name placeholder: %s", e.Reason)
	}
	return fmt.Sprintf("strm: cannot route by-name placeholder for plugin %q: %s", e.PluginID, e.Reason)
}

// isByNameTarget reports whether target's path is the by-name placeholder form
// /api/v1/plugins/by-name/{pid}/<rest>.
//
// A parse failure is reported as "not by-name" rather than an error: the caller
// only reaches here after isHostLocalPluginRoute has already parsed and vetted
// the same URL, so an unparseable target has been rejected upstream and a
// numeric placeholder simply falls through to the existing hop path unchanged.
func isByNameTarget(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	return strings.HasPrefix(u.Path, byNamePrefix)
}

// rewriteByNameTarget turns a by-name placeholder target into the numeric route
// it currently maps to.
//
// The trailing path AND the raw query are preserved verbatim: the HMAC token
// lives in the query as t=, it signs the resolve tuple rather than the URL, and
// dropping or re-encoding it would break authentication at the origin. Only the
// {id} segment is substituted; everything after it is carried over untouched.
//
// A nil resolver, or a plugin id the resolver declines, is a hard
// ByNameUnresolvedError — never a fallback id. The security gate is re-run by
// the caller on the returned URL, so this function does not itself re-validate
// loopback/port/traversal.
func rewriteByNameTarget(target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("strm: parse by-name target: %w", err)
	}

	// u.Path is already decoded and, because isHostLocalPluginRoute vetted it,
	// free of encoded traversal. Split "{pid}/<rest>" off the by-name prefix.
	remainder := strings.TrimPrefix(u.Path, byNamePrefix)
	pid, rest, _ := strings.Cut(remainder, "/")
	if pid == "" {
		return "", &ByNameUnresolvedError{Reason: "no plugin id in placeholder path"}
	}

	resolver := pluginIDResolver.Load()
	if resolver == nil {
		return "", &ByNameUnresolvedError{PluginID: pid, Reason: "no plugin id resolver is configured"}
	}
	id, ok := (*resolver)(pid)
	if !ok {
		// The closure that backs the resolver logs the specific cause
		// (not installed / disabled / ambiguous); here we only know it failed.
		return "", &ByNameUnresolvedError{PluginID: pid, Reason: "plugin is not installed, is disabled, or is ambiguous"}
	}

	// Rebuild /api/v1/plugins/{id}/<rest>. The id comes from %d so it is always a
	// clean integer; RawQuery (token included) is left untouched, so u.String()
	// re-emits it byte-for-byte.
	u.Path = fmt.Sprintf("%s%d/%s", pluginRoutePrefix, id, rest)
	return u.String(), nil
}
