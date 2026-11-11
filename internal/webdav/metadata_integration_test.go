package webdav

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func TestDuplicatiTestPath_RestrictedScope(t *testing.T) {
	// Must match probe file and duplicati-*.tmp prefix
	if !isDuplicatiTestPath(DuplicatiTestProbeFileName) {
		t.Errorf("expected %s to be recognized as Duplicati probe", DuplicatiTestProbeFileName)
	}
	if !isDuplicatiTestPath("/duplicati-access-privileges-test.tmp") {
		t.Errorf("expected /duplicati-access-privileges-test.tmp to be recognized")
	}
	if !isDuplicatiTestPath("/subfolder/duplicati-12345.tmp") {
		t.Errorf("expected /subfolder/duplicati-12345.tmp to be recognized")
	}

	// Must NOT match generic .tmp files
	genericTmpFiles := []string{
		"regular.tmp",
		"/folder/regular.tmp",
		"/duplicati.tmp",
		"/duplicati-probe.txt",
		"/test.tmp",
		"/backup.tmp",
	}
	for _, p := range genericTmpFiles {
		if isDuplicatiTestPath(p) {
			t.Errorf("generic tmp path %q was incorrectly identified as Duplicati probe", p)
		}
	}
}

func TestMovistarFS_MetadataStore_ResolutionWithoutSAPI(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_test.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	defer ms.Close()

	// Seed root folder, a subfolder, and a file
	rootID := int64(1)
	if err := ms.UpsertFolder(sapi.FolderItem{ID: rootID, Name: "/", Magic: true, ParentID: 0}); err != nil {
		t.Fatalf("UpsertFolder root failed: %v", err)
	}
	if err := ms.SetSyncState("root_folder_id", "1"); err != nil {
		t.Fatalf("SetSyncState failed: %v", err)
	}
	_ = ms.SetInitialSync()

	subFolderID := int64(100)
	if err := ms.UpsertFolder(sapi.FolderItem{ID: subFolderID, Name: "DuplicatiBackups", ParentID: rootID}); err != nil {
		t.Fatalf("UpsertFolder subfolder failed: %v", err)
	}

	media := sapi.MediaItem{
		ID:               "media_999",
		Name:             "duplicati-20261111.dblock.zip.aes",
		Size:             52428800,
		FolderID:         subFolderID,
		URL:              "https://example.com/download/999",
		ModificationDate: sapi.FlexDate("20261111T100000Z"),
	}
	if err := ms.UpsertMedia(media); err != nil {
		t.Fatalf("UpsertMedia failed: %v", err)
	}

	// Create MovistarFS with nil SAPI client - all resolutions must come strictly from SQLite
	fs := NewMovistarFS(nil, ms)

	// 1. Resolve root
	folderInfo, _, isFolder, err := fs.resolvePath("/")
	if err != nil || !isFolder || folderInfo == nil || folderInfo.ID != rootID {
		t.Fatalf("resolvePath(/) failed: %v, folder: %+v", err, folderInfo)
	}

	// 2. Resolve subfolder (case insensitive)
	fInfo, _, isF, err := fs.resolvePath("/duplicatibackups")
	if err != nil || !isF || fInfo == nil || fInfo.ID != subFolderID {
		t.Fatalf("resolvePath(/duplicatibackups) failed: %v, folder: %+v", err, fInfo)
	}

	// 3. Resolve media file (mixed case)
	_, mInfo, isFileFolder, err := fs.resolvePath("/DuplicatiBackups/DUPLICATI-20261111.DBLOCK.ZIP.AES")
	if err != nil || isFileFolder || mInfo == nil || mInfo.ID != "media_999" {
		t.Fatalf("resolvePath(mixed case file) failed: %v, media: %+v", err, mInfo)
	}
	if mInfo.Size != 52428800 {
		t.Errorf("expected size 52428800, got %d", mInfo.Size)
	}

	// 4. Listing subfolders and media directly from MetadataStore
	subs, err := fs.getSubfolders(rootID)
	if err != nil {
		t.Fatalf("getSubfolders failed: %v", err)
	}
	if len(subs) != 1 || subs[0].Name != "DuplicatiBackups" {
		t.Errorf("expected 1 subfolder DuplicatiBackups, got %+v", subs)
	}

	medList, err := fs.getMedia(subFolderID)
	if err != nil {
		t.Fatalf("getMedia failed: %v", err)
	}
	if len(medList) != 1 || medList[0].ID != "media_999" {
		t.Errorf("expected 1 media item media_999, got %+v", medList)
	}

	// 5. Stat using virtual file system
	fi, err := fs.Stat(context.Background(), "/DuplicatiBackups/duplicati-20261111.dblock.zip.aes")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if fi.Size() != 52428800 || fi.IsDir() {
		t.Errorf("Stat returned unexpected FileInfo: name=%s, size=%d, isDir=%v", fi.Name(), fi.Size(), fi.IsDir())
	}
}

