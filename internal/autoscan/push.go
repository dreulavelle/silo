package autoscan

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// IngestPush accepts changes a scan_source plugin reported of its own accord.
//
// This is a sibling of IngestChanges, not a variant of it. A webhook delivery
// comes from outside, over an authenticated HTTP endpoint, from a provider that
// may never speak again — so it is persisted and retried, and it requires the
// source to be in webhook mode. A push comes from a plugin the host is already
// connected to, about files that plugin just wrote itself. Those are different
// enough that sharing the entry point meant lying about one of them.
//
// Concretely, three things differ:
//
//   - Delivery mode is not constrained. A push is orthogonal to how the source
//     is polled; a source can be push-fed AND polled, which is the intended
//     arrangement — push for latency, poll as the backstop.
//   - Nothing is persisted for retry. If a push fails the files are still on
//     disk and the next poll finds them, whereas a retry queue would re-scan
//     paths that a poll had meanwhile already handled.
//   - The event records the source's real delivery mode, so the admin view
//     does not show a poll source reporting webhook activity.
//
// The actual work — path rewriting, resolving paths to library folders,
// suppression and enqueueing — is the same consumeSourceChanges the poll path
// uses, so a pushed change and a polled one are treated identically once
// accepted.
func (s *Service) IngestPush(ctx context.Context, in ChangeIngest) (IngestResult, error) {
	if len(in.Changes) == 0 {
		return IngestResult{}, nil
	}

	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	if !settings.Enabled {
		// Autoscan being switched off is an operator decision, and a push must
		// respect it exactly as a poll does.
		return IngestResult{}, errWebhookDeliveryDisabled
	}

	src, err := s.store.GetSource(ctx, in.SourceID)
	if err != nil {
		return IngestResult{}, err
	}
	if !src.Enabled {
		return IngestResult{}, errWebhookDeliveryDisabled
	}

	startedAt := in.ReceivedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}

	// Bookkeeping failure must not block the scan itself, matching the poll
	// path: eventID 0 makes finishEvent a no-op and enqueues without a link.
	eventID, err := s.store.CreateEvent(ctx, EventCreate{
		SourceID:          src.ID,
		PluginID:          src.PluginID,
		CapabilityID:      src.CapabilityID,
		StartedAt:         startedAt,
		DeliveryMode:      src.DeliveryMode,
		ProviderEventType: in.ProviderEventType,
		SkipRunningCheck:  true,
	})
	if err != nil {
		slog.WarnContext(ctx, "autoscan: create push event failed",
			"component", "autoscan", "source_id", src.ID, "err", err)
		eventID = 0
	}

	result, cerr := s.consumeSourceChanges(ctx, src, in.Changes, consumeOptions{
		EventID: eventID,
		TTL:     time.Duration(settings.DebounceSeconds) * time.Second,
	})
	if cerr != nil {
		return IngestResult{}, fmt.Errorf("autoscan: consume pushed changes: %w", cerr)
	}
	return IngestResult{
		Enqueued:   result.Enqueue.Created + result.Enqueue.Reused,
		Suppressed: result.Stats.Suppressed,
		Unresolved: result.Status == EventStatusUnresolved,
	}, nil
}
