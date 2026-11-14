package cache

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"

	_ "modernc.org/sqlite"
)

// MetadataStore provides a local, thread-safe SQLite-backed metadata cache
// for Movistar Cloud folders and media items to enable zero-SAPI read operations.
type MetadataStore struct {
	dbPath              string
	db                  *sql.DB
	writeMu             sync.Mutex
	syncMu              sync.Mutex
	cleanerMu           sync.RWMutex
	tombstoneCleaner    func(path string)
	hasTombstonesFilter func() bool
	rootFolderID        atomic.Int64
}

// NewMetadataStore opens or creates a SQLite database at dbPath and configures
// high-performance PRAGMAs and schema tables.
func NewMetadataStore(dbPath string) (*MetadataStore, error) {
	if dbPath == "" {
		return nil, errors.New("dbPath cannot be empty")
	}

	if dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file:") {
		dir := filepath.Dir(dbPath)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, fmt.Errorf("create database directory %s: %w", dir, err)
			}
		}
	}

	// Los PRAGMA de abajo solo afectan a la conexión que los ejecuta, y database/sql
	// abre varias: busy_timeout y WAL van también en la cadena de conexión para que
	// todas esperen en vez de fallar con SQLITE_BUSY cuando coinciden dos escrituras.
	dsn := "file:" + (&url.URL{Path: dbPath}).EscapedPath() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db %s: %w", dbPath, err)
	}

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA temp_store=MEMORY;",
		"PRAGMA foreign_keys=OFF;",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply pragma %q: %w", pragma, err)
		}
	}

	store := &MetadataStore{
		dbPath: dbPath,
		db:     db,
	}

	if err := store.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return store, nil
}

// Close closes the underlying SQLite database connection.
func (s *MetadataStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Flushes WAL entries to the database file and truncates the -wal file.
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	return s.db.Close()
}

// Path returns the database file path used by this store.
func (s *MetadataStore) Path() string {
	if s == nil {
		return ""
	}
	return s.dbPath
}

// DB returns the underlying sql.DB instance (primarily for inspection/testing).
func (s *MetadataStore) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// SetTombstoneCleaner sets an optional callback invoked when items are updated or upserted.
func (s *MetadataStore) SetTombstoneCleaner(cleaner func(path string)) {
	if s == nil {
		return
	}
	s.cleanerMu.Lock()
	defer s.cleanerMu.Unlock()
	s.tombstoneCleaner = cleaner
}

// SetTombstoneFilter sets an optional callback reporting whether any tombstones are actively held.
func (s *MetadataStore) SetTombstoneFilter(filter func() bool) {
	if s == nil {
		return
	}
	s.cleanerMu.Lock()
	defer s.cleanerMu.Unlock()
	s.hasTombstonesFilter = filter
}

func (s *MetadataStore) shouldClearTombstone() bool {
	if s == nil {
		return false
	}
	s.cleanerMu.RLock()
	cleaner := s.tombstoneCleaner
	filter := s.hasTombstonesFilter
	s.cleanerMu.RUnlock()
	if cleaner == nil {
		return false
	}
	if filter != nil {
		return filter()
	}
	return true
}

func (s *MetadataStore) clearTombstone(itemPath string) {
	if s == nil {
		return
	}
	s.cleanerMu.RLock()
	cleaner := s.tombstoneCleaner
	s.cleanerMu.RUnlock()
	if cleaner != nil && itemPath != "" {
		cleaner(itemPath)
	}
}

