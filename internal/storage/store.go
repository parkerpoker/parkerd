package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	cfg "github.com/parkerpoker/parkerd/internal/config"
	"github.com/dgraph-io/badger/v4"
	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

const (
	namespaceRuntimeTableState          = "runtime/table/state"
	namespaceRuntimePrivateState        = "runtime/table/private"
	namespaceRuntimePublicAds           = "runtime/public/ad"
	namespaceRuntimeTableFunds          = "runtime/table/funds"
	namespaceRuntimeEvents              = "runtime/table/events"
	namespaceRuntimeSnapshots           = "runtime/table/snapshots"
	namespaceRuntimeTransportManifest   = "runtime/transport/manifest"
	namespaceRuntimeTransportPeers      = "runtime/transport/peer"
	namespaceRuntimeTransportOutbox     = "runtime/transport/outbox"
	namespaceRuntimeTransportInbox      = "runtime/transport/inbox"
	namespaceRuntimeTransportDedupe     = "runtime/transport/dedupe"
	namespaceRuntimeTransportDeadLetter = "runtime/transport/dead-letter"
	namespaceRuntimeTransportTables     = "runtime/transport/table"

	namespaceIndexerAds     = "indexer/public/ad"
	namespaceIndexerStates  = "indexer/public/state"
	namespaceIndexerUpdates = "indexer/public/update"
)

type recordStore interface {
	PutRecord(namespace, key string, value []byte) error
	GetRecord(namespace, key string) ([]byte, error)
	ListRecords(namespace string) (map[string][]byte, error)
	DeleteRecord(namespace, key string) error
	Close() error
}

type eventStore interface {
	ReplaceList(namespace, key string, values [][]byte) error
	ListValues(namespace, key string) ([][]byte, error)
	AppendValue(namespace, key string, value []byte) error
	Close() error
}

type CacheStore interface {
	Get(key string) ([]byte, bool, error)
	Set(key string, value []byte, ttl time.Duration) error
	Delete(keys ...string) error
	Close() error
}

type Stores struct {
	cache  CacheStore
	core   recordStore
	events eventStore
}

type RuntimeRepository struct {
	profile string
	stores  *Stores
}

type IndexerRepository struct {
	stores *Stores
}

type RawPublicTableView struct {
	Advertisement []byte
	LatestState   []byte
	RecentUpdates [][]byte
}

var namedLocks sync.Map

func Open(runtimeConfig cfg.RuntimeConfig) (*Stores, error) {
	core, err := openRecordStore(runtimeConfig.CoreDBType, runtimeConfig.CoreDBDSN)
	if err != nil {
		return nil, err
	}

	events, err := openEventStore(runtimeConfig.EventDBType, runtimeConfig.EventDBDSN)
	if err != nil {
		_ = core.Close()
		return nil, err
	}

	cache, err := openCacheStore(runtimeConfig)
	if err != nil {
		_ = core.Close()
		_ = events.Close()
		return nil, err
	}

	return &Stores{
		cache:  cache,
		core:   core,
		events: events,
	}, nil
}

func (stores *Stores) Cache() CacheStore {
	return stores.cache
}

func (stores *Stores) Close() error {
	var joined error
	if stores.cache != nil {
		joined = errors.Join(joined, stores.cache.Close())
	}
	if stores.events != nil {
		joined = errors.Join(joined, stores.events.Close())
	}
	if stores.core != nil {
		joined = errors.Join(joined, stores.core.Close())
	}
	return joined
}

func OpenRuntimeRepository(runtimeConfig cfg.RuntimeConfig, profile string) (*RuntimeRepository, error) {
	profileSlug := cfg.SlugProfile(profile)
	scopedConfig := runtimeConfig
	scopedConfig.CoreDBDSN = runtimeScopedDSN(runtimeConfig.CoreDBType, runtimeConfig.CoreDBDSN, profileSlug)
	scopedConfig.EventDBDSN = runtimeScopedDSN(runtimeConfig.EventDBType, runtimeConfig.EventDBDSN, profileSlug)

	stores, err := Open(scopedConfig)
	if err != nil {
		return nil, err
	}
	return &RuntimeRepository{
		profile: profileSlug,
		stores:  stores,
	}, nil
}

func (repository *RuntimeRepository) Close() error {
	return repository.stores.Close()
}

