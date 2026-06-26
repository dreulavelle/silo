package catalog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/embeddingvectors"
	"github.com/Silo-Server/silo-server/internal/models"
)

const (
	meilisearchDefaultBatchSize        = 100
	meilisearchDefaultCandidateScanCap = 1000
	meilisearchDefaultDeepOffsetLimit  = 500
	meilisearchCircuitCooldown         = 30 * time.Second
	meilisearchQueryVectorCacheTTL     = 15 * time.Minute
	meilisearchQueryVectorCacheMax     = 1024
)

type MeilisearchProviderConfig struct {
	URL              string
	APIKey           string
	Index            string
	Timeout          time.Duration
	MatchingStrategy string
	BatchSize        int
	CandidateScanCap int
	DeepOffsetLimit  int
	CircuitCooldown  time.Duration
	IndexTypes       []string
	SemanticEnabled  bool
	SemanticRatio    float64
	Embedder         string
	Vectorizer       CatalogSearchQueryVectorizer
	Coverage         SemanticCoverageGate
}

type MeilisearchSearchProvider struct {
	itemRepo  *ItemRepository
	stateRepo *SearchIndexEventRepository
	fallback  *PostgresSearchProvider
	client    *meilisearchClient
	config    MeilisearchProviderConfig

	mu              sync.Mutex
	unhealthyUntil  time.Time
	unhealthyReason string
	lastFallback    string
	vectorCache     map[string]cachedCatalogSearchQueryVector
	vectorCacheSeq  int64
}

type cachedCatalogSearchQueryVector struct {
	vector    []float32
	expiresAt time.Time
	seq       int64
}

func NewMeilisearchSearchProvider(
	itemRepo *ItemRepository,
	stateRepo *SearchIndexEventRepository,
	fallback *PostgresSearchProvider,
	config MeilisearchProviderConfig,
) (*MeilisearchSearchProvider, error) {
	if fallback == nil {
		fallback = NewPostgresSearchProvider(itemRepo)
	}
	if config.Index == "" {
		config.Index = DefaultMeilisearchIndex
	}
	if config.Timeout <= 0 {
		config.Timeout = time.Duration(DefaultMeilisearchTimeoutMS) * time.Millisecond
	}
	if config.MatchingStrategy == "" {
		config.MatchingStrategy = DefaultMeilisearchMatchingStrategy
	}
	if config.BatchSize <= 0 {
		config.BatchSize = meilisearchDefaultBatchSize
	}
	if config.CandidateScanCap <= 0 {
		config.CandidateScanCap = meilisearchDefaultCandidateScanCap
	}
	if config.DeepOffsetLimit <= 0 {
		config.DeepOffsetLimit = meilisearchDefaultDeepOffsetLimit
	}
	if config.CircuitCooldown <= 0 {
		config.CircuitCooldown = meilisearchCircuitCooldown
	}
	config.IndexTypes = normalizeCatalogSearchItemTypes(config.IndexTypes)
	if config.SemanticRatio < 0 || config.SemanticRatio > 1 {
		config.SemanticRatio = DefaultMeilisearchSemanticRatio
	}
	embedder, err := NormalizeCatalogSearchEmbedderName(config.Embedder)
	if err != nil {
		return nil, err
	}
	config.Embedder = embedder
	client, err := newMeilisearchClient(config.URL, config.APIKey, config.Timeout)
	if err != nil {
		return nil, err
	}
	return &MeilisearchSearchProvider{
		itemRepo:    itemRepo,
		stateRepo:   stateRepo,
		fallback:    fallback,
		client:      client,
		config:      config,
		vectorCache: make(map[string]cachedCatalogSearchQueryVector),
	}, nil
}