func TestMovistarFS_TombstonesAndUploadCache_CaseInsensitive(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_tomb_test.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	defer ms.Close()

	rootID := int64(1)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: rootID, Name: "/", Magic: true, ParentID: 0})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()

	fs := NewMovistarFS(nil, ms)

	// Add upload cache entry with lowercase key
	lowerPath := strings.ToLower("/uploads/TestFile.BIN")
	fs.cacheMu.Lock()
	fs.uploadCache[lowerPath] = &MemoryFile{
		ID:       "temp_123",
		Name:     "TestFile.BIN",
		FolderID: rootID,
		Size:     1024,
		ModTime:  time.Now(),
		CachedAt: time.Now(),
	}
	fs.cacheMu.Unlock()

	// Stat with uppercase should match
	fi, err := fs.Stat(context.Background(), "/UPLOADS/TESTFILE.BIN")
	if err != nil {
		t.Fatalf("Stat with mixed-case for uploadCache failed: %v", err)
	}
	if fi.Name() != "TestFile.BIN" || fi.Size() != 1024 {
		t.Errorf("Stat returned unexpected file info: %+v", fi)
	}

	// Mark as deleted tombstone with lowercase key
	fs.cacheMu.Lock()
	fs.deletedTombstones[lowerPath] = time.Now().Add(5 * time.Minute)
	fs.cacheMu.Unlock()

	// resolvePath with different casing must respect tombstone and return os.ErrNotExist
	_, _, _, resErr := fs.resolvePath("/uploads/testfile.bin")
	if resErr == nil {
		t.Errorf("expected ErrNotExist due to tombstone, but got nil error")
	}
}

func TestMovistarFS_Tombstone_DoesNotHideItemInSQLite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_tomb_sqlite.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	defer ms.Close()

	rootID := int64(1)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: rootID, Name: "/", Magic: true, ParentID: 0})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()

	// Insert item directly in SQLite
	media := sapi.MediaItem{
		ID:               "media_authoritative",
		Name:             "important.doc",
		Size:             2048,
		FolderID:         rootID,
		ModificationDate: sapi.FlexDate("20261111T120000Z"),
	}
	if err := ms.UpsertMedia(media); err != nil {
		t.Fatalf("UpsertMedia failed: %v", err)
	}

	fs := NewMovistarFS(nil, ms)

	// Artificially put tombstone in memory for "/important.doc"
	tombKey := strings.ToLower("/important.doc")
	fs.cacheMu.Lock()
	fs.deletedTombstones[tombKey] = time.Now().Add(15 * time.Second)
	fs.cacheMu.Unlock()

	// 1. resolvePath MUST NOT hide the item since it exists in SQLite MetadataStore
	_, mInfo, isFolder, err := fs.resolvePath("/important.doc")
	if err != nil {
		t.Fatalf("expected item to be found in SQLite despite tombstone, got err: %v", err)
	}
	if isFolder || mInfo == nil || mInfo.ID != "media_authoritative" {
		t.Fatalf("unexpected media resolved: %+v", mInfo)
	}

	// 2. The tombstone in fs.deletedTombstones should now be cleared
	fs.cacheMu.RLock()
	_, stillTomb := fs.deletedTombstones[tombKey]
	fs.cacheMu.RUnlock()
	if stillTomb {
		t.Errorf("expected tombstone for %s to be cleared, but still present", tombKey)
	}

	// 3. Test in Readdir
	fs.cacheMu.Lock()
	fs.deletedTombstones[tombKey] = time.Now().Add(15 * time.Second)
	fs.cacheMu.Unlock()

	dirReader := NewWebDAVDirReader(fs, "/", rootID)
	entries, err := dirReader.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir failed: %v", err)
	}
	found := false
	for _, fi := range entries {
		if fi.Name() == "important.doc" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Readdir did not include 'important.doc' when it exists in SQLite")
	}
}