func (repository *RuntimeRepository) ListTableIDs() ([]string, error) {
	records, err := repository.stores.core.ListRecords(namespaceRuntimeTableState)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(records))
	for key := range records {
		ids = append(ids, key)
	}
	sort.Strings(ids)
	return ids, nil
}

func (repository *RuntimeRepository) LoadTableState(tableID string) ([]byte, error) {
	return repository.stores.core.GetRecord(namespaceRuntimeTableState, tableID)
}

func (repository *RuntimeRepository) SaveTableState(tableID string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTableState, tableID, raw)
}

func (repository *RuntimeRepository) LoadPrivateState(tableID string) ([]byte, error) {
	return repository.stores.core.GetRecord(namespaceRuntimePrivateState, tableID)
}

func (repository *RuntimeRepository) SavePrivateState(tableID string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimePrivateState, tableID, raw)
}

func (repository *RuntimeRepository) ReplaceEvents(tableID string, values [][]byte) error {
	return repository.stores.events.ReplaceList(namespaceRuntimeEvents, tableID, values)
}

func (repository *RuntimeRepository) ListEvents(tableID string) ([][]byte, error) {
	return repository.stores.events.ListValues(namespaceRuntimeEvents, tableID)
}

func (repository *RuntimeRepository) ReplaceSnapshots(tableID string, values [][]byte) error {
	return repository.stores.events.ReplaceList(namespaceRuntimeSnapshots, tableID, values)
}

func (repository *RuntimeRepository) ListSnapshots(tableID string) ([][]byte, error) {
	return repository.stores.events.ListValues(namespaceRuntimeSnapshots, tableID)
}

func (repository *RuntimeRepository) UpsertPublicAd(tableID string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimePublicAds, tableID, raw)
}

func (repository *RuntimeRepository) LoadPublicAds() (map[string][]byte, error) {
	return repository.stores.core.ListRecords(namespaceRuntimePublicAds)
}

func (repository *RuntimeRepository) LoadTableFunds() ([]byte, error) {
	return repository.stores.core.GetRecord(namespaceRuntimeTableFunds, repository.profile)
}

func (repository *RuntimeRepository) SaveTableFunds(raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTableFunds, repository.profile, raw)
}

func (repository *RuntimeRepository) WithTableLock(tableID string, fn func() error) error {
	mutexValue, _ := namedLocks.LoadOrStore(tableID, &sync.Mutex{})
	mutex := mutexValue.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	return fn()
}

func (repository *RuntimeRepository) LoadTransportManifest() ([]byte, error) {
	return repository.stores.core.GetRecord(namespaceRuntimeTransportManifest, repository.profile)
}

func (repository *RuntimeRepository) SaveTransportManifest(raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTransportManifest, repository.profile, raw)
}

func (repository *RuntimeRepository) ListTransportPeers() (map[string][]byte, error) {
	return repository.stores.core.ListRecords(namespaceRuntimeTransportPeers)
}

func (repository *RuntimeRepository) SaveTransportPeer(peerID string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTransportPeers, peerID, raw)
}

func (repository *RuntimeRepository) DeleteTransportPeer(peerID string) error {
	return repository.stores.core.DeleteRecord(namespaceRuntimeTransportPeers, peerID)
}

func (repository *RuntimeRepository) ListTransportOutbox() (map[string][]byte, error) {
	return repository.stores.core.ListRecords(namespaceRuntimeTransportOutbox)
}

func (repository *RuntimeRepository) SaveTransportOutboxEntry(messageID string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTransportOutbox, messageID, raw)
}

func (repository *RuntimeRepository) DeleteTransportOutboxEntry(messageID string) error {
	return repository.stores.core.DeleteRecord(namespaceRuntimeTransportOutbox, messageID)
}

func (repository *RuntimeRepository) ListTransportInbox() (map[string][]byte, error) {
	return repository.stores.core.ListRecords(namespaceRuntimeTransportInbox)
}

func (repository *RuntimeRepository) SaveTransportInboxEntry(messageID string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTransportInbox, messageID, raw)
}

func (repository *RuntimeRepository) DeleteTransportInboxEntry(messageID string) error {
	return repository.stores.core.DeleteRecord(namespaceRuntimeTransportInbox, messageID)
}