func (p *MeilisearchSearchProvider) Search(ctx context.Context, req CatalogSearchRequest) (*CatalogSearchResult, error) {
	if p == nil || p.client == nil {
		return p.fallbackSearch(ctx, req, "meilisearch not configured")
	}
	if reason, blocked := p.circuitBlocked(time.Now()); blocked {
		return p.fallbackSearch(ctx, req, reason)
	}
	if p.stateRepo == nil {
		return p.fallbackSearch(ctx, req, "meilisearch index state is not configured")
	}
	if !p.indexCoversRequest(req.ItemTypes) {
		return p.fallbackSearch(ctx, req, "meilisearch index does not cover requested media scope")
	}
	if req.Offset > p.config.DeepOffsetLimit {
		return p.fallbackSearch(ctx, req, "deep offset exceeds meilisearch scan policy")
	}
	state, err := p.stateRepo.GetState(ctx, SearchProviderMeilisearch)
	if err != nil {
		p.markFallback("index state unavailable")
		return p.fallback.Search(ctx, req)
	}
	if strings.TrimSpace(state.ActiveIndexUID) == "" {
		return p.fallbackSearch(ctx, req, "meilisearch index has not been built")
	}
	if state.SchemaVersion != catalogSearchMeilisearchSchemaVersion(p.config.Embedder, p.config.IndexTypes) {
		return p.fallbackSearch(ctx, req, "meilisearch index schema mismatch")
	}
	pending, err := p.stateRepo.PendingCount(ctx, SearchProviderMeilisearch)
	if err != nil {
		p.markFallback("index pending state unavailable")
		return p.fallback.Search(ctx, req)
	}
	if pending > 0 {
		return p.fallbackSearch(ctx, req, "meilisearch index has pending updates")
	}

	result, err := p.searchMeilisearch(ctx, req, state.ActiveIndexUID)
	if err != nil {
		if p.shouldTripCircuit(err) {
			p.tripCircuit(err)
		}
		return p.fallbackSearch(ctx, req, err.Error())
	}
	if result.FallbackReason != "" {
		p.markFallback(result.FallbackReason)
	} else {
		p.clearFallback()
	}
	return result, nil
}

func (p *MeilisearchSearchProvider) indexCoversRequest(itemTypes []string) bool {
	if p == nil || len(p.config.IndexTypes) == 0 {
		return true
	}
	requested := normalizeCatalogSearchItemTypes(itemTypes)
	if len(requested) == 0 {
		return false
	}
	covered := make(map[string]struct{}, len(p.config.IndexTypes))
	for _, itemType := range p.config.IndexTypes {
		covered[itemType] = struct{}{}
	}
	for _, itemType := range requested {
		if _, ok := covered[itemType]; !ok {
			return false
		}
	}
	return true
}

func (p *MeilisearchSearchProvider) searchMeilisearch(ctx context.Context, req CatalogSearchRequest, indexUID string) (*CatalogSearchResult, error) {
	target := req.Offset + req.Limit + 1
	if target <= 0 {
		target = 1
	}
	batchSize := p.config.BatchSize
	if batchSize < req.Limit+1 {
		batchSize = req.Limit + 1
	}
	if batchSize <= 0 {
		batchSize = meilisearchDefaultBatchSize
	}

	var accessible []*models.MediaItem
	meiliOffset := 0
	scanned := 0
	estimatedTotalHits := 0
	exhausted := false
	baseSearchReq, semanticFallback := p.buildMeilisearchSearchRequest(ctx, req)

	for len(accessible) < target && !exhausted {
		if scanned >= p.config.CandidateScanCap {
			return nil, fmt.Errorf("meilisearch candidate scan cap reached")
		}
		nextLimit := batchSize
		if remaining := p.config.CandidateScanCap - scanned; remaining < nextLimit {
			nextLimit = remaining
		}
		searchReq := baseSearchReq
		searchReq.Offset = meiliOffset
		searchReq.Limit = nextLimit
		resp, err := p.client.Search(ctx, indexUID, searchReq)
		if err != nil && baseSearchReq.Hybrid != nil {
			semanticFallback = "meilisearch hybrid search failed: " + err.Error()
			baseSearchReq.Vector = nil
			baseSearchReq.Hybrid = nil
			searchReq = baseSearchReq
			searchReq.Offset = meiliOffset
			searchReq.Limit = nextLimit
			resp, err = p.client.Search(ctx, indexUID, searchReq)
		}
		if err != nil {
			return nil, err
		}
		estimatedTotalHits = resp.EstimatedTotalHits
		if len(resp.Hits) == 0 {
			exhausted = true
			break
		}
		scanned += len(resp.Hits)
		meiliOffset += len(resp.Hits)

		ids := make([]string, 0, len(resp.Hits))
		position := make(map[string]int, len(resp.Hits))
		for i, hit := range resp.Hits {
			id := strings.TrimSpace(hit.ContentID)
			if id == "" {
				continue
			}
			if _, exists := position[id]; exists {
				continue
			}
			position[id] = i
			ids = append(ids, id)
		}
		hydrated, err := p.itemRepo.GetByIDsWithAccess(ctx, ids, req.Access)
		if err != nil {
			return nil, err
		}
		accessible = append(accessible, orderItemsByIDPosition(hydrated, position)...)

		if meiliOffset >= estimatedTotalHits || len(resp.Hits) < nextLimit {
			exhausted = true
		}
	}

	if len(accessible) < target && !exhausted {
		return nil, fmt.Errorf("meilisearch candidate scan cap reached before page was proven")
	}
	page := []*models.MediaItem{}
	if req.Offset < len(accessible) {
		end := req.Offset + req.Limit
		if end > len(accessible) {
			end = len(accessible)
		}
		page = accessible[req.Offset:end]
	}
	hasMore := len(accessible) > req.Offset+len(page)
	total := req.Offset + len(page)
	if hasMore {
		total++
	} else if estimatedTotalHits == 0 {
		total = 0
	}
	p.markFallback(semanticFallback)
	return &CatalogSearchResult{
		Items:          page,
		Total:          total,
		HasMore:        hasMore,
		TotalExact:     false,
		Provider:       SearchProviderMeilisearch,
		FallbackReason: semanticFallback,
	}, nil
}