func TestMovistarFS_MaybeTriggerSync_OnPropfind(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_sync_test.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	defer ms.Close()

	rootID := int64(1)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: rootID, Name: "/", Magic: true, ParentID: 0})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()

	syncCalled := make(chan bool, 5)
	sapiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "profile/changes") {
			syncCalled <- true
			resp := map[string]interface{}{
				"requesttime":  1794389900000,
				"responsetime": 1794389901000,
				"data":         map[string]interface{}{},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(sapiHandler)
	defer srv.Close()

	client, _ := sapi.NewClient(config.AccountConfig{RefreshToken: "active_token"})
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	davServer := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true}, ms)
	davServer.FS().SetSyncInterval(15 * time.Second)

	// Send PROPFIND request
	req := httptest.NewRequest("PROPFIND", "/", nil)
	rec := httptest.NewRecorder()
	davServer.ServeHTTP(rec, req)

	// Wait for async sync trigger
	select {
	case <-syncCalled:
		// Succeeded! Sync was triggered
	case <-time.After(2 * time.Second):
		t.Fatalf("expected MaybeTriggerSync to trigger SyncChanges on PROPFIND, but timed out")
	}
}

func TestMetadataStore_ConcurrentSyncChanges(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_concurrent_sync.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	defer ms.Close()

	var sapiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "profile/changes") {
			sapiCalls.Add(1)
			time.Sleep(50 * time.Millisecond) // Simulate slow network response
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"requesttime":  1794389900000,
				"responsetime": 1794389901000,
				"data":         map[string]interface{}{},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client, _ := sapi.NewClient(config.AccountConfig{RefreshToken: "test_token"})
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	const numCallers = 10
	var wg sync.WaitGroup
	wg.Add(numCallers)
	for i := 0; i < numCallers; i++ {
		go func() {
			defer wg.Done()
			_, _ = ms.SyncChanges(client, false)
		}()
	}
	wg.Wait()

	calls := sapiCalls.Load()
	if calls != 1 {
		t.Errorf("expected exactly 1 SAPI call due to single-flight syncMu, got %d", calls)
	}
}

func TestMetadataStore_SyncChanges_AdvancesCursorOnEmptyChanges(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_cursor_test.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	defer ms.Close()

	if ms.HasInitialSync() {
		t.Fatalf("expected HasInitialSync to be false before first sync")
	}

	reqTime := int64(1794389950000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requesttime":  reqTime,
			"responsetime": reqTime + 1000,
			"data":         map[string]interface{}{},
		})
	}))
	defer srv.Close()

	client, _ := sapi.NewClient(config.AccountConfig{RefreshToken: "test_token"})
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	changed, err := ms.SyncChanges(client, false)
	if err != nil {
		t.Fatalf("SyncChanges: %v", err)
	}
	if changed != 0 {
		t.Errorf("expected 0 changed items, got %d", changed)
	}

	if !ms.HasInitialSync() {
		t.Errorf("expected HasInitialSync to be true after empty sync")
	}

	cursor, err := ms.GetSyncState("last_sync_cursor")
	if err != nil || cursor == "" {
		t.Errorf("expected valid last_sync_cursor, got %q (err: %v)", cursor, err)
	}
}