func (s *MetadataStore) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS sync_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS folders (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL COLLATE NOCASE,
		parent_id INTEGER NOT NULL,
		magic INTEGER NOT NULL DEFAULT 0,
		offline INTEGER NOT NULL DEFAULT 0,
		creation_date TEXT NOT NULL DEFAULT '',
		modification_date TEXT NOT NULL DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_folders_parent_name ON folders (parent_id, name COLLATE NOCASE);
	CREATE INDEX IF NOT EXISTS idx_folders_magic ON folders (magic);

	CREATE TABLE IF NOT EXISTS media (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL COLLATE NOCASE,
		size INTEGER NOT NULL DEFAULT 0,
		url TEXT NOT NULL DEFAULT '',
		etag TEXT NOT NULL DEFAULT '',
		folder_id INTEGER NOT NULL,
		creation_date TEXT NOT NULL DEFAULT '',
		modification_date TEXT NOT NULL DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_media_folder_name ON media (folder_id, name COLLATE NOCASE);

	CREATE TABLE IF NOT EXISTS virtual_empty_files (
		folder_id INTEGER NOT NULL,
		name TEXT NOT NULL COLLATE NOCASE,
		mod_time TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (folder_id, name COLLATE NOCASE)
	);

	CREATE INDEX IF NOT EXISTS idx_virtual_empty_folder ON virtual_empty_files (folder_id);
	`
	_, err := s.db.Exec(schema)
	return err
}

// --- Sync State APIs ---

// GetSyncState retrieves a metadata string value for the given key.
// Returns "", nil if key does not exist.
func (s *MetadataStore) GetSyncState(key string) (string, error) {
	if s == nil || s.db == nil {
		return "", nil
	}
	var value string
	err := s.db.QueryRow("SELECT value FROM sync_state WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query sync_state key %q: %w", key, err)
	}
	return value, nil
}

// SetSyncState sets or updates a key-value pair in sync_state.
func (s *MetadataStore) SetSyncState(key, value string) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	query := `
		INSERT INTO sync_state (key, value, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = CURRENT_TIMESTAMP
	`
	if _, err := s.db.Exec(query, key, value); err != nil {
		return fmt.Errorf("set sync_state key %q: %w", key, err)
	}
	if key == "root_folder_id" {
		if id, err := strconv.ParseInt(value, 10, 64); err == nil && id > 0 {
			s.rootFolderID.Store(id)
		}
	}
	return nil
}

// HasInitialSync returns true if the initial metadata synchronization has completed (last_sync_cursor is set).
func (s *MetadataStore) HasInitialSync() bool {
	if s == nil || s.db == nil {
		return false
	}
	cursor, err := s.GetSyncState("last_sync_cursor")
	return err == nil && cursor != ""
}

// SetInitialSync marks the metadata store as having completed an initial sync.
func (s *MetadataStore) SetInitialSync(cursor ...string) error {
	if s == nil || s.db == nil {
		return nil
	}
	c := time.Now().UTC().Format("20060102T150405Z")
	if len(cursor) > 0 && cursor[0] != "" {
		c = cursor[0]
	}
	return s.SetSyncState("last_sync_cursor", c)
}

// --- Read APIs: Folders ---

func scanFolder(scanFn func(dest ...interface{}) error) (*sapi.FolderItem, error) {
	var item sapi.FolderItem
	var magicInt, offlineInt int
	var cDate, mDate string

	if err := scanFn(&item.ID, &item.Name, &item.ParentID, &magicInt, &offlineInt, &cDate, &mDate); err != nil {
		return nil, err
	}
	item.Magic = (magicInt != 0)
	item.Offline = (offlineInt != 0)
	item.CreationDate = sapi.FlexDate(cDate)
	item.ModificationDate = sapi.FlexDate(mDate)
	return &item, nil
}

// GetRootFolder retrieves the root folder item.
// Looks up "root_folder_id" from sync_state or falls back to folder with magic=1 or parent_id=0.
// Returns nil, nil if no root folder exists in the database.
func (s *MetadataStore) GetRootFolder() (*sapi.FolderItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if rootIDStr, err := s.GetSyncState("root_folder_id"); err == nil && rootIDStr != "" {
		if rootID, err := strconv.ParseInt(rootIDStr, 10, 64); err == nil && rootID > 0 {
			if f, err := s.GetFolderByID(rootID); err == nil && f != nil {
				return f, nil
			}
		}
	}

	query := `
		SELECT id, name, parent_id, magic, offline, creation_date, modification_date
		FROM folders
		WHERE magic = 1 OR parent_id = 0
		ORDER BY magic DESC, id ASC
		LIMIT 1
	`
	row := s.db.QueryRow(query)
	f, err := scanFolder(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query root folder: %w", err)
	}

	if f != nil {
		s.rootFolderID.Store(f.ID)
	}
	_ = s.SetSyncState("root_folder_id", strconv.FormatInt(f.ID, 10))
	return f, nil
}

// GetFolderByID retrieves a folder by its unique ID.
// Returns nil, nil if the folder is not found.
func (s *MetadataStore) GetFolderByID(id int64) (*sapi.FolderItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := `
		SELECT id, name, parent_id, magic, offline, creation_date, modification_date
		FROM folders
		WHERE id = ?
	`
	row := s.db.QueryRow(query, id)
	f, err := scanFolder(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query folder by id %d: %w", id, err)
	}
	return f, nil
}

// GetFolderPath returns the absolute path (e.g. "/Documents/Work") for a given folder ID.
// Root folder returns "/".
func (s *MetadataStore) GetFolderPath(folderID int64) (string, error) {
	if s == nil || s.db == nil || folderID <= 0 {
		return "/", nil
	}
	rootID := s.rootFolderID.Load()
	if rootID <= 0 {
		if rootIDStr, _ := s.GetSyncState("root_folder_id"); rootIDStr != "" {
			if id, err := strconv.ParseInt(rootIDStr, 10, 64); err == nil && id > 0 {
				rootID = id
				s.rootFolderID.Store(rootID)
			}
		}
	}

	currID := folderID
	var segments []string
	visited := make(map[int64]bool)

	for currID > 0 {
		if visited[currID] {
			break
		}
		visited[currID] = true

		if rootID > 0 && currID == rootID {
			break
		}

		f, err := s.GetFolderByID(currID)
		if err != nil || f == nil {
			break
		}
		if f.Magic || f.ParentID == 0 || f.Name == "/" || f.Name == "" {
			break
		}
		segments = append([]string{f.Name}, segments...)
		currID = f.ParentID
	}

	if len(segments) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(segments, "/"), nil
}

// GetFolderByParentAndName retrieves a folder by parent ID and name (case-insensitive).
// Includes Synology HyperBackup .hbk tolerance: if name has .hbk suffix, searches without it,
// and if name has no .hbk suffix, searches with .hbk appended.
// Returns nil, nil if not found.
func (s *MetadataStore) GetFolderByParentAndName(parentID int64, name string) (*sapi.FolderItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}

	query := `
		SELECT id, name, parent_id, magic, offline, creation_date, modification_date
		FROM folders
		WHERE parent_id = ? AND name = ? COLLATE NOCASE
		LIMIT 1
	`
	row := s.db.QueryRow(query, parentID, name)
	f, err := scanFolder(row.Scan)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("query folder %q in parent %d: %w", name, parentID, err)
	}

	// .hbk tolerance check
	var altName string
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".hbk") && len(lower) > 4 {
		altName = name[:len(name)-4]
	} else {
		altName = name + ".hbk"
	}

	row = s.db.QueryRow(query, parentID, altName)
	f, err = scanFolder(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query folder alt %q in parent %d: %w", altName, parentID, err)
	}
	return f, nil
}

// ListSubfolders returns all direct child folders of parentID, ordered alphabetically by name.
func (s *MetadataStore) ListSubfolders(parentID int64) ([]sapi.FolderItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := `
		SELECT id, name, parent_id, magic, offline, creation_date, modification_date
		FROM folders
		WHERE parent_id = ?
		ORDER BY name COLLATE NOCASE ASC
	`
	rows, err := s.db.Query(query, parentID)
	if err != nil {
		return nil, fmt.Errorf("list subfolders for parent %d: %w", parentID, err)
	}
	defer rows.Close()

	items := make([]sapi.FolderItem, 0)
	for rows.Next() {
		f, err := scanFolder(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan subfolder row for parent %d: %w", parentID, err)
		}
		items = append(items, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("subfolder rows error for parent %d: %w", parentID, err)
	}
	return items, nil
}

// --- Read APIs: Media ---

func scanMedia(scanFn func(dest ...interface{}) error) (*sapi.MediaItem, error) {
	var item sapi.MediaItem
	var cDate, mDate string

	if err := scanFn(&item.ID, &item.Name, &item.Size, &item.URL, &item.ETag, &item.FolderID, &cDate, &mDate); err != nil {
		return nil, err
	}
	item.CreationDate = sapi.FlexDate(cDate)
	item.ModificationDate = sapi.FlexDate(mDate)
	return &item, nil
}

// GetMediaByID retrieves a media item by its string ID.
// Returns nil, nil if the item is not found.
func (s *MetadataStore) GetMediaByID(id string) (*sapi.MediaItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := `
		SELECT id, name, size, url, etag, folder_id, creation_date, modification_date
		FROM media
		WHERE id = ?
	`
	row := s.db.QueryRow(query, id)
	m, err := scanMedia(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query media by id %s: %w", id, err)
	}
	return m, nil
}

// GetMediaByFolderAndName retrieves a media item by its parent folder ID and name (case-insensitive).
// If duplicates exist, it deterministically picks the newest (modification_date DESC, id DESC) and logs a warning.
// Returns nil, nil if not found.
func (s *MetadataStore) GetMediaByFolderAndName(folderID int64, name string) (*sapi.MediaItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}

	query := `
		SELECT id, name, size, url, etag, folder_id, creation_date, modification_date
		FROM media
		WHERE folder_id = ? AND name = ? COLLATE NOCASE
		ORDER BY modification_date DESC, id DESC
	`
	rows, err := s.db.Query(query, folderID, name)
	if err != nil {
		return nil, fmt.Errorf("query media %q in folder %d: %w", name, folderID, err)
	}
	defer rows.Close()

	var items []sapi.MediaItem
	for rows.Next() {
		m, err := scanMedia(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan media: %w", err)
		}
		items = append(items, *m)
	}
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) > 1 {
		var ids []string
		for _, itm := range items {
			ids = append(ids, itm.ID)
		}
		logger.Log("[Metadata] ⚠️ Nombres duplicados detectados para '%s' en carpeta %d: IDs %v. Seleccionando el más reciente (%s, ID: %s)",
			name, folderID, ids, items[0].ModificationDate, items[0].ID)
	}
	res := items[0]
	return &res, nil
}

// ListMediaInFolder returns all media items in folderID, ordered alphabetically by name.
// When duplicates exist, it deterministically keeps the newest and logs a warning with the IDs.
func (s *MetadataStore) ListMediaInFolder(folderID int64) ([]sapi.MediaItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := `
		SELECT id, name, size, url, etag, folder_id, creation_date, modification_date
		FROM media
		WHERE folder_id = ?
		ORDER BY name COLLATE NOCASE ASC, modification_date DESC, id DESC
	`
	rows, err := s.db.Query(query, folderID)
	if err != nil {
		return nil, fmt.Errorf("list media in folder %d: %w", folderID, err)
	}
	defer rows.Close()

	items := make([]sapi.MediaItem, 0)
	var lastLower string
	var currentGroup []sapi.MediaItem

	flushGroup := func() {
		if len(currentGroup) == 0 {
			return
		}
		items = append(items, currentGroup[0])
		if len(currentGroup) > 1 {
			var ids []string
			for _, itm := range currentGroup {
				ids = append(ids, itm.ID)
			}
			logger.Log("[Metadata] ⚠️ Nombres duplicados detectados para '%s' en carpeta %d: IDs %v. Seleccionando el más reciente (%s, ID: %s)",
				currentGroup[0].Name, folderID, ids, currentGroup[0].ModificationDate, currentGroup[0].ID)
		}
		currentGroup = currentGroup[:0]
	}

	for rows.Next() {
		m, err := scanMedia(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan media row in folder %d: %w", folderID, err)
		}
		low := strings.ToLower(m.Name)
		if low != lastLower {
			flushGroup()
			lastLower = low
		}
		currentGroup = append(currentGroup, *m)
	}
	flushGroup()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("media rows error in folder %d: %w", folderID, err)
	}
	return items, nil
}

// GetDuplicateMediaInFolder returns all media rows with the same name in folderID, ordered newest first.
func (s *MetadataStore) GetDuplicateMediaInFolder(folderID int64, name string) ([]sapi.MediaItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := `
		SELECT id, name, size, url, etag, folder_id, creation_date, modification_date
		FROM media
		WHERE folder_id = ? AND name = ? COLLATE NOCASE
		ORDER BY modification_date DESC, id DESC
	`
	rows, err := s.db.Query(query, folderID, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []sapi.MediaItem
	for rows.Next() {
		m, err := scanMedia(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, *m)
	}
	return items, rows.Err()
}

// --- Write-Through / Mutation APIs ---

// UpsertFolder inserts or updates a single folder.
func (s *MetadataStore) UpsertFolder(item sapi.FolderItem) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.UpsertFoldersBatch([]sapi.FolderItem{item}); err != nil {
		return err
	}
	if s.shouldClearTombstone() {
		if folderPath, err := s.GetFolderPath(item.ParentID); err == nil {
			s.clearTombstone(path.Clean(path.Join(folderPath, item.Name)))
		}
	}
	return nil
}

// UpsertFoldersBatch inserts or updates a slice of folders within a single transaction.
func (s *MetadataStore) UpsertFoldersBatch(items []sapi.FolderItem) error {
	if s == nil || s.db == nil || len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin upsert folders tx: %w", err)
	}
	defer tx.Rollback()

	query := `
		INSERT INTO folders (id, name, parent_id, magic, offline, creation_date, modification_date)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			parent_id = excluded.parent_id,
			magic = excluded.magic,
			offline = excluded.offline,
			creation_date = CASE WHEN excluded.creation_date != '' THEN excluded.creation_date ELSE folders.creation_date END,
			modification_date = CASE WHEN excluded.modification_date != '' THEN excluded.modification_date ELSE folders.modification_date END
	`
	stmt, err := tx.Prepare(query)
	if err != nil {
		return fmt.Errorf("prepare upsert folder stmt: %w", err)
	}
	defer stmt.Close()

	for _, item := range items {
		if item.ID == 0 {
			continue
		}
		magicInt := 0
		if item.Magic {
			magicInt = 1
		}
		offlineInt := 0
		if item.Offline {
			offlineInt = 1
		}

		if _, err := stmt.Exec(
			item.ID,
			item.Name,
			item.ParentID,
			magicInt,
			offlineInt,
			string(item.CreationDate),
			string(item.ModificationDate),
		); err != nil {
			return fmt.Errorf("exec upsert folder %d: %w", item.ID, err)
		}

		if item.Magic {
			updRoot := `
				INSERT INTO sync_state (key, value, updated_at)
				VALUES ('root_folder_id', ?, CURRENT_TIMESTAMP)
				ON CONFLICT(key) DO UPDATE SET
					value = excluded.value,
					updated_at = CURRENT_TIMESTAMP
			`
			if _, err := tx.Exec(updRoot, strconv.FormatInt(item.ID, 10)); err != nil {
				return fmt.Errorf("record root_folder_id in tx: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert folders tx: %w", err)
	}
	return nil
}

// DeleteFolder removes a folder and all its descendant subfolders and media items recursively.
func (s *MetadataStore) DeleteFolder(id int64) error {
	if s == nil || s.db == nil || id == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete folder tx: %w", err)
	}
	defer tx.Rollback()

	// Recursively collect all descendant folder IDs including id itself
	query := `
		WITH RECURSIVE subfolders(id) AS (
			SELECT id FROM folders WHERE id = ?
			UNION
			SELECT f.id FROM folders f JOIN subfolders s ON f.parent_id = s.id
		)
		SELECT id FROM subfolders;
	`
	rows, err := tx.Query(query, id)
	if err != nil {
		return fmt.Errorf("query subfolder tree for %d: %w", id, err)
	}
	var folderIDs []int64
	for rows.Next() {
		var fid int64
		if err := rows.Scan(&fid); err != nil {
			rows.Close()
			return fmt.Errorf("scan subfolder id: %w", err)
		}
		folderIDs = append(folderIDs, fid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("subfolder tree rows err: %w", err)
	}

	if len(folderIDs) == 0 {
		return nil
	}

	delMediaStmt, err := tx.Prepare("DELETE FROM media WHERE folder_id = ?")
	if err != nil {
		return fmt.Errorf("prepare delete media stmt: %w", err)
	}
	defer delMediaStmt.Close()

	delFolderStmt, err := tx.Prepare("DELETE FROM folders WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare delete folder stmt: %w", err)
	}
	defer delFolderStmt.Close()

	for _, fid := range folderIDs {
		if _, err := delMediaStmt.Exec(fid); err != nil {
			return fmt.Errorf("delete media in folder %d: %w", fid, err)
		}
		if _, err := delFolderStmt.Exec(fid); err != nil {
			return fmt.Errorf("delete folder %d: %w", fid, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete folder tx: %w", err)
	}
	return nil
}

// UpsertMedia inserts or updates a single media item.
func (s *MetadataStore) UpsertMedia(item sapi.MediaItem) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.UpsertMediaBatch([]sapi.MediaItem{item}); err != nil {
		return err
	}
	if s.shouldClearTombstone() {
		if folderPath, err := s.GetFolderPath(item.FolderID); err == nil {
			s.clearTombstone(path.Clean(path.Join(folderPath, item.Name)))
		}
	}
	return nil
}

// UpsertMediaBatch inserts or updates a slice of media items within a single transaction.
func (s *MetadataStore) UpsertMediaBatch(items []sapi.MediaItem) error {
	if s == nil || s.db == nil || len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin upsert media tx: %w", err)
	}
	defer tx.Rollback()

	query := `
		INSERT INTO media (id, name, size, url, etag, folder_id, creation_date, modification_date)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			size = excluded.size,
			url = CASE WHEN excluded.url != '' THEN excluded.url ELSE media.url END,
			etag = CASE WHEN excluded.etag != '' THEN excluded.etag ELSE media.etag END,
			folder_id = excluded.folder_id,
			creation_date = CASE WHEN excluded.creation_date != '' THEN excluded.creation_date ELSE media.creation_date END,
			modification_date = CASE WHEN excluded.modification_date != '' THEN excluded.modification_date ELSE media.modification_date END
	`
	stmt, err := tx.Prepare(query)
	if err != nil {
		return fmt.Errorf("prepare upsert media stmt: %w", err)
	}
	defer stmt.Close()

	for _, item := range items {
		if item.ID == "" {
			continue
		}
		if _, err := stmt.Exec(
			item.ID,
			item.Name,
			item.Size,
			item.URL,
			item.ETag,
			item.FolderID,
			string(item.CreationDate),
			string(item.ModificationDate),
		); err != nil {
			return fmt.Errorf("exec upsert media %s: %w", item.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert media tx: %w", err)
	}
	return nil
}

// DeleteMedia deletes a media item by its string ID.
func (s *MetadataStore) DeleteMedia(id string) error {
	if s == nil || s.db == nil || id == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.db.Exec("DELETE FROM media WHERE id = ?", id)
	return err
}

// DeleteMediaByFolderAndName deletes a media item by its folder ID and name (case-insensitive).
func (s *MetadataStore) DeleteMediaByFolderAndName(folderID int64, name string) error {
	if s == nil || s.db == nil || folderID == 0 || name == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.db.Exec("DELETE FROM media WHERE folder_id = ? AND name = ? COLLATE NOCASE", folderID, name)
	return err
}

// MoveFolder updates a folder's parent ID and/or name.
func (s *MetadataStore) MoveFolder(id int64, newParentID int64, newName string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if id == 0 {
		return errors.New("cannot move folder with zero ID")
	}

	err := func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()

		var res sql.Result
		var err error

		if newParentID != 0 && newName != "" {
			res, err = s.db.Exec("UPDATE folders SET parent_id = ?, name = ? WHERE id = ?", newParentID, newName, id)
		} else if newName != "" {
			res, err = s.db.Exec("UPDATE folders SET name = ? WHERE id = ?", newName, id)
		} else if newParentID != 0 {
			res, err = s.db.Exec("UPDATE folders SET parent_id = ? WHERE id = ?", newParentID, id)
		} else {
			return nil
		}

		if err != nil {
			return fmt.Errorf("move folder %d: %w", id, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("folder %d not found", id)
		}
		return nil
	}()
	if err != nil {
		return err
	}

	// Release s.writeMu BEFORE clearTombstone to prevent deadlocks with readers taking cacheMu
	if newParentID != 0 && newName != "" && s.shouldClearTombstone() {
		if folderPath, err := s.GetFolderPath(newParentID); err == nil {
			s.clearTombstone(path.Clean(path.Join(folderPath, newName)))
		}
	}
	return nil
}

// MoveMedia updates a media item's parent folder ID and/or name.
func (s *MetadataStore) MoveMedia(id string, newFolderID int64, newName string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if id == "" {
		return errors.New("cannot move media with empty ID")
	}

	err := func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()

		var res sql.Result
		var err error

		if newFolderID != 0 && newName != "" {
			res, err = s.db.Exec("UPDATE media SET folder_id = ?, name = ? WHERE id = ?", newFolderID, newName, id)
		} else if newName != "" {
			res, err = s.db.Exec("UPDATE media SET name = ? WHERE id = ?", newName, id)
		} else if newFolderID != 0 {
			res, err = s.db.Exec("UPDATE media SET folder_id = ? WHERE id = ?", newFolderID, id)
		} else {
			return nil
		}

		if err != nil {
			return fmt.Errorf("move media %s: %w", id, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("media %s not found", id)
		}
		return nil
	}()
	if err != nil {
		return err
	}

	// Release s.writeMu BEFORE clearTombstone to prevent deadlocks with readers taking cacheMu
	if newFolderID != 0 && newName != "" && s.shouldClearTombstone() {
		if folderPath, err := s.GetFolderPath(newFolderID); err == nil {
			s.clearTombstone(path.Clean(path.Join(folderPath, newName)))
		}
	}
	return nil
}

// UpdateFolderModificationDate updates modification_date for a specific folder ID.
func (s *MetadataStore) UpdateFolderModificationDate(id int64, modTime string) error {
	if s == nil || s.db == nil || id <= 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.db.Exec("UPDATE folders SET modification_date = ? WHERE id = ?", modTime, id)
	return err
}

// TouchFolders updates modification_date for a list of folder IDs within a single transaction.
func (s *MetadataStore) TouchFolders(folderIDs []int64, modTime string) error {
	if s == nil || s.db == nil || len(folderIDs) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("UPDATE folders SET modification_date = ? WHERE id = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range folderIDs {
		if id > 0 {
			if _, err := stmt.Exec(modTime, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// --- SAPI Synchronization Logic ---

// SyncChanges queries SAPI incremental changes since last_sync_cursor and applies them
// to the local SQLite store. If forceFullBootstrap is true or no cursor exists,
// starts from "19700101T000000Z".
// Returns the number of changed items processed (deletions + additions/updates).
func (s *MetadataStore) SyncChanges(client *sapi.Client, forceFullBootstrap bool) (changedCount int, err error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if !s.syncMu.TryLock() {
		return 0, nil
	}
	defer s.syncMu.Unlock()

	if client == nil {
		return 0, errors.New("sapi client cannot be nil")
	}

	cursor, err := s.GetSyncState("last_sync_cursor")
	if err != nil {
		return 0, fmt.Errorf("get last_sync_cursor: %w", err)
	}
	if cursor == "" || forceFullBootstrap || strings.Contains(cursor, "T") {
		var count int
		if s.db.QueryRow("SELECT COUNT(*) FROM media").Scan(&count) == nil && count > 0 {
			// Si la base de datos ya tiene metadatos sincronizados, no re-descargar 73.000 items desde cero.
			// Iniciamos el cursor en el momento actual para sincronizar únicamente cambios incrementales.
			cursor = strconv.FormatInt(time.Now().UnixMilli(), 10)
			_ = s.SetSyncState("last_sync_cursor", cursor)
		} else {
			cursor = "0"
		}
	}

	resp, err := client.GetProfileChanges(cursor)
	if err != nil {
		return 0, fmt.Errorf("get profile changes: %w", err)
	}
	if resp == nil {
		return 0, nil
	}

	// Calculate cursorVal as epoch milliseconds (official SAPI format)
	cursorVal := "0"
	if resp.RequestTime > 0 {
		cursorVal = strconv.FormatInt(resp.RequestTime, 10)
	} else {
		cursorVal = strconv.FormatInt(time.Now().UnixMilli(), 10)
	}

	if !resp.HasChanges() {
		// Even without changes, advance cursor so HasInitialSync becomes true
		// and subsequent calls don't restart from beginning.
		if err := s.SetSyncState("last_sync_cursor", cursorVal); err != nil {
			return 0, fmt.Errorf("set last_sync_cursor: %w", err)
		}
		return 0, nil
	}

	// Track parent folders to update their modification_date
	modifiedParentIDs := make(map[int64]bool)
	nowModDate := cursorVal

	// 1. Delete deleted folders (folder.D)
	for _, folderID := range resp.Data.Folder.D {
		if f, fErr := s.GetFolderByID(folderID); fErr == nil && f != nil && f.ParentID > 0 {
			modifiedParentIDs[f.ParentID] = true
		}
		if err := s.DeleteFolder(folderID); err != nil {
			return changedCount, fmt.Errorf("delete folder %d: %w", folderID, err)
		}
		changedCount++
	}

	// 2. Delete deleted media (file.D, picture.D, video.D, audio.D)
	var deletedMediaIDs []int64
	deletedMediaIDs = append(deletedMediaIDs, resp.Data.File.D...)
	deletedMediaIDs = append(deletedMediaIDs, resp.Data.Picture.D...)
	deletedMediaIDs = append(deletedMediaIDs, resp.Data.Video.D...)
	deletedMediaIDs = append(deletedMediaIDs, resp.Data.Audio.D...)

	for _, mediaID := range deletedMediaIDs {
		idStr := strconv.FormatInt(mediaID, 10)
		if m, mErr := s.GetMediaByID(idStr); mErr == nil && m != nil && m.FolderID > 0 {
			modifiedParentIDs[m.FolderID] = true
		}
		if err := s.DeleteMedia(idStr); err != nil {
			return changedCount, fmt.Errorf("delete media %s: %w", idStr, err)
		}
		changedCount++
	}

	// 3. Fetch changed folders (folder.N and folder.U) via client.GetFoldersByIDs and upsert in batch
	var folderIDs []int64
	folderIDs = append(folderIDs, resp.Data.Folder.N...)
	folderIDs = append(folderIDs, resp.Data.Folder.U...)
	folderIDs = dedupeInt64(folderIDs)
	if len(folderIDs) > 0 {
		folders, err := client.GetFoldersByIDs(folderIDs)
		if err != nil {
			return changedCount, fmt.Errorf("fetch folders by ids: %w", err)
		}
		if err := s.UpsertFoldersBatch(folders); err != nil {
			return changedCount, fmt.Errorf("upsert folders batch: %w", err)
		}
		for _, f := range folders {
			if f.ParentID > 0 {
				modifiedParentIDs[f.ParentID] = true
			}
			if s.shouldClearTombstone() {
				if folderPath, pErr := s.GetFolderPath(f.ParentID); pErr == nil {
					itemPath := path.Clean(path.Join(folderPath, f.Name))
					s.clearTombstone(itemPath)
				}
			}
		}
		changedCount += len(folders)
	}

	// 4. Fetch changed media (*.U and *.N) via client.GetMediaByIDs in chunks and upsert incrementally
	var mediaIDs []int64
	mediaIDs = append(mediaIDs, resp.Data.File.U...)
	mediaIDs = append(mediaIDs, resp.Data.File.N...)
	mediaIDs = append(mediaIDs, resp.Data.Picture.U...)
	mediaIDs = append(mediaIDs, resp.Data.Picture.N...)
	mediaIDs = append(mediaIDs, resp.Data.Video.U...)
	mediaIDs = append(mediaIDs, resp.Data.Video.N...)
	mediaIDs = append(mediaIDs, resp.Data.Audio.U...)
	mediaIDs = append(mediaIDs, resp.Data.Audio.N...)
	mediaIDs = dedupeInt64(mediaIDs)
	if len(mediaIDs) > 0 {
		totalMedia := len(mediaIDs)
		processedMedia := 0
		const chunkSize = 1000
		for i := 0; i < totalMedia; i += chunkSize {
			end := i + chunkSize
			if end > totalMedia {
				end = totalMedia
			}
			chunk := mediaIDs[i:end]
			items, err := client.GetMediaByIDs(chunk)
			if err != nil {
				return changedCount, fmt.Errorf("fetch media by ids (chunk %d-%d): %w", i, end, err)
			}
			if err := s.UpsertMediaBatch(items); err != nil {
				return changedCount, fmt.Errorf("upsert media batch (chunk %d-%d): %w", i, end, err)
			}
			for _, m := range items {
				if m.FolderID > 0 {
					modifiedParentIDs[m.FolderID] = true
				}
				if s.shouldClearTombstone() {
					if folderPath, pErr := s.GetFolderPath(m.FolderID); pErr == nil {
						itemPath := path.Clean(path.Join(folderPath, m.Name))
						s.clearTombstone(itemPath)
					}
				}
			}
			processedMedia += len(items)
			changedCount += len(items)
			if processedMedia%5000 == 0 || end == totalMedia {
				logger.Log("[Metadata] 📥 Metadatos sincronizados: %d/%d...", processedMedia, totalMedia)
			}
		}
	}

	// 5. Update modification_date for parent folders that were affected
	if len(modifiedParentIDs) > 0 {
		parents := make([]int64, 0, len(modifiedParentIDs))
		for pID := range modifiedParentIDs {
			parents = append(parents, pID)
		}
		if err := s.TouchFolders(parents, nowModDate); err != nil {
			logger.Log("[Metadata] ⚠️ Error actualizando modification_date de carpetas padre: %v", err)
		}
	}

	// 6. Update last_sync_cursor formatted as %Y%m%dT%H%M%SZ (from response requesttime)
	if err := s.SetSyncState("last_sync_cursor", cursorVal); err != nil {
		return changedCount, fmt.Errorf("set last_sync_cursor: %w", err)
	}

	return changedCount, nil
}

func dedupeInt64(in []int64) []int64 {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[int64]bool, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// --- Virtual Empty Files (0-byte Emulation) ---

// VirtualEmptyFileRecord represents an emulated 0-byte file in SQLite.
type VirtualEmptyFileRecord struct {
	FolderID int64  `json:"folder_id"`
	Name     string `json:"name"`
	ModTime  string `json:"mod_time"`
}

// VirtualEmptyFilePathRecord represents an emulated 0-byte file with its resolved path.
type VirtualEmptyFilePathRecord struct {
	Path    string `json:"path"`
	ModTime string `json:"mod_time"`
}

func (s *MetadataStore) UpsertVirtualEmptyFile(folderID int64, name, modTime string) error {
	if s == nil || s.db == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("virtual empty file name cannot be empty")
	}
	query := `
		INSERT INTO virtual_empty_files (folder_id, name, mod_time)
		VALUES (?, ?, ?)
		ON CONFLICT(folder_id, name COLLATE NOCASE) DO UPDATE SET
			mod_time = excluded.mod_time
	`
	_, err := s.db.Exec(query, folderID, name, modTime)
	return err
}

func (s *MetadataStore) GetVirtualEmptyFile(folderID int64, name string) (*VirtualEmptyFileRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	query := `
		SELECT folder_id, name, mod_time
		FROM virtual_empty_files
		WHERE folder_id = ? AND name = ? COLLATE NOCASE
		LIMIT 1
	`
	var r VirtualEmptyFileRecord
	err := s.db.QueryRow(query, folderID, name).Scan(&r.FolderID, &r.Name, &r.ModTime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *MetadataStore) DeleteVirtualEmptyFile(folderID int64, name string) error {
	if s == nil || s.db == nil {
		return nil
	}
	query := `DELETE FROM virtual_empty_files WHERE folder_id = ? AND name = ? COLLATE NOCASE`
	_, err := s.db.Exec(query, folderID, strings.TrimSpace(name))
	return err
}

func (s *MetadataStore) DeleteVirtualEmptyFilesInFolder(folderID int64) error {
	if s == nil || s.db == nil {
		return nil
	}
	query := `DELETE FROM virtual_empty_files WHERE folder_id = ?`
	_, err := s.db.Exec(query, folderID)
	return err
}

func (s *MetadataStore) ListVirtualEmptyFilesInFolder(folderID int64) ([]VirtualEmptyFileRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := `
		SELECT folder_id, name, mod_time
		FROM virtual_empty_files
		WHERE folder_id = ?
		ORDER BY name COLLATE NOCASE ASC
	`
	rows, err := s.db.Query(query, folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []VirtualEmptyFileRecord
	for rows.Next() {
		var r VirtualEmptyFileRecord
		if err := rows.Scan(&r.FolderID, &r.Name, &r.ModTime); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *MetadataStore) ListAllVirtualEmptyFiles() ([]VirtualEmptyFilePathRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := `
		WITH RECURSIVE folder_path(id, path) AS (
			SELECT id, '/' FROM folders WHERE parent_id = 0 OR magic = 1
			UNION ALL
			SELECT f.id, CASE WHEN fp.path = '/' THEN '/' || f.name ELSE fp.path || '/' || f.name END
			FROM folders f
			JOIN folder_path fp ON f.parent_id = fp.id
			WHERE f.magic = 0
		)
		SELECT CASE WHEN fp.path = '/' THEN '/' || v.name ELSE fp.path || '/' || v.name END, v.mod_time
		FROM virtual_empty_files v
		JOIN folder_path fp ON v.folder_id = fp.id
		ORDER BY 1 ASC
	`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []VirtualEmptyFilePathRecord
	for rows.Next() {
		var r VirtualEmptyFilePathRecord
		if err := rows.Scan(&r.Path, &r.ModTime); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}