func (p *MeilisearchSearchProvider) buildMeilisearchSearchRequest(ctx context.Context, req CatalogSearchRequest) (meilisearchSearchRequest, string) {
	if p == nil {
		return meilisearchSearchRequest{
			Query:                strings.TrimSpace(req.Query),
			Filter:               meilisearchTypeFilter(req.ItemTypes),
			AttributesToRetrieve: []string{"content_id"},
		}, ""
	}
	searchReq := meilisearchSearchRequest{
		Query:                strings.TrimSpace(req.Query),
		Filter:               meilisearchTypeFilter(req.ItemTypes),
		AttributesToRetrieve: []string{"content_id"},
		MatchingStrategy:     p.config.MatchingStrategy,
	}
	if !p.shouldUseSemanticSearch(req) {
		return searchReq, ""
	}
	if p.config.Coverage != nil {
		if ready, reason := p.config.Coverage.CoverageReady(req.ItemTypes); !ready {
			return searchReq, "semantic_not_ready: " + reason
		}
	}
	if p.config.Vectorizer == nil {
		return searchReq, "semantic search enabled but query vectorizer is unavailable"
	}
	vector, err := p.cachedQueryVector(ctx, req.Query)
	if err != nil {
		return searchReq, "semantic query embedding failed: " + err.Error()
	}
	if len(vector) == 0 {
		return searchReq, "semantic query embedding returned no vector"
	}
	searchReq.Vector = vector
	searchReq.Hybrid = &meilisearchHybridRequest{
		Embedder:      p.config.Embedder,
		SemanticRatio: p.config.SemanticRatio,
	}
	return searchReq, ""
}

func (p *MeilisearchSearchProvider) shouldUseSemanticSearch(req CatalogSearchRequest) bool {
	if p == nil || !p.config.SemanticEnabled {
		return false
	}
	return len(strings.Fields(normalizeCatalogSearchQueryForVector(req.Query))) >= 3
}

func (p *MeilisearchSearchProvider) cachedQueryVector(ctx context.Context, query string) ([]float32, error) {
	if p == nil || p.config.Vectorizer == nil {
		return nil, fmt.Errorf("semantic query vectorizer is unavailable")
	}
	normalized := normalizeCatalogSearchQueryForVector(query)
	if normalized == "" {
		return nil, nil
	}
	cacheKey := strings.ToLower(normalized)
	now := time.Now()

	p.mu.Lock()
	if cached, ok := p.vectorCache[cacheKey]; ok && now.Before(cached.expiresAt) {
		vector := cloneFloat32Slice(cached.vector)
		p.mu.Unlock()
		return vector, nil
	}
	p.mu.Unlock()

	vector, err := p.config.Vectorizer.EmbedSearchQuery(ctx, normalized)
	if err != nil {
		return nil, err
	}
	vector, err = embeddingvectors.EnsureCanonicalDimensions(vector)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vectorCache == nil {
		p.vectorCache = make(map[string]cachedCatalogSearchQueryVector)
	}
	p.vectorCacheSeq++
	p.vectorCache[cacheKey] = cachedCatalogSearchQueryVector{
		vector:    cloneFloat32Slice(vector),
		expiresAt: now.Add(meilisearchQueryVectorCacheTTL),
		seq:       p.vectorCacheSeq,
	}
	p.pruneQueryVectorCacheLocked(now)
	return cloneFloat32Slice(vector), nil
}