func TestMetadataStore_NilGuardsAndConcurrentNilStore(t *testing.T) {
	var nilStore *cache.MetadataStore

	// Every method on nilStore must return zero values without panicking
	if nilStore.Path() != "" {
		t.Errorf("expected empty path on nil store")
	}
	if nilStore.DB() != nil {
		t.Errorf("expected nil DB on nil store")
	}
	if nilStore.HasInitialSync() {
		t.Errorf("expected false HasInitialSync on nil store")
	}
	if val, err := nilStore.GetSyncState("key"); val != "" || err != nil {
		t.Errorf("unexpected GetSyncState on nil store: %v, %v", val, err)
	}
	if err := nilStore.SetSyncState("k", "v"); err != nil {
		t.Errorf("unexpected SetSyncState on nil store: %v", err)
	}
	if f, err := nilStore.GetRootFolder(); f != nil || err != nil {
		t.Errorf("unexpected GetRootFolder on nil store: %v, %v", f, err)
	}
	if f, err := nilStore.GetFolderByID(1); f != nil || err != nil {
		t.Errorf("unexpected GetFolderByID on nil store: %v, %v", f, err)
	}
	if p, err := nilStore.GetFolderPath(1); p != "/" || err != nil {
		t.Errorf("unexpected GetFolderPath on nil store: %v, %v", p, err)
	}
	if f, err := nilStore.GetFolderByParentAndName(1, "sub"); f != nil || err != nil {
		t.Errorf("unexpected GetFolderByParentAndName on nil store: %v, %v", f, err)
	}
	if list, err := nilStore.ListSubfolders(1); list != nil || err != nil {
		t.Errorf("unexpected ListSubfolders on nil store: %v, %v", list, err)
	}
	if m, err := nilStore.GetMediaByID("1"); m != nil || err != nil {
		t.Errorf("unexpected GetMediaByID on nil store: %v, %v", m, err)
	}
	if m, err := nilStore.GetMediaByFolderAndName(1, "f"); m != nil || err != nil {
		t.Errorf("unexpected GetMediaByFolderAndName on nil store: %v, %v", m, err)
	}
	if list, err := nilStore.ListMediaInFolder(1); list != nil || err != nil {
		t.Errorf("unexpected ListMediaInFolder on nil store: %v, %v", list, err)
	}
	if err := nilStore.UpsertFolder(sapi.FolderItem{}); err != nil {
		t.Errorf("unexpected UpsertFolder on nil store: %v", err)
	}
	if err := nilStore.UpsertFoldersBatch(nil); err != nil {
		t.Errorf("unexpected UpsertFoldersBatch on nil store: %v", err)
	}
	if err := nilStore.DeleteFolder(1); err != nil {
		t.Errorf("unexpected DeleteFolder on nil store: %v", err)
	}
	if err := nilStore.UpsertMedia(sapi.MediaItem{}); err != nil {
		t.Errorf("unexpected UpsertMedia on nil store: %v", err)
	}
	if err := nilStore.UpsertMediaBatch(nil); err != nil {
		t.Errorf("unexpected UpsertMediaBatch on nil store: %v", err)
	}
	if err := nilStore.DeleteMedia("1"); err != nil {
		t.Errorf("unexpected DeleteMedia on nil store: %v", err)
	}
	if err := nilStore.DeleteMediaByFolderAndName(1, "f"); err != nil {
		t.Errorf("unexpected DeleteMediaByFolderAndName on nil store: %v", err)
	}
	if err := nilStore.MoveFolder(1, 2, "name"); err != nil {
		t.Errorf("unexpected MoveFolder on nil store: %v", err)
	}
	if err := nilStore.MoveMedia("1", 2, "name"); err != nil {
		t.Errorf("unexpected MoveMedia on nil store: %v", err)
	}
	if err := nilStore.UpdateFolderModificationDate(1, ""); err != nil {
		t.Errorf("unexpected UpdateFolderModificationDate on nil store: %v", err)
	}
	if err := nilStore.TouchFolders(nil, ""); err != nil {
		t.Errorf("unexpected TouchFolders on nil store: %v", err)
	}
	if n, err := nilStore.SyncChanges(nil, false); n != 0 || err != nil {
		t.Errorf("unexpected SyncChanges on nil store: %v, %v", n, err)
	}
	if err := nilStore.Close(); err != nil {
		t.Errorf("unexpected Close on nil store: %v", err)
	}
	nilStore.SetTombstoneCleaner(nil)
	nilStore.SetTombstoneFilter(nil)

	// Test concurrent SetMetadataStore(nil) during WebDAV operations
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_swap.db")
	ms, _ := cache.NewMetadataStore(dbPath)
	defer ms.Close()
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 1, Name: "/", Magic: true, ParentID: 0})
	_ = ms.SetSyncState("root_folder_id", "1")

	fs := NewMovistarFS(nil, ms)
	stop := make(chan bool)
	var wg sync.WaitGroup

	// Swapper goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		toggle := true
		for {
			select {
			case <-stop:
				return
			default:
				if toggle {
					fs.SetMetadataStore(nil)
				} else {
					fs.SetMetadataStore(ms)
				}
				toggle = !toggle
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Reader goroutines
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _, _, _ = fs.resolvePath("/")
				_, _ = fs.getSubfolders(1)
				_, _ = fs.getMedia(1)
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestMovistarFS_ConcurrentReaddirAndRename(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_deadlock.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	defer ms.Close()

	rootID := int64(1)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: rootID, Name: "/", Magic: true, ParentID: 0})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()

	workFolderID := int64(20)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: workFolderID, Name: "work", ParentID: rootID})

	for i := 1; i <= 5; i++ {
		_ = ms.UpsertMedia(sapi.MediaItem{
			ID:       fmt.Sprintf("media_%d", i),
			Name:     fmt.Sprintf("file_%d.txt", i),
			Size:     1024,
			FolderID: workFolderID,
		})
	}

	fs := NewMovistarFS(nil, ms)

	done := make(chan bool)
	go func() {
		var wg sync.WaitGroup
		// 5 goroutines doing Readdir
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 30; j++ {
					reader := NewWebDAVDirReader(fs, "/work", workFolderID)
					_, _ = reader.Readdir(0)
				}
			}()
		}
		// 5 goroutines doing MoveMedia on the metadata store
		for i := 1; i <= 5; i++ {
			idx := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 30; j++ {
					newName := fmt.Sprintf("file_%d_renamed_%d.txt", idx, j)
					_ = ms.MoveMedia(fmt.Sprintf("media_%d", idx), workFolderID, newName)
				}
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Succeeded without deadlock!
	case <-time.After(10 * time.Second):
		t.Fatalf("DEADLOCK detected in concurrent Readdir and MoveMedia")
	}
}

