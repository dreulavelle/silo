package scanpush

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Silo-Server/silo-server/internal/autoscan"
	"github.com/Silo-Server/silo-server/internal/events"
)

type stubSources struct {
	sources []autoscan.Source
	err     error
}

func (s *stubSources) ListSources(context.Context) ([]autoscan.Source, error) {
	return s.sources, s.err
}

type stubIngest struct {
	mu   sync.Mutex
	call []autoscan.ChangeIngest
}

func (s *stubIngest) IngestChanges(_ context.Context, in autoscan.ChangeIngest) (autoscan.IngestResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.call = append(s.call, in)
	return autoscan.IngestResult{Enqueued: len(in.Changes)}, nil
}

func (s *stubIngest) calls() []autoscan.ChangeIngest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]autoscan.ChangeIngest(nil), s.call...)
}

func envelope(t *testing.T, event string, p Payload) events.Envelope {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return events.Envelope{Channel: events.ChannelPlugins, Event: event, Data: raw}
}

// The publishing plugin's id is taken from the event name because the host
// stamps it there itself — a plugin cannot choose what it is stamped with, so
// this doubles as the authorization check.
func TestPluginIDForEvent(t *testing.T) {
	if id, ok := PluginIDForEvent("plugin.wisp.scan.changes"); !ok || id != "wisp" {
		t.Errorf("got %q, %v; want wisp, true", id, ok)
	}
	for _, name := range []string{
		"plugin.scan.changes",            // no id
		"scan.changes",                   // unprefixed
		"plugin.wisp.scan.other",         // different event
		"plugin.a.b.scan.changes",        // dotted id: ambiguous, must not match
		"plugin.wisp.scan.changes.extra", // suffix not terminal
		"",
	} {
		if id, ok := PluginIDForEvent(name); ok {
			t.Errorf("PluginIDForEvent(%q) = %q, true; want it rejected", name, id)
		}
	}
}

// A push carries paths; a source is resolved from the plugin id. A plugin must
// never be able to push into a source that is not its own.
func TestPushOnlyReachesItsOwnSources(t *testing.T) {
	ingest := &stubIngest{}
	c := New(nil, &stubSources{sources: []autoscan.Source{
		{ID: "mine", PluginID: "wisp", Enabled: true},
		{ID: "someone-else", PluginID: "other-plugin", Enabled: true},
	}}, ingest, discardLogger())

	c.handle(context.Background(), envelope(t, "plugin.wisp.scan.changes", Payload{
		SourcePaths: []string{"/library/movies/A/A.strm"},
		Scope:       "file",
	}))

	calls := ingest.calls()
	if len(calls) != 1 {
		t.Fatalf("ingested into %d source(s), want 1", len(calls))
	}
	if calls[0].SourceID != "mine" {
		t.Errorf("ingested into %q; a plugin must only reach its own sources", calls[0].SourceID)
	}
}

// A disabled source is an operator saying "not now". A push must respect that
// exactly as a poll would.
func TestDisabledSourcesAreSkipped(t *testing.T) {
	ingest := &stubIngest{}
	c := New(nil, &stubSources{sources: []autoscan.Source{
		{ID: "off", PluginID: "wisp", Enabled: false},
	}}, ingest, discardLogger())

	c.handle(context.Background(), envelope(t, "plugin.wisp.scan.changes", Payload{
		SourcePaths: []string{"/library/movies/A/A.strm"},
	}))

	if got := len(ingest.calls()); got != 0 {
		t.Errorf("ingested %d time(s) into a disabled source", got)
	}
}

// Blank and duplicate paths must not reach autoscan: a chatty plugin should not
// be able to enqueue the same scan repeatedly.
func TestPayloadDropsBlanksAndDuplicates(t *testing.T) {
	got := Payload{
		SourcePaths: []string{"/a.strm", "", "  ", "/a.strm", "/b.strm", " /b.strm "},
		Scope:       "file",
	}.changes()

	if len(got) != 2 {
		t.Fatalf("got %d changes, want 2: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Scope != autoscan.ChangeScopeFile {
			t.Errorf("scope = %q, want file", c.Scope)
		}
	}
}

// An unrecognised or omitted scope becomes a subtree scan. Over-scanning
// slightly is recoverable; wrongly claiming file scope on a directory silently
// misses everything inside it.
func TestUnknownScopeFallsBackToSubtree(t *testing.T) {
	for _, scope := range []string{"", "  ", "directory", "nonsense"} {
		got := Payload{SourcePaths: []string{"/a"}, Scope: scope}.changes()
		if len(got) != 1 || got[0].Scope != autoscan.ChangeScopeSubtree {
			t.Errorf("scope %q produced %+v, want a subtree change", scope, got)
		}
	}
}

// Events from other channels, other plugins' event names, and unreadable
// payloads must all be ignored without touching autoscan.
func TestIrrelevantEventsAreIgnored(t *testing.T) {
	ingest := &stubIngest{}
	c := New(nil, &stubSources{sources: []autoscan.Source{
		{ID: "mine", PluginID: "wisp", Enabled: true},
	}}, ingest, discardLogger())
	ctx := context.Background()

	// Right event, wrong channel.
	c.handle(ctx, events.Envelope{
		Channel: events.ChannelCatalog,
		Event:   "plugin.wisp." + EventSuffix,
		Data:    json.RawMessage(`{"source_paths":["/a"]}`),
	})
	// Right channel, unrelated event.
	c.handle(ctx, envelope(t, "plugin.wisp.something.else", Payload{SourcePaths: []string{"/a"}}))
	// Right shape, unreadable payload.
	c.handle(ctx, events.Envelope{
		Channel: events.ChannelPlugins,
		Event:   "plugin.wisp." + EventSuffix,
		Data:    json.RawMessage(`{not json`),
	})
	// Right shape, no paths.
	c.handle(ctx, envelope(t, "plugin.wisp."+EventSuffix, Payload{}))

	if got := len(ingest.calls()); got != 0 {
		t.Errorf("ingested %d time(s) for events that should have been ignored", got)
	}
}

// A source lookup failure must not take the process down or ingest blindly.
func TestSourceLookupFailureIsSurvivable(t *testing.T) {
	ingest := &stubIngest{}
	c := New(nil, &stubSources{err: context.DeadlineExceeded}, ingest, discardLogger())

	c.handle(context.Background(), envelope(t, "plugin.wisp."+EventSuffix, Payload{
		SourcePaths: []string{"/a.strm"},
	}))

	if got := len(ingest.calls()); got != 0 {
		t.Errorf("ingested %d time(s) despite being unable to resolve a source", got)
	}
}