func (p *MeilisearchSearchProvider) pruneQueryVectorCacheLocked(now time.Time) {
	for key, cached := range p.vectorCache {
		if !now.Before(cached.expiresAt) {
			delete(p.vectorCache, key)
		}
	}
	for len(p.vectorCache) > meilisearchQueryVectorCacheMax {
		var oldestKey string
		var oldestSeq int64
		first := true
		for key, cached := range p.vectorCache {
			if first || cached.seq < oldestSeq {
				first = false
				oldestKey = key
				oldestSeq = cached.seq
			}
		}
		if oldestKey == "" {
			return
		}
		delete(p.vectorCache, oldestKey)
	}
}

func (p *MeilisearchSearchProvider) fallbackSearch(ctx context.Context, req CatalogSearchRequest, reason string) (*CatalogSearchResult, error) {
	if p == nil || p.fallback == nil {
		return nil, fmt.Errorf("meilisearch fallback unavailable: %s", reason)
	}
	p.markFallback(reason)
	result, err := p.fallback.Search(ctx, req)
	if result != nil {
		result.FallbackReason = reason
	}
	return result, err
}

func (p *MeilisearchSearchProvider) circuitBlocked(now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Before(p.unhealthyUntil) {
		return p.unhealthyReason, true
	}
	return "", false
}

func (p *MeilisearchSearchProvider) tripCircuit(cause error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unhealthyUntil = time.Now().Add(p.config.CircuitCooldown)
	p.unhealthyReason = cause.Error()
	p.lastFallback = cause.Error()
}

func (p *MeilisearchSearchProvider) markFallback(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastFallback = reason
}

func (p *MeilisearchSearchProvider) clearFallback() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastFallback = ""
}

func (p *MeilisearchSearchProvider) shouldTripCircuit(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var httpErr *meilisearchHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusUnauthorized ||
			httpErr.StatusCode == http.StatusForbidden ||
			httpErr.StatusCode >= http.StatusInternalServerError
	}
	var decodeErr *meilisearchDecodeError
	if errors.As(err, &decodeErr) {
		return true
	}
	return false
}

func (p *MeilisearchSearchProvider) Status() CatalogSearchMeiliStatus {
	if p == nil {
		return CatalogSearchMeiliStatus{
			Configured:   false,
			Healthy:      false,
			CircuitState: "not_configured",
		}
	}
	status := CatalogSearchMeiliStatus{
		Configured:       p.client != nil,
		Healthy:          true,
		CircuitState:     "closed",
		TimeoutMS:        int(p.config.Timeout / time.Millisecond),
		MatchingStrategy: p.config.MatchingStrategy,
		IndexTypes:       p.config.IndexTypes,
		SemanticEnabled:  p.config.SemanticEnabled,
		SemanticRatio:    p.config.SemanticRatio,
		Embedder:         p.config.Embedder,
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	status.LastFallback = p.lastFallback
	if time.Now().Before(p.unhealthyUntil) {
		until := p.unhealthyUntil
		status.Healthy = false
		status.CircuitState = "open"
		status.CircuitReason = p.unhealthyReason
		status.CircuitUntil = &until
	}
	return status
}

func meilisearchTypeFilter(itemTypes []string) string {
	cleaned := compactNonEmptyStrings(itemTypes)
	if len(cleaned) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cleaned))
	for _, itemType := range cleaned {
		itemType = strings.ReplaceAll(strings.ToLower(itemType), `"`, `\"`)
		parts = append(parts, fmt.Sprintf(`type = "%s"`, itemType))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func normalizeCatalogSearchQueryForVector(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func cloneFloat32Slice(values []float32) []float32 {
	if len(values) == 0 {
		return nil
	}
	out := make([]float32, len(values))
	copy(out, values)
	return out
}

func orderItemsByIDPosition(items []*models.MediaItem, position map[string]int) []*models.MediaItem {
	if len(items) == 0 {
		return items
	}
	ordered := append([]*models.MediaItem(nil), items...)
	sortByPosition := func(i, j int) bool {
		left := position[ordered[i].ContentID]
		right := position[ordered[j].ContentID]
		return left < right
	}
	sort.SliceStable(ordered, sortByPosition)
	return ordered
}
