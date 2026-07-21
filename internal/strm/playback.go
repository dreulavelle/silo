package strm

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
)

// RedirectStatus is the status code used to send a client at a resolved stream.
//
// It must stay 302 (or 307). FFmpeg caches 301 and 308 redirects for the life of
// its HTTPContext and does not cache 302/303/307 (libavformat/http.c). Since the
// whole point of a placeholder is that its target is short-lived — a debrid link
// measured in hours — a cached redirect is unrecoverable without restarting
// playback.
//
// The 302 also buys correct seek behaviour for free: when FFmpeg seeks in a way
// that needs a new connection it reverts to the *original* URI rather than the
// previously-resolved location, so every seek re-resolves through us and picks
// up a fresh target.
const RedirectStatus = http.StatusFound

// ServePlaceholder resolves a .strm placeholder and redirects the caller to its
// target. It never streams bytes: the client talks to the origin directly, so
// media never transits this server.
//
// The file is read on every request. That is what lets whatever wrote the
// placeholder repoint it at any time — including between the probe request and
// the playback request — without a rescan or a cache to invalidate.
func ServePlaceholder(w http.ResponseWriter, r *http.Request, filePath string) error {
	target, err := ReadTarget(filePath)
	if err != nil {
		return writePlaceholderError(w, r, filePath, err)
	}

	// A placeholder written by a resolver plugin points at that plugin's route
	// on this host, which no client can reach. Make that hop here and redirect
	// the client to whatever the resolver answers with. External targets pass
	// through untouched.
	target, err = ResolveTarget(r.Context(), target)
	if err != nil {
		return writePlaceholderError(w, r, filePath, err)
	}

	slog.DebugContext(r.Context(), "strm: redirecting to resolved target",
		"component", "strm", "path", filePath, "status", RedirectStatus)

	// Placeholder targets are resolved fresh per request and frequently expire.
	// Caching this redirect anywhere — browser, proxy, CDN — would hand a client
	// a stale target with no way to recover.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")

	http.Redirect(w, r, target, RedirectStatus)
	return nil
}

// writePlaceholderError maps a placeholder failure onto an HTTP response.
//
// Failure modes are distinguished because they mean very different things
// operationally: a missing file is a library/scan race, an empty file is a
// placeholder that was created but never filled in, and a bad scheme is either
// misconfiguration or an attempt at local file disclosure.
func writePlaceholderError(w http.ResponseWriter, r *http.Request, filePath string, err error) error {
	ctx := r.Context()

	var schemeErr *InvalidSchemeError
	var unavailableErr *ResolverUnavailableError
	switch {
	case errors.As(err, &schemeErr):
		// Loud on purpose. In normal operation this cannot happen: whatever
		// writes placeholders validates targets at write time. Reaching here
		// means either a misconfigured writer or someone probing for the
		// arbitrary-file-read that unvalidated .strm handling historically gave
		// away (CVE-2026-35031).
		slog.ErrorContext(ctx, "strm: refusing disallowed target scheme",
			"component", "strm", "path", filePath,
			"scheme", schemeErr.Scheme, "error", err)
		http.Error(w, "invalid stream target", http.StatusBadGateway)

	case errors.Is(err, ErrEmpty):
		// The placeholder exists but has no target yet — expected while an item
		// is requested-but-unresolved. Not an error worth alarming on.
		slog.InfoContext(ctx, "strm: placeholder has no target yet",
			"component", "strm", "path", filePath)
		http.Error(w, "stream not ready", http.StatusServiceUnavailable)

	case errors.As(err, &unavailableErr):
		// The resolver was reached and said it has nothing to serve right now —
		// a title that is not released, not cached, or briefly unavailable.
		// This is the single most common way playback fails and it is not a
		// fault: answering 500 tells a viewer the server is broken and tells a
		// client not to bother retrying, when both are wrong.
		//
		// ResolverUnavailableError exists precisely to carry this distinction.
		// It was being caught by the default branch, so the type was recording
		// something nothing acted on.
		slog.InfoContext(ctx, "strm: resolver cannot serve this yet",
			"component", "strm", "path", filePath,
			"status", unavailableErr.Status, "retry_after", unavailableErr.RetryAfter)
		// Pass the upstream's own backoff hint through when it gave one, so a
		// client waits out a rate limit on the provider's schedule rather than
		// guessing and deepening it.
		if unavailableErr.RetryAfter != "" {
			w.Header().Set("Retry-After", unavailableErr.RetryAfter)
		}
		http.Error(w, "not available yet", http.StatusServiceUnavailable)

	case errors.Is(err, os.ErrNotExist):
		slog.WarnContext(ctx, "strm: placeholder file is missing",
			"component", "strm", "path", filePath)
		http.Error(w, "file not found", http.StatusNotFound)

	case errors.Is(err, ErrNotPlaceholder):
		// Programmer error: something routed a real media file here.
		slog.ErrorContext(ctx, "strm: non-placeholder routed to placeholder handler",
			"component", "strm", "path", filePath)
		http.Error(w, "internal server error", http.StatusInternalServerError)

	default:
		slog.ErrorContext(ctx, "strm: failed to read placeholder",
			"component", "strm", "path", filePath, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}

	return err
}
