// Package scanpush lets a scan_source plugin tell the host that files have
// appeared, instead of waiting to be asked.
//
// Autoscan polls its sources on a shared interval, defaulting to ten minutes.
// That is a sensible way to notice changes made by something external — an arr
// importing a download, a user dropping a file on a share. It is a poor fit for
// a plugin that WRITES the files it reports: it knows the exact moment each one
// lands, and the poll interval becomes a floor on how long that file stays
// invisible. For an on-demand library, where a request is supposed to become a
// playable item immediately, that delay IS the user-visible latency.
//
// The push travels over the gRPC connection a plugin already has, via
// RuntimeHost.PublishEvent, rather than an HTTP webhook. That avoids a token to
// mint, store, rotate and leak; avoids a second network path with its own
// failure modes; and avoids plugins having to imitate a Sonarr payload to be
// understood. It is also authenticated by construction: the host stamps the
// publishing plugin's id itself rather than reading it from the request, so a
// plugin cannot push on another's behalf.
//
// Polling is deliberately left in place underneath. A push can be missed — the
// plugin may crash between writing a file and reporting it, or the host may be
// restarting — and a slow poll is the backstop that eventually notices. Push is
// for latency; poll is for eventual correctness.
//
// This package is fork-owned. See FORK.md.
package scanpush

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/autoscan"
	"github.com/Silo-Server/silo-server/internal/events"
)

// EventSuffix is the event name a plugin publishes to report new files.
//
// The host prefixes it with "plugin.<plugin_id>.", so a plugin publishing
// "scan.changes" arrives here as "plugin.wisp.scan.changes". That prefix is how
// the source is identified, and it is stamped server-side.
const EventSuffix = "scan.changes"

// ingestTimeout bounds one ingest. Generous: the work is a database write plus
// an enqueue, and the alternative to finishing is a file that stays invisible
// until the next poll.
const ingestTimeout = 30 * time.Second

// Payload is what a plugin sends.
//
// Deliberately the same shape autoscan already uses internally, so nothing is
// inferred from it. A plugin states the paths it wrote and what each one is.
type Payload struct {
	SourcePaths []string `json:"source_paths"`
	// Scope is "file" or "subtree"; empty means subtree. Subtree is the
	// conservative default — it over-scans slightly, whereas wrongly claiming
	// file scope on a directory silently misses everything inside it.
	Scope string `json:"scope"`
}

// sourceLister finds the autoscan sources belonging to a plugin.
type sourceLister interface {
	ListSources(ctx context.Context) ([]autoscan.Source, error)
}

// ingester accepts changes into autoscan's existing pipeline, which owns path
// rewriting, folder resolution, suppression and scan enqueueing.
//
// Deliberately IngestPush rather than IngestChanges: the latter is the webhook
// path, which requires the source to be in webhook mode and persists a retrying
// delivery record. A push is neither — it comes from a plugin already connected
// to this host about files it just wrote, and if it fails the next poll finds
// them anyway.
type ingester interface {
	IngestPush(ctx context.Context, in autoscan.ChangeIngest) (autoscan.IngestResult, error)
}

// Consumer routes plugin-published change events into autoscan.
type Consumer struct {
	hub     *events.Hub
	sources sourceLister
	ingest  ingester
	log     *slog.Logger

	mu          sync.Mutex
	unsubscribe func()
}

// New builds a consumer. A nil hub, lister or ingester makes Start a no-op, so
// a deployment without autoscan wired up simply does not push.
func New(hub *events.Hub, sources sourceLister, ingest ingester, log *slog.Logger) *Consumer {
	if log == nil {
		log = slog.Default()
	}
	return &Consumer{hub: hub, sources: sources, ingest: ingest, log: log}
}

// Start subscribes to the event hub and consumes until ctx is cancelled.
func (c *Consumer) Start(ctx context.Context) {
	if c == nil || c.hub == nil || c.sources == nil || c.ingest == nil {
		return
	}

	ch, unsubscribe := c.hub.Subscribe()
	c.mu.Lock()
	c.unsubscribe = unsubscribe
	c.mu.Unlock()

	go func() {
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case env, ok := <-ch:
				if !ok {
					return
				}
				c.handle(ctx, env)
			}
		}
	}()
}