func TestMovistarFS_SAPIFallback_WithCooldown(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_fallback_cd.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	defer ms.Close()

	rootID := int64(1)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: rootID, Name: "/", Magic: true, ParentID: 0})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()
	_ = ms.SetSyncState("last_sync_cursor", "20261111T000000Z") // initial sync marked done

	var sapiMediaCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "subfolder") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"folder": []interface{}{}},
			})
			return
		}
		if strings.Contains(r.URL.Path, "media") {
			sapiMediaCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"media": []map[string]interface{}{
						{
							"id":       "remote_999",
							"name":     "new_remote_file.pdf",
							"size":     12345,
							"folderid": 1,
						},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client, _ := sapi.NewClient(config.AccountConfig{RefreshToken: "active_token"})
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	fs := NewMovistarFS(client, ms)

	// 1. Resolve item that is absent in SQLite, but present in SAPI
	_, mInfo, isFolder, err := fs.resolvePath("/new_remote_file.pdf")
	if err != nil || isFolder || mInfo == nil || mInfo.ID != "remote_999" {
		t.Fatalf("expected fallback to find remote_999, got: %v, media: %+v", err, mInfo)
	}

	// 2. Item must now exist in SQLite
	inDB, err := ms.GetMediaByFolderAndName(rootID, "new_remote_file.pdf")
	if err != nil || inDB == nil || inDB.ID != "remote_999" {
		t.Fatalf("expected item to be written through to SQLite, got %+v (err: %v)", inDB, err)
	}

	callsAfterFirst := sapiMediaCalls.Load()
	if callsAfterFirst < 1 {
		t.Errorf("expected at least 1 SAPI media call, got %d", callsAfterFirst)
	}

	// 3. Resolve a truly nonexistent item: should call SAPI (if cooldown elapsed or folder 1)
	fs.sapiFallbackMu.Lock()
	delete(fs.sapiFallbackCD, rootID)
	fs.sapiFallbackMu.Unlock()

	callsBeforeNonexistent := sapiMediaCalls.Load()
	_, _, _, errNotFound := fs.resolvePath("/really_nonexistent.xyz")
	if errNotFound == nil {
		t.Errorf("expected ErrNotExist, got nil")
	}
	callsAfterNonexistent := sapiMediaCalls.Load()
	if callsAfterNonexistent <= callsBeforeNonexistent {
		t.Errorf("expected SAPI call on first nonexistent lookup")
	}

	// 4. Resolve the same nonexistent item again immediately: must be in cooldown!
	_, _, _, errNotFound2 := fs.resolvePath("/really_nonexistent.xyz")
	if errNotFound2 == nil {
		t.Errorf("expected ErrNotExist on second lookup")
	}
	callsAfterSecond := sapiMediaCalls.Load()
	if callsAfterSecond != callsAfterNonexistent {
		t.Errorf("expected 0 additional SAPI calls due to cooldown, got calls before: %d, after: %d", callsAfterNonexistent, callsAfterSecond)
	}
}

func TestMovistarFS_Rename_AtomicDestination(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_rename_atomic.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	defer ms.Close()

	rootID := int64(1)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: rootID, Name: "/", Magic: true, ParentID: 0})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()

	// Source file and destination file both exist
	_ = ms.UpsertMedia(sapi.MediaItem{ID: "src_1", Name: "source.txt", Size: 10, FolderID: rootID})
	_ = ms.UpsertMedia(sapi.MediaItem{ID: "dst_1", Name: "dest.txt", Size: 20, FolderID: rootID})

	var shouldFailSAPI atomic.Bool
	shouldFailSAPI.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if shouldFailSAPI.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "server error"})
			return
		}
		// Success response
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
	}))
	defer srv.Close()

	client, _ := sapi.NewClient(config.AccountConfig{RefreshToken: "active_token"})
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	fs := NewMovistarFS(client, ms)

	// 1. Rename with failing SAPI: destination must NOT be deleted
	err = fs.Rename(context.Background(), "/source.txt", "/dest.txt")
	if err == nil {
		t.Fatalf("expected Rename to fail when SAPI fails")
	}

	// Verify dst_1 still exists in SQLite
	dstMedia, err := ms.GetMediaByFolderAndName(rootID, "dest.txt")
	if err != nil || dstMedia == nil || dstMedia.ID != "dst_1" {
		t.Fatalf("destination file dst_1 was prematurely deleted despite failure! got: %+v (err: %v)", dstMedia, err)
	}

	// 2. Now allow SAPI to succeed
	shouldFailSAPI.Store(false)
	err = fs.Rename(context.Background(), "/source.txt", "/dest.txt")
	if err != nil {
		t.Fatalf("expected Rename to succeed, got: %v", err)
	}

	// Verify dst_1 was cleaned up from SQLite after successful move
	oldDst, _ := ms.GetMediaByID("dst_1")
	if oldDst != nil {
		t.Errorf("expected old destination dst_1 to be deleted after successful move")
	}

	// And src_1 is now named dest.txt
	movedSrc, _ := ms.GetMediaByID("src_1")
	if movedSrc == nil || movedSrc.Name != "dest.txt" {
		t.Errorf("expected src_1 to be renamed to dest.txt, got %+v", movedSrc)
	}
}

