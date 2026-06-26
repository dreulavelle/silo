package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogSearchSettingsFromMapParsesMeilisearchTuning(t *testing.T) {
	settings, err := CatalogSearchSettingsFromMap(map[string]string{
		SearchSettingProvider:                    SearchProviderMeilisearch,
		SearchSettingMeilisearchSyncBatchSize:    "750",
		SearchSettingMeilisearchRebuildBatchSize: "5000",
		SearchSettingMeilisearchRebuildQueue:     "6",
		SearchSettingMeilisearchIndexTypes:       "video,audiobook",
		SearchSettingMeilisearchSemanticEnabled:  "true",
		SearchSettingMeilisearchSemanticRatio:    "0.42",
		SearchSettingMeilisearchEmbedder:         "custom_embedder",
	})
	if err != nil {
		t.Fatalf("CatalogSearchSettingsFromMap returned error: %v", err)
	}
	if settings.SyncBatchSize != 750 {
		t.Fatalf("SyncBatchSize = %d, want 750", settings.SyncBatchSize)
	}
	if settings.RebuildBatchSize != 5000 {
		t.Fatalf("RebuildBatchSize = %d, want 5000", settings.RebuildBatchSize)
	}
	if settings.RebuildQueueDepth != 6 {
		t.Fatalf("RebuildQueueDepth = %d, want 6", settings.RebuildQueueDepth)
	}
	wantTypes := []string{"movie", "series", "audiobook"}
	if !reflect.DeepEqual(settings.IndexTypes, wantTypes) {
		t.Fatalf("IndexTypes = %#v, want %#v", settings.IndexTypes, wantTypes)
	}
	if !settings.SemanticEnabled {
		t.Fatal("SemanticEnabled = false, want true")
	}
	if settings.SemanticRatio != 0.42 {
		t.Fatalf("SemanticRatio = %v, want 0.42", settings.SemanticRatio)
	}
	if settings.Embedder != "custom_embedder" {
		t.Fatalf("Embedder = %q, want custom_embedder", settings.Embedder)
	}
}

func TestCatalogSearchSettingsFromMapRejectsOutOfRangeTuning(t *testing.T) {
	tests := []map[string]string{
		{SearchSettingMeilisearchSyncBatchSize: "0"},
		{SearchSettingMeilisearchRebuildBatchSize: "25001"},
		{SearchSettingMeilisearchRebuildQueue: "0"},
		{SearchSettingMeilisearchIndexTypes: "movie,unknown"},
		{SearchSettingMeilisearchSemanticEnabled: "sometimes"},
		{SearchSettingMeilisearchSemanticRatio: "-0.1"},
		{SearchSettingMeilisearchSemanticRatio: "1.1"},
		{SearchSettingMeilisearchSemanticRatio: "NaN"},
		{SearchSettingMeilisearchEmbedder: "bad.name"},
	}

	for _, values := range tests {
		if _, err := CatalogSearchSettingsFromMap(values); err == nil {
			t.Fatalf("CatalogSearchSettingsFromMap(%v) succeeded, want error", values)
		}
	}
}

func TestActiveCatalogSearchProviderRequiresConfiguredMeilisearch(t *testing.T) {
	if got := ActiveCatalogSearchProvider(DefaultCatalogSearchSettings()); got != SearchProviderPostgres {
		t.Fatalf("default active provider = %q, want postgres", got)
	}
	if got := ActiveCatalogSearchProvider(CatalogSearchSettings{
		Provider: SearchProviderMeilisearch,
	}); got != SearchProviderPostgres {
		t.Fatalf("meilisearch without URL active provider = %q, want postgres", got)
	}
	if got := ActiveCatalogSearchProvider(CatalogSearchSettings{
		Provider:         SearchProviderMeilisearch,
		MeilisearchURL:   "http://localhost:7700",
		MeilisearchIndex: DefaultMeilisearchIndex,
	}); got != SearchProviderMeilisearch {
		t.Fatalf("configured meilisearch active provider = %q, want meilisearch", got)
	}
}