// Stop releases the subscription.
func (c *Consumer) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.unsubscribe != nil {
		c.unsubscribe()
		c.unsubscribe = nil
	}
}

// handle processes one envelope, ignoring everything that is not a scan push.
func (c *Consumer) handle(ctx context.Context, env events.Envelope) {
	if env.Channel != events.ChannelPlugins {
		return
	}
	pluginID, ok := PluginIDForEvent(env.Event)
	if !ok {
		return
	}

	var payload Payload
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &payload); err != nil {
			c.log.Warn("scanpush: unreadable payload", "plugin_id", pluginID, "err", err)
			return
		}
	}

	changes := payload.changes()
	if len(changes) == 0 {
		return
	}

	// The push names no source: a plugin knows what it wrote, not how an
	// operator chose to configure autoscan around it. Resolving the source here
	// keeps that mapping where it belongs, and means a plugin cannot push into
	// a source that is not its own.
	sources, err := c.sources.ListSources(ctx)
	if err != nil {
		c.log.Warn("scanpush: could not list autoscan sources", "plugin_id", pluginID, "err", err)
		return
	}

	matched := 0
	for _, src := range sources {
		if !strings.EqualFold(strings.TrimSpace(src.PluginID), pluginID) {
			continue
		}
		matched++
		if !src.Enabled {
			continue
		}
		c.ingestInto(ctx, src, pluginID, changes)
	}

	if matched == 0 {
		// Worth saying: the plugin is doing its part and nothing is listening,
		// which looks from the outside like the push silently not working.
		c.log.Info("scanpush: no autoscan source is bound to this plugin, so its changes go unused until the next poll",
			"plugin_id", pluginID, "paths", len(changes))
	}
}

func (c *Consumer) ingestInto(ctx context.Context, src autoscan.Source, pluginID string, changes []autoscan.Change) {
	ingestCtx, cancel := context.WithTimeout(ctx, ingestTimeout)
	defer cancel()

	result, err := c.ingest.IngestPush(ingestCtx, autoscan.ChangeIngest{
		SourceID:          src.ID,
		ProviderEventType: EventSuffix,
		Changes:           changes,
		ReceivedAt:        time.Now(),
	})
	if err != nil {
		c.log.Warn("scanpush: ingest failed", "plugin_id", pluginID, "source_id", src.ID, "err", err)
		return
	}
	c.log.Info("scanpush: ingested plugin-reported changes",
		"plugin_id", pluginID, "source_id", src.ID,
		"paths", len(changes), "enqueued", result.Enqueued, "suppressed", result.Suppressed)
}

// changes converts a payload into autoscan changes, dropping blanks and
// duplicates so a chatty plugin cannot enqueue the same path repeatedly.
func (p Payload) changes() []autoscan.Change {
	scope := autoscan.ChangeScopeSubtree
	if strings.EqualFold(strings.TrimSpace(p.Scope), string(autoscan.ChangeScopeFile)) {
		scope = autoscan.ChangeScopeFile
	}

	seen := make(map[string]struct{}, len(p.SourcePaths))
	out := make([]autoscan.Change, 0, len(p.SourcePaths))
	for _, raw := range p.SourcePaths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, autoscan.Change{SourcePath: path, Scope: scope})
	}
	return out
}

// PluginIDForEvent extracts the publishing plugin's id from an event name of
// the form "plugin.<plugin_id>.scan.changes", reporting false for anything
// else.
//
// The id is taken from the name because the host puts it there itself; a plugin
// cannot choose what it is stamped with, so this doubles as the authorization
// check.
func PluginIDForEvent(name string) (string, bool) {
	const prefix = "plugin."
	suffix := "." + EventSuffix

	// The length check is load-bearing, not defensive: for a name like
	// "plugin.scan.changes" the prefix and suffix overlap, and slicing between
	// them panics.
	if len(name) <= len(prefix)+len(suffix) {
		return "", false
	}
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return "", false
	}
	id := name[len(prefix) : len(name)-len(suffix)]
	if id == "" || strings.Contains(id, ".") {
		return "", false
	}
	return id, true
}

// discardLogger is used by tests that assert on behaviour rather than output.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
