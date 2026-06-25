package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	SearchIndexEventUpsert = "upsert"
	SearchIndexEventDelete = "delete"
	SearchIndexEventRename = "rename"

	searchIndexMaintenanceLockID int64 = 0x53494c4f5345531
)

type SearchIndexEvent struct {
	ID                int64
	Provider          string
	Action            string
	ContentID         string
	PreviousContentID string
	Attempts          int
	CreatedAt         time.Time
}

type SearchIndexState struct {
	Provider             string
	ActiveIndexUID       string
	SchemaVersion        int
	DocumentCount        int
	LastRebuildAt        *time.Time
	LastSyncAt           *time.Time
	LastProcessedEventID int64
	UpdatedAt            time.Time
}

type SearchIndexEventRepository struct {
	pool *pgxpool.Pool
}

func NewSearchIndexEventRepository(pool *pgxpool.Pool) *SearchIndexEventRepository {
	return &SearchIndexEventRepository{pool: pool}
}

func (r *SearchIndexEventRepository) EnqueueUpsert(ctx context.Context, execer itemExecer, contentID string) error {
	return r.enqueue(ctx, execer, SearchProviderMeilisearch, SearchIndexEventUpsert, contentID, "")
}

func (r *SearchIndexEventRepository) EnqueueDelete(ctx context.Context, execer itemExecer, contentID string) error {
	return r.enqueue(ctx, execer, SearchProviderMeilisearch, SearchIndexEventDelete, contentID, "")
}

func (r *SearchIndexEventRepository) EnqueueDeletes(ctx context.Context, execer itemExecer, contentIDs []string) error {
	if r == nil || execer == nil {
		return nil
	}
	contentIDs = compactNonEmptyStrings(contentIDs)
	if len(contentIDs) == 0 {
		return nil
	}
	_, err := execer.Exec(ctx, `
		INSERT INTO catalog_search_index_events (provider, action, content_id, previous_content_id)
		SELECT $1, $2, ids.content_id, ''
		FROM unnest($3::text[]) AS ids(content_id)
		WHERE EXISTS (
			SELECT 1
			FROM server_settings
			WHERE key = 'catalog.search.provider'
			  AND lower(value) = 'meilisearch'
		)
	`, SearchProviderMeilisearch, SearchIndexEventDelete, contentIDs)
	if isSearchIndexSchemaUnavailable(err) {
		return nil
	}
	return err
}

func (r *SearchIndexEventRepository) EnqueueRename(ctx context.Context, execer itemExecer, previousContentID, contentID string) error {
	return r.enqueue(ctx, execer, SearchProviderMeilisearch, SearchIndexEventRename, contentID, previousContentID)
}

func (r *SearchIndexEventRepository) enqueue(ctx context.Context, execer itemExecer, provider, action, contentID, previousContentID string) error {
	if r == nil || execer == nil {
		return nil
	}
	contentID = strings.TrimSpace(contentID)
	previousContentID = strings.TrimSpace(previousContentID)
	if contentID == "" {
		return nil
	}
	_, err := execer.Exec(ctx, `
		INSERT INTO catalog_search_index_events (provider, action, content_id, previous_content_id)
		SELECT $1, $2, $3, $4
		WHERE EXISTS (
			SELECT 1
			FROM server_settings
			WHERE key = 'catalog.search.provider'
			  AND lower(value) = 'meilisearch'
		)
	`, provider, action, contentID, previousContentID)
	if isSearchIndexSchemaUnavailable(err) {
		return nil
	}
	return err
}

func EnqueueSearchIndexUpsert(ctx context.Context, execer itemExecer, contentID string) error {
	return NewSearchIndexEventRepository(nil).EnqueueUpsert(ctx, execer, contentID)
}

func EnqueueSearchIndexDelete(ctx context.Context, execer itemExecer, contentID string) error {
	return NewSearchIndexEventRepository(nil).EnqueueDelete(ctx, execer, contentID)
}

func EnqueueSearchIndexDeletes(ctx context.Context, execer itemExecer, contentIDs []string) error {
	return NewSearchIndexEventRepository(nil).EnqueueDeletes(ctx, execer, contentIDs)
}

func EnqueueSearchIndexRename(ctx context.Context, execer itemExecer, previousContentID, contentID string) error {
	return NewSearchIndexEventRepository(nil).EnqueueRename(ctx, execer, previousContentID, contentID)
}

func (r *SearchIndexEventRepository) ListPending(ctx context.Context, provider string, limit int) ([]SearchIndexEvent, error) {
	if r == nil || r.pool == nil || limit <= 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, provider, action, content_id, previous_content_id, attempts, created_at
		FROM catalog_search_index_events
		WHERE provider = $1
		  AND processed_at IS NULL
		  AND available_at <= NOW()
		ORDER BY id ASC
		LIMIT $2
	`, provider, limit)
	if err != nil {
		if isSearchIndexSchemaUnavailable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()

	var events []SearchIndexEvent
	for rows.Next() {
		var event SearchIndexEvent
		if err := rows.Scan(
			&event.ID,
			&event.Provider,
			&event.Action,
			&event.ContentID,
			&event.PreviousContentID,
			&event.Attempts,
			&event.CreatedAt,
		); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func (r *SearchIndexEventRepository) PendingCount(ctx context.Context, provider string) (int, error) {
	if r == nil || r.pool == nil {
		return 0, nil
	}
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM catalog_search_index_events
		WHERE provider = $1
		  AND processed_at IS NULL
	`, provider).Scan(&count)
	if isSearchIndexSchemaUnavailable(err) {
		return 0, nil
	}
	return count, err
}