func TestMovistarFS_RemoveAll_And_Rename_WithSQLite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "meta_remove_rename_sqlite.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	defer ms.Close()

	rootID := int64(1)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: rootID, Name: "/", Magic: true, ParentID: 0})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()

	// Seed folder and file
	folderID := int64(10)
	_ = ms.UpsertFolder(sapi.FolderItem{ID: folderID, Name: "SubFolder", ParentID: rootID})
	_ = ms.UpsertMedia(sapi.MediaItem{ID: "m_1", Name: "test.txt", Size: 100, FolderID: folderID})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
	}))
	defer srv.Close()

	client, _ := sapi.NewClient(config.AccountConfig{RefreshToken: "active_token"})
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	fs := NewMovistarFS(client, ms)

	// 1. Test RemoveAll on media file
	err = fs.RemoveAll(context.Background(), "/SubFolder/test.txt")
	if err != nil {
		t.Fatalf("RemoveAll file failed: %v", err)
	}

	// Verify row deleted from SQLite media table
	item, err := ms.GetMediaByID("m_1")
	if err != nil || item != nil {
		t.Errorf("expected media m_1 to be deleted from SQLite, got: %+v (err: %v)", item, err)
	}

	// Verify tombstone is set with lowercase key
	fs.cacheMu.RLock()
	_, isTomb := fs.deletedTombstones[strings.ToLower("/subfolder/test.txt")]
	fs.cacheMu.RUnlock()
	if !isTomb {
		t.Errorf("expected tombstone in lowercase for /subfolder/test.txt")
	}

	// 2. Test RemoveAll on folder
	err = fs.RemoveAll(context.Background(), "/SubFolder")
	if err != nil {
		t.Fatalf("RemoveAll folder failed: %v", err)
	}

	// Verify folder row deleted from SQLite folders table
	fol, err := ms.GetFolderByID(folderID)
	if err != nil || fol != nil {
		t.Errorf("expected folder %d to be deleted from SQLite, got: %+v (err: %v)", folderID, fol, err)
	}

	// Verify folder tombstone in lowercase
	fs.cacheMu.RLock()
	_, isFolTomb := fs.deletedTombstones[strings.ToLower("/subfolder")]
	fs.cacheMu.RUnlock()
	if !isFolTomb {
		t.Errorf("expected tombstone in lowercase for /subfolder")
	}
}