func TestCatalogSearchDocumentVectorsUseEmbedderAndOptOutMissing(t *testing.T) {
	docs := []catalogSearchDocument{
		{ContentID: "movie-1", Title: "One"},
		{ContentID: "movie-2", Title: "Two"},
	}
	count := setCatalogSearchDocumentVectors(docs, map[string][]float32{
		"movie-1": {0.1, 0.2},
	}, "silo_recommendations")
	if count != 1 {
		t.Fatalf("vectorized docs = %d, want 1", count)
	}
	if got := docs[0].Vectors["silo_recommendations"]; !reflect.DeepEqual(got, []float32{0.1, 0.2}) {
		t.Fatalf("doc vector = %#v", got)
	}
	if docs[1].Vectors == nil {
		t.Fatal("missing vector doc should include explicit _vectors opt-out")
	}
	if got, ok := docs[1].Vectors["silo_recommendations"]; !ok || got != nil {
		t.Fatalf("missing vector opt-out = %#v, present=%v; want nil value", got, ok)
	}

	data, err := json.Marshal(docs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"_vectors"`) {
		t.Fatalf("marshaled docs missing _vectors: %s", data)
	}
	if strings.Count(string(data), `"_vectors"`) != 2 {
		t.Fatalf("marshaled docs should include _vectors for both docs: %s", data)
	}
	if !strings.Contains(string(data), `"silo_recommendations":null`) {
		t.Fatalf("marshaled docs missing null vector opt-out: %s", data)
	}
	if got := catalogSearchVectorDocumentCount(docs); got != 1 {
		t.Fatalf("vector document count = %d, want 1", got)
	}
}

func TestCatalogSearchDocumentPayloadBatchesSplitVectorDocs(t *testing.T) {
	vector := make([]float32, 128)
	docs := []catalogSearchDocument{
		{ContentID: "movie-1", Vectors: map[string][]float32{DefaultMeilisearchEmbedder: vector}},
		{ContentID: "movie-2", Vectors: map[string][]float32{DefaultMeilisearchEmbedder: vector}},
		{ContentID: "movie-3", Vectors: map[string][]float32{DefaultMeilisearchEmbedder: vector}},
	}
	maxBytes := estimateCatalogSearchDocumentJSONBytes(docs[0]) + 10
	batches := catalogSearchDocumentPayloadBatches(docs, maxBytes)
	if len(batches) != len(docs) {
		t.Fatalf("batch count = %d, want %d", len(batches), len(docs))
	}
	for idx, batch := range batches {
		if len(batch) != 1 || batch[0].ContentID != docs[idx].ContentID {
			t.Fatalf("batch %d = %#v", idx, batch)
		}
	}
}

func TestMeilisearchSearchRequestBuildsKeywordOnlyByDefault(t *testing.T) {
	provider := &MeilisearchSearchProvider{
		config: MeilisearchProviderConfig{
			MatchingStrategy: DefaultMeilisearchMatchingStrategy,
			SemanticRatio:    DefaultMeilisearchSemanticRatio,
			Embedder:         DefaultMeilisearchEmbedder,
		},
	}
	req, fallback := provider.buildMeilisearchSearchRequest(context.Background(), CatalogSearchRequest{
		Query:     "dune",
		ItemTypes: []string{"movie"},
	})
	if fallback != "" {
		t.Fatalf("fallback = %q, want empty", fallback)
	}
	if req.Vector != nil || req.Hybrid != nil {
		t.Fatalf("keyword request should not include vector or hybrid: %#v", req)
	}
	if req.Filter != `type = "movie"` {
		t.Fatalf("filter = %q, want movie filter", req.Filter)
	}
}

func TestMeilisearchSearchRequestBuildsHybridWhenSemanticEnabled(t *testing.T) {
	vectorizer := &fakeCatalogSearchVectorizer{vector: []float32{0.5, 0.25}}
	provider := &MeilisearchSearchProvider{
		config: MeilisearchProviderConfig{
			MatchingStrategy: DefaultMeilisearchMatchingStrategy,
			SemanticEnabled:  true,
			SemanticRatio:    0.4,
			Embedder:         "silo_recommendations",
			Vectorizer:       vectorizer,
		},
	}
	req, fallback := provider.buildMeilisearchSearchRequest(context.Background(), CatalogSearchRequest{
		Query: "  found family space opera  ",
	})
	if fallback != "" {
		t.Fatalf("fallback = %q, want empty", fallback)
	}
	if req.Hybrid == nil {
		t.Fatal("hybrid request missing")
	}
	if req.Hybrid.Embedder != "silo_recommendations" {
		t.Fatalf("embedder = %q", req.Hybrid.Embedder)
	}
	if req.Hybrid.SemanticRatio != 0.4 {
		t.Fatalf("semantic ratio = %v, want 0.4", req.Hybrid.SemanticRatio)
	}
	if len(req.Vector) != 3072 {
		t.Fatalf("vector len = %d, want 3072", len(req.Vector))
	}
	if vectorizer.calls != 1 || vectorizer.lastQuery != "found family space opera" {
		t.Fatalf("vectorizer calls/query = %d/%q", vectorizer.calls, vectorizer.lastQuery)
	}

	req, fallback = provider.buildMeilisearchSearchRequest(context.Background(), CatalogSearchRequest{
		Query: "found   family space opera",
	})
	if fallback != "" || req.Hybrid == nil {
		t.Fatalf("cached hybrid fallback/request = %q/%#v", fallback, req.Hybrid)
	}
	if vectorizer.calls != 1 {
		t.Fatalf("cached query should not call vectorizer again, calls = %d", vectorizer.calls)
	}
}

func TestMeilisearchSearchRequestStaysKeywordWhenCoverageNotReady(t *testing.T) {
	vectorizer := &fakeCatalogSearchVectorizer{vector: []float32{0.5, 0.25}}
	provider := &MeilisearchSearchProvider{
		config: MeilisearchProviderConfig{
			MatchingStrategy: DefaultMeilisearchMatchingStrategy,
			SemanticEnabled:  true,
			SemanticRatio:    0.4,
			Embedder:         "silo_recommendations",
			Vectorizer:       vectorizer,
			Coverage:         fakeCoverageGate{ready: false, reason: `type "movie" coverage 40% below threshold`},
		},
	}
	req, fallback := provider.buildMeilisearchSearchRequest(context.Background(), CatalogSearchRequest{
		Query:     "found family space opera",
		ItemTypes: []string{"movie"},
	})
	if req.Vector != nil || req.Hybrid != nil {
		t.Fatalf("not-ready coverage should stay keyword-only: %#v", req)
	}
	if fallback != `semantic_not_ready: type "movie" coverage 40% below threshold` {
		t.Fatalf("fallback = %q, want semantic_not_ready diagnostic", fallback)
	}
	if vectorizer.calls != 0 {
		t.Fatalf("not-ready coverage should not call vectorizer, calls = %d", vectorizer.calls)
	}
}

func TestMeilisearchSearchRequestBuildsHybridWhenCoverageReady(t *testing.T) {
	vectorizer := &fakeCatalogSearchVectorizer{vector: []float32{0.5, 0.25}}
	provider := &MeilisearchSearchProvider{
		config: MeilisearchProviderConfig{
			MatchingStrategy: DefaultMeilisearchMatchingStrategy,
			SemanticEnabled:  true,
			SemanticRatio:    0.4,
			Embedder:         "silo_recommendations",
			Vectorizer:       vectorizer,
			Coverage:         fakeCoverageGate{ready: true},
		},
	}
	req, fallback := provider.buildMeilisearchSearchRequest(context.Background(), CatalogSearchRequest{
		Query:     "found family space opera",
		ItemTypes: []string{"movie"},
	})
	if fallback != "" {
		t.Fatalf("fallback = %q, want empty", fallback)
	}
	if req.Hybrid == nil || req.Vector == nil {
		t.Fatalf("ready coverage should emit hybrid request: %#v", req)
	}
}

func TestSemanticModelProviderUsesCommaOkSafety(t *testing.T) {
	if got := semanticModelProvider(nil); got != nil {
		t.Fatalf("nil vectorizer should yield nil model provider, got %#v", got)
	}
	// A vectorizer that does NOT implement CatalogSemanticModelProvider must not
	// be asserted into the interface (a nil-interface assertion would panic).
	if got := semanticModelProvider(&fakeCatalogSearchVectorizer{}); got != nil {
		t.Fatalf("non-implementing vectorizer should yield nil, got %#v", got)
	}
	impl := &fakeModelVectorizer{}
	if got := semanticModelProvider(impl); got != CatalogSemanticModelProvider(impl) {
		t.Fatalf("implementing vectorizer should yield itself, got %#v", got)
	}
}

func TestMeilisearchSearchRequestBuildsHybridForApproximateInteractiveSearch(t *testing.T) {
	vectorizer := &fakeCatalogSearchVectorizer{vector: []float32{0.5, 0.25}}
	provider := &MeilisearchSearchProvider{
		config: MeilisearchProviderConfig{
			MatchingStrategy: DefaultMeilisearchMatchingStrategy,
			SemanticEnabled:  true,
			SemanticRatio:    0.4,
			Embedder:         "silo_recommendations",
			Vectorizer:       vectorizer,
		},
	}
	req, fallback := provider.buildMeilisearchSearchRequest(context.Background(), CatalogSearchRequest{
		Query:     "found family space opera",
		SkipTotal: true,
	})
	if fallback != "" {
		t.Fatalf("fallback = %q, want empty", fallback)
	}
	if req.Vector == nil || req.Hybrid == nil {
		t.Fatalf("approximate interactive search should include hybrid search: %#v", req)
	}
	if vectorizer.calls != 1 {
		t.Fatalf("interactive search should call vectorizer once, calls = %d", vectorizer.calls)
	}
}

func TestMeilisearchSearchRequestSkipsHybridForShortTitleSearch(t *testing.T) {
	vectorizer := &fakeCatalogSearchVectorizer{vector: []float32{0.5, 0.25}}
	provider := &MeilisearchSearchProvider{
		config: MeilisearchProviderConfig{
			MatchingStrategy: DefaultMeilisearchMatchingStrategy,
			SemanticEnabled:  true,
			SemanticRatio:    0.4,
			Embedder:         "silo_recommendations",
			Vectorizer:       vectorizer,
		},
	}
	for _, query := range []string{"sponge", "spongebob square"} {
		req, fallback := provider.buildMeilisearchSearchRequest(context.Background(), CatalogSearchRequest{
			Query: query,
		})
		if fallback != "" {
			t.Fatalf("fallback for %q = %q, want empty", query, fallback)
		}
		if req.Vector != nil || req.Hybrid != nil {
			t.Fatalf("short title search %q should stay keyword-only: %#v", query, req)
		}
	}
	if vectorizer.calls != 0 {
		t.Fatalf("short title searches should not call vectorizer, calls = %d", vectorizer.calls)
	}
}

func TestMeilisearchSearchRequestFallsBackWhenEmbeddingFails(t *testing.T) {
	provider := &MeilisearchSearchProvider{
		config: MeilisearchProviderConfig{
			MatchingStrategy: DefaultMeilisearchMatchingStrategy,
			SemanticEnabled:  true,
			SemanticRatio:    DefaultMeilisearchSemanticRatio,
			Embedder:         DefaultMeilisearchEmbedder,
			Vectorizer:       &fakeCatalogSearchVectorizer{err: errors.New("embedding offline")},
		},
	}
	req, fallback := provider.buildMeilisearchSearchRequest(context.Background(), CatalogSearchRequest{
		Query: "something vague atmospheric",
	})
	if req.Vector != nil || req.Hybrid != nil {
		t.Fatalf("fallback request should be keyword-only: %#v", req)
	}
	if !strings.Contains(fallback, "semantic query embedding failed") {
		t.Fatalf("fallback = %q, want semantic query failure", fallback)
	}
}

func TestMeilisearchCircuitTripsOnServerAndDecodeErrors(t *testing.T) {
	provider := &MeilisearchSearchProvider{}
	if !provider.shouldTripCircuit(&meilisearchHTTPError{StatusCode: 500}) {
		t.Fatal("HTTP 500 should trip circuit")
	}
	if !provider.shouldTripCircuit(&meilisearchDecodeError{Err: errors.New("bad json")}) {
		t.Fatal("decode failure should trip circuit")
	}
	if provider.shouldTripCircuit(context.Canceled) {
		t.Fatal("context.Canceled should not trip circuit")
	}
}

func TestMeilisearchProviderFallsBackWhenScopedIndexCannotSatisfyRequest(t *testing.T) {
	provider := &MeilisearchSearchProvider{
		config: MeilisearchProviderConfig{
			IndexTypes: []string{"movie", "series"},
		},
		fallback: &PostgresSearchProvider{},
	}

	if !provider.indexCoversRequest([]string{"movie"}) {
		t.Fatal("movie request should be covered by movie/series index")
	}
	if provider.indexCoversRequest([]string{"audiobook"}) {
		t.Fatal("audiobook request should not be covered by movie/series index")
	}
	if provider.indexCoversRequest(nil) {
		t.Fatal("unscoped request should not be covered by a scoped index")
	}
}

func TestMeilisearchProviderAllowsUnscopedIndexForAnyRequest(t *testing.T) {
	provider := &MeilisearchSearchProvider{}
	for _, itemTypes := range [][]string{nil, []string{"movie"}, []string{"audiobook", "ebook"}} {
		if !provider.indexCoversRequest(itemTypes) {
			t.Fatalf("unscoped index should cover request %#v", itemTypes)
		}
	}
}

func TestNormalizeCatalogSearchIndexTypesValueFormatsCanonicalList(t *testing.T) {
	itemTypes, err := NormalizeCatalogSearchIndexTypesValue("video, movie, ebook")
	if err != nil {
		t.Fatalf("NormalizeCatalogSearchIndexTypesValue returned error: %v", err)
	}
	want := "movie,series,ebook"
	if got := FormatCatalogSearchIndexTypesValue(itemTypes); got != want {
		t.Fatalf("formatted index types = %q, want %q", got, want)
	}
}

func TestMeilisearchSchemaVersionChangesWithEmbedder(t *testing.T) {
	defaultVersion := catalogSearchMeilisearchSchemaVersion(DefaultMeilisearchEmbedder, nil)
	customVersion := catalogSearchMeilisearchSchemaVersion("custom_embedder", nil)
	if defaultVersion == customVersion {
		t.Fatal("schema version should change when embedder changes")
	}
	if defaultVersion/1_000_000 != SearchMeilisearchSchemaVersion {
		t.Fatalf("base schema version = %d, want %d", defaultVersion/1_000_000, SearchMeilisearchSchemaVersion)
	}
}

func TestMeilisearchSchemaVersionChangesWithIndexTypes(t *testing.T) {
	allTypesVersion := catalogSearchMeilisearchSchemaVersion(DefaultMeilisearchEmbedder, nil)
	videoOnlyVersion := catalogSearchMeilisearchSchemaVersion(DefaultMeilisearchEmbedder, []string{"movie", "series"})
	if allTypesVersion == videoOnlyVersion {
		t.Fatal("schema version should change when indexed media scope changes")
	}
}

func TestPostgresSearchProviderNilRepoStillErrors(t *testing.T) {
	provider := NewPostgresSearchProvider(nil)
	if _, err := provider.Search(context.Background(), CatalogSearchRequest{}); err == nil {
		t.Fatal("nil item repo should still return an error")
	}
}

type fakeCatalogSearchVectorizer struct {
	vector    []float32
	err       error
	calls     int
	lastQuery string
}

func (f *fakeCatalogSearchVectorizer) EmbedSearchQuery(_ context.Context, query string) ([]float32, error) {
	f.calls++
	f.lastQuery = query
	if f.err != nil {
		return nil, f.err
	}
	return append([]float32(nil), f.vector...), nil
}

// fakeCoverageGate is a hot-path gate stub: it returns a fixed readiness verdict
// regardless of item types, so buildMeilisearchSearchRequest's gating can be
// exercised without a live coverage tracker.
type fakeCoverageGate struct {
	ready  bool
	reason string
}

func (g fakeCoverageGate) CoverageReady(_ []string) (bool, string) {
	return g.ready, g.reason
}

// fakeModelVectorizer implements both CatalogSearchQueryVectorizer and
// CatalogSemanticModelProvider so semanticModelProvider's comma-ok assertion can
// be verified against a type that does satisfy the model interface.
type fakeModelVectorizer struct {
	fakeCatalogSearchVectorizer
	model string
}

func (f *fakeModelVectorizer) ActiveEmbeddingModel(_ context.Context) (string, error) {
	return f.model, nil
}