func (repository *RuntimeRepository) ListTransportDedupe() (map[string][]byte, error) {
	return repository.stores.core.ListRecords(namespaceRuntimeTransportDedupe)
}

func (repository *RuntimeRepository) SaveTransportDedupeEntry(key string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTransportDedupe, key, raw)
}

func (repository *RuntimeRepository) DeleteTransportDedupeEntry(key string) error {
	return repository.stores.core.DeleteRecord(namespaceRuntimeTransportDedupe, key)
}

func (repository *RuntimeRepository) ListTransportDeadLetters() (map[string][]byte, error) {
	return repository.stores.core.ListRecords(namespaceRuntimeTransportDeadLetter)
}

func (repository *RuntimeRepository) SaveTransportDeadLetter(messageID string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTransportDeadLetter, messageID, raw)
}

func (repository *RuntimeRepository) DeleteTransportDeadLetter(messageID string) error {
	return repository.stores.core.DeleteRecord(namespaceRuntimeTransportDeadLetter, messageID)
}

func (repository *RuntimeRepository) LoadTransportTable(tableID string) ([]byte, error) {
	return repository.stores.core.GetRecord(namespaceRuntimeTransportTables, tableID)
}

func (repository *RuntimeRepository) SaveTransportTable(tableID string, raw []byte) error {
	return repository.stores.core.PutRecord(namespaceRuntimeTransportTables, tableID, raw)
}

func (repository *RuntimeRepository) ListTransportTables() (map[string][]byte, error) {
	return repository.stores.core.ListRecords(namespaceRuntimeTransportTables)
}

func OpenIndexerRepository(runtimeConfig cfg.RuntimeConfig) (*IndexerRepository, error) {
	stores, err := Open(runtimeConfig)
	if err != nil {
		return nil, err
	}
	return &IndexerRepository{stores: stores}, nil
}

func (repository *IndexerRepository) Close() error {
	return repository.stores.Close()
}

func (repository *IndexerRepository) SavePublicTableAd(tableID string, raw []byte) error {
	if err := repository.stores.core.PutRecord(namespaceIndexerAds, tableID, raw); err != nil {
		return err
	}
	return repository.stores.cache.Delete(indexerTableCacheKey(tableID), indexerTablesCacheKey())
}

func (repository *IndexerRepository) SavePublicUpdate(tableID string, raw []byte, latestState []byte) error {
	if err := repository.stores.events.AppendValue(namespaceIndexerUpdates, tableID, raw); err != nil {
		return err
	}
	if len(latestState) > 0 {
		if err := repository.stores.core.PutRecord(namespaceIndexerStates, tableID, latestState); err != nil {
			return err
		}
	}
	return repository.stores.cache.Delete(indexerTableCacheKey(tableID), indexerTablesCacheKey())
}

func (repository *IndexerRepository) LoadPublicTable(tableID string) (*RawPublicTableView, error) {
	ad, err := repository.stores.core.GetRecord(namespaceIndexerAds, tableID)
	if err != nil {
		return nil, err
	}
	if len(ad) == 0 {
		return nil, nil
	}
	state, err := repository.stores.core.GetRecord(namespaceIndexerStates, tableID)
	if err != nil {
		return nil, err
	}
	updates, err := repository.stores.events.ListValues(namespaceIndexerUpdates, tableID)
	if err != nil {
		return nil, err
	}
	reverseBytes(updates)
	if len(updates) > 32 {
		updates = updates[:32]
	}
	return &RawPublicTableView{
		Advertisement: ad,
		LatestState:   state,
		RecentUpdates: updates,
	}, nil
}

func (repository *IndexerRepository) ListPublicTables() ([]RawPublicTableView, error) {
	ads, err := repository.stores.core.ListRecords(namespaceIndexerAds)
	if err != nil {
		return nil, err
	}
	tableIDs := make([]string, 0, len(ads))
	for tableID := range ads {
		tableIDs = append(tableIDs, tableID)
	}
	sort.Strings(tableIDs)

	views := make([]RawPublicTableView, 0, len(tableIDs))
	for _, tableID := range tableIDs {
		view, err := repository.LoadPublicTable(tableID)
		if err != nil {
			return nil, err
		}
		if view == nil {
			continue
		}
		views = append(views, *view)
	}
	return views, nil
}