func (r *SearchIndexEventRepository) MarkProcessed(ctx context.Context, ids []int64) error {
	if r == nil || r.pool == nil || len(ids) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE catalog_search_index_events
		SET processed_at = NOW(),
		    last_error = ''
		WHERE id = ANY($1)
	`, ids)
	if isSearchIndexSchemaUnavailable(err) {
		return nil
	}
	return err
}

func (r *SearchIndexEventRepository) MarkFailed(ctx context.Context, ids []int64, cause error) error {
	if r == nil || r.pool == nil || len(ids) == 0 {
		return nil
	}
	message := "search index sync failed"
	if cause != nil {
		message = cause.Error()
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE catalog_search_index_events
		SET attempts = attempts + 1,
		    available_at = NOW() + LEAST(((attempts + 1) * INTERVAL '30 seconds'), INTERVAL '15 minutes'),
		    last_error = $2
		WHERE id = ANY($1)
	`, ids, message)
	if isSearchIndexSchemaUnavailable(err) {
		return nil
	}
	return err
}

func (r *SearchIndexEventRepository) GetState(ctx context.Context, provider string) (SearchIndexState, error) {
	state := SearchIndexState{Provider: provider}
	if r == nil || r.pool == nil {
		return state, nil
	}
	err := r.pool.QueryRow(ctx, `
		SELECT provider, active_index_uid, schema_version, document_count,
		       last_rebuild_at, last_sync_at, last_processed_event_id, updated_at
		FROM catalog_search_index_state
		WHERE provider = $1
	`, provider).Scan(
		&state.Provider,
		&state.ActiveIndexUID,
		&state.SchemaVersion,
		&state.DocumentCount,
		&state.LastRebuildAt,
		&state.LastSyncAt,
		&state.LastProcessedEventID,
		&state.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) || isSearchIndexSchemaUnavailable(err) {
		return state, nil
	}
	return state, err
}

func (r *SearchIndexEventRepository) UpdateStateAfterRebuild(ctx context.Context, provider, activeIndexUID string, schemaVersion, documentCount int) error {
	if r == nil || r.pool == nil {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO catalog_search_index_state (
			provider, active_index_uid, schema_version, document_count,
			last_rebuild_at, last_sync_at, updated_at
		)
		VALUES ($1, $2, $3, $4, NOW(), NOW(), NOW())
		ON CONFLICT (provider) DO UPDATE SET
			active_index_uid = EXCLUDED.active_index_uid,
			schema_version = EXCLUDED.schema_version,
			document_count = EXCLUDED.document_count,
			last_rebuild_at = EXCLUDED.last_rebuild_at,
			last_sync_at = EXCLUDED.last_sync_at,
			updated_at = NOW()
	`, provider, activeIndexUID, schemaVersion, documentCount)
	if isSearchIndexSchemaUnavailable(err) {
		return nil
	}
	return err
}

func (r *SearchIndexEventRepository) UpdateStateAfterSync(ctx context.Context, provider string, lastProcessedEventID int64, documentCount int) error {
	if r == nil || r.pool == nil {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO catalog_search_index_state (
			provider, document_count, last_sync_at, last_processed_event_id, updated_at
		)
		VALUES ($1, $2, NOW(), $3, NOW())
		ON CONFLICT (provider) DO UPDATE SET
			document_count = EXCLUDED.document_count,
			last_sync_at = EXCLUDED.last_sync_at,
			last_processed_event_id = GREATEST(catalog_search_index_state.last_processed_event_id, EXCLUDED.last_processed_event_id),
			updated_at = NOW()
	`, provider, documentCount, lastProcessedEventID)
	if isSearchIndexSchemaUnavailable(err) {
		return nil
	}
	return err
}

type SearchIndexAdvisoryLock struct {
	conn *pgxpool.Conn
	key  int64
}

func (r *SearchIndexEventRepository) TryAdvisoryLock(ctx context.Context, key int64) (*SearchIndexAdvisoryLock, bool, error) {
	if r == nil || r.pool == nil {
		return nil, false, nil
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	return &SearchIndexAdvisoryLock{conn: conn, key: key}, true, nil
}

func (l *SearchIndexAdvisoryLock) Close(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return nil
	}
	defer l.conn.Release()
	var unlocked bool
	if err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, l.key).Scan(&unlocked); err != nil {
		return err
	}
	if !unlocked {
		return fmt.Errorf("search index advisory lock %d was not held", l.key)
	}
	return nil
}

func isSearchIndexSchemaUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P01"
	}
	return false
}