func openRecordStore(kind string, dsn string) (recordStore, error) {
	switch kind {
	case "sqlite":
		return openSQLStore("sqlite", dsn)
	case "postgres":
		return openSQLStore("pgx", dsn)
	case "badger":
		return openBadgerStore(dsn)
	default:
		return nil, fmt.Errorf("unsupported core db type %q", kind)
	}
}

func openEventStore(kind string, dsn string) (eventStore, error) {
	switch kind {
	case "postgres":
		return openSQLStore("pgx", dsn)
	case "badger":
		return openBadgerStore(dsn)
	default:
		return nil, fmt.Errorf("unsupported event db type %q", kind)
	}
}

func openCacheStore(runtimeConfig cfg.RuntimeConfig) (CacheStore, error) {
	switch runtimeConfig.CacheType {
	case "", "memory":
		return newMemoryCache(), nil
	case "redis":
		return newRedisCache(runtimeConfig), nil
	default:
		return nil, fmt.Errorf("unsupported cache type %q", runtimeConfig.CacheType)
	}
}

type sqlStore struct {
	db         *sql.DB
	driverName string
}

func openSQLStore(driverName, dsn string) (*sqlStore, error) {
	if driverName == "sqlite" {
		if err := os.MkdirAll(filepath.Dir(dsn), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	if driverName == "sqlite" {
		// The runtime drives table replication and protocol progress from multiple goroutines.
		// Serializing the SQLite handle and enabling a small busy timeout keeps those internal
		// races from surfacing as spurious SQLITE_BUSY errors during local reads and writes.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
			_ = db.Close()
			return nil, err
		}
		if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	store := &sqlStore{
		db:         db,
		driverName: driverName,
	}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (store *sqlStore) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS parker_records (
			namespace TEXT NOT NULL,
			record_key TEXT NOT NULL,
			record_json TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (namespace, record_key)
		)`,
		`CREATE TABLE IF NOT EXISTS parker_lists (
			namespace TEXT NOT NULL,
			owner_key TEXT NOT NULL,
			item_index INTEGER NOT NULL,
			record_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (namespace, owner_key, item_index)
		)`,
	}
	for _, query := range queries {
		if _, err := store.db.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

func (store *sqlStore) PutRecord(namespace, key string, value []byte) error {
	query := store.formatArgs(`
		INSERT INTO parker_records (namespace, record_key, record_json, updated_at)
		VALUES (%s, %s, %s, %s)
		ON CONFLICT(namespace, record_key) DO UPDATE SET
			record_json = EXCLUDED.record_json,
			updated_at = EXCLUDED.updated_at
	`)
	_, err := store.db.Exec(query, namespace, key, string(value), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (store *sqlStore) GetRecord(namespace, key string) ([]byte, error) {
	query := store.formatArgs(`
		SELECT record_json
		FROM parker_records
		WHERE namespace = %s AND record_key = %s
	`)
	var raw string
	err := store.db.QueryRow(query, namespace, key).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return []byte(raw), nil
}

func (store *sqlStore) ListRecords(namespace string) (map[string][]byte, error) {
	query := store.formatArgs(`
		SELECT record_key, record_json
		FROM parker_records
		WHERE namespace = %s
		ORDER BY record_key
	`)
	rows, err := store.db.Query(query, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := map[string][]byte{}
	for rows.Next() {
		var key string
		var raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		records[key] = []byte(raw)
	}
	return records, rows.Err()
}

func (store *sqlStore) DeleteRecord(namespace, key string) error {
	query := store.formatArgs(`
		DELETE FROM parker_records
		WHERE namespace = %s AND record_key = %s
	`)
	_, err := store.db.Exec(query, namespace, key)
	return err
}

func (store *sqlStore) ReplaceList(namespace, key string, values [][]byte) error {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	deleteQuery := store.formatArgs(`
		DELETE FROM parker_lists
		WHERE namespace = %s AND owner_key = %s
	`)
	if _, err := tx.Exec(deleteQuery, namespace, key); err != nil {
		_ = tx.Rollback()
		return err
	}

	insertQuery := store.formatArgs(`
		INSERT INTO parker_lists (namespace, owner_key, item_index, record_json, created_at)
		VALUES (%s, %s, %s, %s, %s)
	`)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for index, value := range values {
		if _, err := tx.Exec(insertQuery, namespace, key, index, string(value), now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (store *sqlStore) ListValues(namespace, key string) ([][]byte, error) {
	query := store.formatArgs(`
		SELECT record_json
		FROM parker_lists
		WHERE namespace = %s AND owner_key = %s
		ORDER BY item_index
	`)
	rows, err := store.db.Query(query, namespace, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := make([][]byte, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		values = append(values, []byte(raw))
	}
	return values, rows.Err()
}

func (store *sqlStore) AppendValue(namespace, key string, value []byte) error {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	selectQuery := store.formatArgs(`
		SELECT COALESCE(MAX(item_index), -1)
		FROM parker_lists
		WHERE namespace = %s AND owner_key = %s
	`)
	var nextIndex int
	if err := tx.QueryRow(selectQuery, namespace, key).Scan(&nextIndex); err != nil {
		_ = tx.Rollback()
		return err
	}
	nextIndex += 1

	insertQuery := store.formatArgs(`
		INSERT INTO parker_lists (namespace, owner_key, item_index, record_json, created_at)
		VALUES (%s, %s, %s, %s, %s)
	`)
	if _, err := tx.Exec(insertQuery, namespace, key, nextIndex, string(value), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (store *sqlStore) Close() error {
	return store.db.Close()
}

func (store *sqlStore) formatArgs(template string) string {
	placeholder := func(index int) string {
		if store.driverName == "pgx" {
			return fmt.Sprintf("$%d", index)
		}
		return "?"
	}
	formatted := template
	for index := 1; strings.Contains(formatted, "%s"); index += 1 {
		formatted = strings.Replace(formatted, "%s", placeholder(index), 1)
	}
	return formatted
}

type badgerStore struct {
	db *badger.DB
}

func openBadgerStore(path string) (*badgerStore, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	db, err := badger.Open(badger.DefaultOptions(path).WithLogger(nil))
	if err != nil {
		return nil, err
	}
	return &badgerStore{db: db}, nil
}

func (store *badgerStore) PutRecord(namespace, key string, value []byte) error {
	return store.db.Update(func(txn *badger.Txn) error {
		return txn.Set(badgerRecordKey(namespace, key), value)
	})
}

func (store *badgerStore) GetRecord(namespace, key string) ([]byte, error) {
	var raw []byte
	err := store.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(badgerRecordKey(namespace, key))
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return nil
			}
			return err
		}
		return item.Value(func(value []byte) error {
			raw = append([]byte(nil), value...)
			return nil
		})
	})
	return raw, err
}

func (store *badgerStore) ListRecords(namespace string) (map[string][]byte, error) {
	records := map[string][]byte{}
	prefix := badgerRecordPrefix(namespace)
	err := store.db.View(func(txn *badger.Txn) error {
		options := badger.DefaultIteratorOptions
		options.PrefetchValues = true
		iterator := txn.NewIterator(options)
		defer iterator.Close()
		for iterator.Seek(prefix); iterator.ValidForPrefix(prefix); iterator.Next() {
			item := iterator.Item()
			key := string(item.Key()[len(prefix):])
			if err := item.Value(func(value []byte) error {
				records[key] = append([]byte(nil), value...)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return records, err
}

func (store *badgerStore) DeleteRecord(namespace, key string) error {
	return store.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(badgerRecordKey(namespace, key))
	})
}

func (store *badgerStore) ReplaceList(namespace, key string, values [][]byte) error {
	prefix := badgerListPrefix(namespace, key)
	return store.db.Update(func(txn *badger.Txn) error {
		options := badger.DefaultIteratorOptions
		options.PrefetchValues = false
		iterator := txn.NewIterator(options)
		defer iterator.Close()
		keys := make([][]byte, 0)
		for iterator.Seek(prefix); iterator.ValidForPrefix(prefix); iterator.Next() {
			keys = append(keys, append([]byte(nil), iterator.Item().Key()...))
		}
		for _, itemKey := range keys {
			if err := txn.Delete(itemKey); err != nil {
				return err
			}
		}
		for index, value := range values {
			if err := txn.Set(badgerListKey(namespace, key, index), value); err != nil {
				return err
			}
		}
		return nil
	})
}

func (store *badgerStore) ListValues(namespace, key string) ([][]byte, error) {
	values := make([][]byte, 0)
	prefix := badgerListPrefix(namespace, key)
	err := store.db.View(func(txn *badger.Txn) error {
		options := badger.DefaultIteratorOptions
		options.PrefetchValues = true
		iterator := txn.NewIterator(options)
		defer iterator.Close()
		for iterator.Seek(prefix); iterator.ValidForPrefix(prefix); iterator.Next() {
			item := iterator.Item()
			if err := item.Value(func(value []byte) error {
				values = append(values, append([]byte(nil), value...))
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return values, err
}

func (store *badgerStore) AppendValue(namespace, key string, value []byte) error {
	return store.db.Update(func(txn *badger.Txn) error {
		prefix := badgerListPrefix(namespace, key)
		index := 0
		options := badger.DefaultIteratorOptions
		options.PrefetchValues = false
		iterator := txn.NewIterator(options)
		defer iterator.Close()
		for iterator.Seek(prefix); iterator.ValidForPrefix(prefix); iterator.Next() {
			index += 1
		}
		return txn.Set(badgerListKey(namespace, key, index), value)
	})
}

func (store *badgerStore) Close() error {
	return store.db.Close()
}

func badgerRecordPrefix(namespace string) []byte {
	return []byte("r|" + namespace + "|")
}

func badgerRecordKey(namespace, key string) []byte {
	return append(badgerRecordPrefix(namespace), []byte(key)...)
}

func badgerListPrefix(namespace, key string) []byte {
	return []byte("l|" + namespace + "|" + key + "|")
}

func badgerListKey(namespace, key string, index int) []byte {
	return []byte(fmt.Sprintf("l|%s|%s|%020d", namespace, key, index))
}

type memoryCache struct {
	items sync.Map
}

type memoryCacheItem struct {
	expiresAt time.Time
	value     []byte
}

func newMemoryCache() *memoryCache {
	return &memoryCache{}
}

func (cache *memoryCache) Get(key string) ([]byte, bool, error) {
	value, ok := cache.items.Load(key)
	if !ok {
		return nil, false, nil
	}
	item := value.(memoryCacheItem)
	if !item.expiresAt.IsZero() && time.Now().After(item.expiresAt) {
		cache.items.Delete(key)
		return nil, false, nil
	}
	return append([]byte(nil), item.value...), true, nil
}

func (cache *memoryCache) Set(key string, value []byte, ttl time.Duration) error {
	item := memoryCacheItem{value: append([]byte(nil), value...)}
	if ttl > 0 {
		item.expiresAt = time.Now().Add(ttl)
	}
	cache.items.Store(key, item)
	return nil
}

func (cache *memoryCache) Delete(keys ...string) error {
	for _, key := range keys {
		cache.items.Delete(key)
	}
	return nil
}

func (cache *memoryCache) Close() error {
	return nil
}

type redisCache struct {
	client *redis.Client
}

func newRedisCache(runtimeConfig cfg.RuntimeConfig) *redisCache {
	return &redisCache{
		client: redis.NewClient(&redis.Options{
			Addr:     runtimeConfig.CacheRedisAddr,
			DB:       runtimeConfig.CacheRedisDB,
			Password: runtimeConfig.CacheRedisPass,
		}),
	}
}

func (cache *redisCache) Get(key string) ([]byte, bool, error) {
	value, err := cache.client.Get(context.Background(), key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return value, true, nil
}

func (cache *redisCache) Set(key string, value []byte, ttl time.Duration) error {
	return cache.client.Set(context.Background(), key, value, ttl).Err()
}

func (cache *redisCache) Delete(keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return cache.client.Del(context.Background(), keys...).Err()
}

func (cache *redisCache) Close() error {
	return cache.client.Close()
}

func indexerTablesCacheKey() string {
	return "indexer/public/tables"
}

func indexerTableCacheKey(tableID string) string {
	return "indexer/public/table/" + tableID
}

func reverseBytes(values [][]byte) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func runtimeScopedDSN(kind, dsn, profile string) string {
	if dsn == "" || profile == "" {
		return dsn
	}

	switch kind {
	case "sqlite":
		return filepath.Join(filepath.Dir(dsn), "runtime", profile, filepath.Base(dsn))
	case "badger":
		return filepath.Join(dsn, "runtime", profile)
	default:
		return dsn
	}
}
