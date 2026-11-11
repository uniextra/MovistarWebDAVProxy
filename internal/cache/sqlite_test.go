package cache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func createTempStore(t *testing.T) (*MetadataStore, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_metadata.db")
	store, err := NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store, dbPath
}

func TestNewMetadataStore_PragmasAndSchema(t *testing.T) {
	store, dbPath := createTempStore(t)

	// Verify PRAGMAs
	var syncVal int
	if err := store.DB().QueryRow("PRAGMA synchronous;").Scan(&syncVal); err != nil {
		t.Fatalf("query synchronous: %v", err)
	}
	if syncVal != 1 { // 1 = NORMAL
		t.Errorf("expected synchronous=1 (NORMAL), got %d", syncVal)
	}

	var busyTimeout int
	if err := store.DB().QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("expected busy_timeout=5000, got %d", busyTimeout)
	}

	var foreignKeys int
	if err := store.DB().QueryRow("PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 0 {
		t.Errorf("expected foreign_keys=0, got %d", foreignKeys)
	}

	var journalMode string
	if err := store.DB().QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Errorf("expected journal_mode=wal, got %s", journalMode)
	}

	// Verify tables
	tables := []string{"sync_state", "folders", "media"}
	for _, tbl := range tables {
		var name string
		err := store.DB().QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		if err != nil {
			t.Fatalf("table %s does not exist in schema: %v", tbl, err)
		}
	}

	// Verify DB file exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatalf("database file was not created on disk: %s", dbPath)
	}
}

func TestSyncState(t *testing.T) {
	store, _ := createTempStore(t)

	// Key not found returns "", nil
	val, err := store.GetSyncState("missing_key")
	if err != nil {
		t.Fatalf("unexpected error on missing key: %v", err)
	}
	if val != "" {
		t.Fatalf("expected empty string for missing key, got %q", val)
	}

	// Set and Get
	if err := store.SetSyncState("k1", "v1"); err != nil {
		t.Fatalf("SetSyncState failed: %v", err)
	}
	val, err = store.GetSyncState("k1")
	if err != nil || val != "v1" {
		t.Fatalf("expected v1, got %q (err=%v)", val, err)
	}

	// Update (upsert)
	if err := store.SetSyncState("k1", "v2"); err != nil {
		t.Fatalf("SetSyncState update failed: %v", err)
	}
	val, err = store.GetSyncState("k1")
	if err != nil || val != "v2" {
		t.Fatalf("expected v2, got %q (err=%v)", val, err)
	}
}

func TestFolderCRUD_AndCaseInsensitivity(t *testing.T) {
	store, _ := createTempStore(t)

	// Initially no root folder
	rf, err := store.GetRootFolder()
	if err != nil {
		t.Fatalf("GetRootFolder error: %v", err)
	}
	if rf != nil {
		t.Fatalf("expected nil root folder, got %+v", rf)
	}

	// Upsert Root Folder
	rootItem := sapi.FolderItem{
		ID:               100,
		Name:             "RootFolder",
		ParentID:         0,
		Magic:            true,
		CreationDate:     sapi.FlexDate("2026-01-01T00:00:00Z"),
		ModificationDate: sapi.FlexDate("2026-01-01T00:00:00Z"),
	}
	if err := store.UpsertFolder(rootItem); err != nil {
		t.Fatalf("UpsertFolder failed: %v", err)
	}

	// GetRootFolder
	rf, err = store.GetRootFolder()
	if err != nil || rf == nil || rf.ID != 100 {
		t.Fatalf("expected root folder 100, got %+v (err=%v)", rf, err)
	}

	// GetFolderByID
	f, err := store.GetFolderByID(100)
	if err != nil || f == nil || f.ID != 100 {
		t.Fatalf("GetFolderByID failed: %+v (err=%v)", f, err)
	}

	// Case-insensitivity lookup
	cases := []string{"RootFolder", "rootfolder", "ROOTFOLDER", "RoOtFoLdEr"}
	for _, name := range cases {
		f, err := store.GetFolderByParentAndName(0, name)
		if err != nil || f == nil || f.ID != 100 {
			t.Errorf("GetFolderByParentAndName failed for %q: %+v (err=%v)", name, f, err)
		}
	}

	// Add subfolders
	sub1 := sapi.FolderItem{ID: 200, Name: "Documents", ParentID: 100}
	sub2 := sapi.FolderItem{ID: 300, Name: "Photos", ParentID: 100}
	if err := store.UpsertFolder(sub1); err != nil {
		t.Fatalf("UpsertFolder sub1 failed: %v", err)
	}
	if err := store.UpsertFolder(sub2); err != nil {
		t.Fatalf("UpsertFolder sub2 failed: %v", err)
	}

	subs, err := store.ListSubfolders(100)
	if err != nil {
		t.Fatalf("ListSubfolders failed: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subfolders, got %d", len(subs))
	}
	if subs[0].Name != "Documents" || subs[1].Name != "Photos" {
		t.Errorf("unexpected subfolders ordering: %+v", subs)
	}

	// MoveFolder: Rename
	if err := store.MoveFolder(300, 100, "Pictures"); err != nil {
		t.Fatalf("MoveFolder rename failed: %v", err)
	}
	f, err = store.GetFolderByID(300)
	if err != nil || f == nil || f.Name != "Pictures" {
		t.Fatalf("expected renamed folder Pictures, got %+v", f)
	}

	// MoveFolder: Parent change
	if err := store.MoveFolder(300, 200, ""); err != nil {
		t.Fatalf("MoveFolder reparent failed: %v", err)
	}
	f, err = store.GetFolderByID(300)
	if err != nil || f == nil || f.ParentID != 200 {
		t.Fatalf("expected parentID 200, got %+v", f)
	}

	// Move non-existent folder
	if err := store.MoveFolder(999, 100, "Foo"); err == nil {
		t.Fatalf("expected error moving non-existent folder, got nil")
	}
}

func TestFolder_HBKTolerance(t *testing.T) {
	store, _ := createTempStore(t)

	// Folder stored with .hbk suffix
	f1 := sapi.FolderItem{ID: 400, Name: "NAS_Backup.hbk", ParentID: 10}
	if err := store.UpsertFolder(f1); err != nil {
		t.Fatalf("UpsertFolder f1: %v", err)
	}

	// Folder stored without .hbk suffix
	f2 := sapi.FolderItem{ID: 500, Name: "DataStore", ParentID: 10}
	if err := store.UpsertFolder(f2); err != nil {
		t.Fatalf("UpsertFolder f2: %v", err)
	}

	// 1. Search without .hbk when stored with .hbk
	found1, err := store.GetFolderByParentAndName(10, "NAS_Backup")
	if err != nil || found1 == nil || found1.ID != 400 {
		t.Errorf("expected to find NAS_Backup.hbk via 'NAS_Backup': %+v (err=%v)", found1, err)
	}

	// Case-insensitive variant
	found1Lower, err := store.GetFolderByParentAndName(10, "nas_backup")
	if err != nil || found1Lower == nil || found1Lower.ID != 400 {
		t.Errorf("expected to find NAS_Backup.hbk via 'nas_backup': %+v (err=%v)", found1Lower, err)
	}

	// 2. Search with .hbk when stored without .hbk
	found2, err := store.GetFolderByParentAndName(10, "DataStore.hbk")
	if err != nil || found2 == nil || found2.ID != 500 {
		t.Errorf("expected to find DataStore via 'DataStore.hbk': %+v (err=%v)", found2, err)
	}

	found2Lower, err := store.GetFolderByParentAndName(10, "datastore.hbk")
	if err != nil || found2Lower == nil || found2Lower.ID != 500 {
		t.Errorf("expected to find DataStore via 'datastore.hbk': %+v (err=%v)", found2Lower, err)
	}
}

func TestMediaCRUD_AndCaseInsensitivity(t *testing.T) {
	store, _ := createTempStore(t)

	// Upsert media
	m1 := sapi.MediaItem{
		ID:               "media_001",
		Name:             "Report.PDF",
		Size:             2048,
		URL:              "https://example.com/download/001",
		ETag:             "etag_001",
		FolderID:         20,
		CreationDate:     sapi.FlexDate("2026-02-01T10:00:00Z"),
		ModificationDate: sapi.FlexDate("2026-02-01T12:00:00Z"),
	}
	if err := store.UpsertMedia(m1); err != nil {
		t.Fatalf("UpsertMedia failed: %v", err)
	}

	// GetMediaByID
	item, err := store.GetMediaByID("media_001")
	if err != nil || item == nil || item.ID != "media_001" {
		t.Fatalf("GetMediaByID failed: %+v (err=%v)", item, err)
	}

	// Case-insensitivity lookup
	cases := []string{"Report.PDF", "report.pdf", "REPORT.PDF", "RePoRt.PdF"}
	for _, name := range cases {
		m, err := store.GetMediaByFolderAndName(20, name)
		if err != nil || m == nil || m.ID != "media_001" {
			t.Errorf("GetMediaByFolderAndName failed for %q: %+v (err=%v)", name, m, err)
		}
	}

	// ListMediaInFolder
	m2 := sapi.MediaItem{
		ID:       "media_002",
		Name:             "Alpha.txt",
		Size:             512,
		FolderID:         20,
	}
	if err := store.UpsertMedia(m2); err != nil {
		t.Fatalf("UpsertMedia m2 failed: %v", err)
	}

	items, err := store.ListMediaInFolder(20)
	if err != nil {
		t.Fatalf("ListMediaInFolder failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 media items, got %d", len(items))
	}
	if items[0].Name != "Alpha.txt" || items[1].Name != "Report.PDF" {
		t.Errorf("unexpected media alphabetical ordering: %+v", items)
	}

	// MoveMedia: rename and reparent
	if err := store.MoveMedia("media_001", 30, "Report_Final.pdf"); err != nil {
		t.Fatalf("MoveMedia failed: %v", err)
	}
	moved, err := store.GetMediaByID("media_001")
	if err != nil || moved == nil || moved.FolderID != 30 || moved.Name != "Report_Final.pdf" {
		t.Fatalf("expected moved media in folder 30 with name Report_Final.pdf, got %+v", moved)
	}

	// DeleteMediaByFolderAndName
	if err := store.DeleteMediaByFolderAndName(30, "report_final.pdf"); err != nil {
		t.Fatalf("DeleteMediaByFolderAndName failed: %v", err)
	}
	delCheck, err := store.GetMediaByID("media_001")
	if err != nil || delCheck != nil {
		t.Fatalf("expected media_001 deleted, got %+v", delCheck)
	}

	// DeleteMedia by ID
	if err := store.DeleteMedia("media_002"); err != nil {
		t.Fatalf("DeleteMedia failed: %v", err)
	}
	delCheck2, err := store.GetMediaByID("media_002")
	if err != nil || delCheck2 != nil {
		t.Fatalf("expected media_002 deleted, got %+v", delCheck2)
	}
}

func TestRecursiveDeleteFolder(t *testing.T) {
	store, _ := createTempStore(t)

	// Tree structure:
	// 1 (Root)
	// ├── 2 (Branch A)
	// │   ├── 3 (Branch A.1)
	// │   │   └── 4 (Branch A.1.1)
	// └── 5 (Branch B)
	folders := []sapi.FolderItem{
		{ID: 1, Name: "Root", ParentID: 0, Magic: true},
		{ID: 2, Name: "BranchA", ParentID: 1},
		{ID: 3, Name: "BranchA_1", ParentID: 2},
		{ID: 4, Name: "BranchA_1_1", ParentID: 3},
		{ID: 5, Name: "BranchB", ParentID: 1},
	}
	if err := store.UpsertFoldersBatch(folders); err != nil {
		t.Fatalf("UpsertFoldersBatch: %v", err)
	}

	media := []sapi.MediaItem{
		{ID: "m1", Name: "m1.txt", FolderID: 1},
		{ID: "m2", Name: "m2.txt", FolderID: 2},
		{ID: "m3", Name: "m3.txt", FolderID: 3},
		{ID: "m4", Name: "m4.txt", FolderID: 4},
		{ID: "m5", Name: "m5.txt", FolderID: 5},
	}
	if err := store.UpsertMediaBatch(media); err != nil {
		t.Fatalf("UpsertMediaBatch: %v", err)
	}

	// Delete Branch A (ID 2)
	if err := store.DeleteFolder(2); err != nil {
		t.Fatalf("DeleteFolder(2) failed: %v", err)
	}

	// Folders 2, 3, 4 must be gone
	for _, id := range []int64{2, 3, 4} {
		f, err := store.GetFolderByID(id)
		if err != nil || f != nil {
			t.Errorf("expected folder %d to be deleted, got %+v (err=%v)", id, f, err)
		}
	}

	// Media m2, m3, m4 must be gone
	for _, mid := range []string{"m2", "m3", "m4"} {
		m, err := store.GetMediaByID(mid)
		if err != nil || m != nil {
			t.Errorf("expected media %s to be deleted, got %+v (err=%v)", mid, m, err)
		}
	}

	// Root (1) and Branch B (5) must remain intact
	f1, err := store.GetFolderByID(1)
	if err != nil || f1 == nil {
		t.Errorf("folder 1 should still exist: %v", err)
	}
	f5, err := store.GetFolderByID(5)
	if err != nil || f5 == nil {
		t.Errorf("folder 5 should still exist: %v", err)
	}

	// Media m1 and m5 must remain intact
	m1, err := store.GetMediaByID("m1")
	if err != nil || m1 == nil {
		t.Errorf("media m1 should still exist: %v", err)
	}
	m5, err := store.GetMediaByID("m5")
	if err != nil || m5 == nil {
		t.Errorf("media m5 should still exist: %v", err)
	}
}

func TestSyncChanges_MockSAPI(t *testing.T) {
	store, _ := createTempStore(t)

	var requestCount int
	var capturedFrom string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(r.URL.Path, "/profile/changes") {
			capturedFrom = r.URL.Query().Get("from")

			if capturedFrom == "19700101T000000Z" {
				// Initial full bootstrap changes
				resp := map[string]interface{}{
					"requesttime":  1794389693000,
					"responsetime": 1794389695000,
					"data": map[string]interface{}{
						"folder": map[string]interface{}{
							"N": []int64{10, 20},
						},
						"file": map[string]interface{}{
							"U": []int64{101},
						},
						"picture": map[string]interface{}{
							"U": []int64{102},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
				return
			}

			// Incremental change
			resp := map[string]interface{}{
				"requesttime":  1794389700000,
				"responsetime": 1794389701000,
				"data": map[string]interface{}{
					"folder": map[string]interface{}{
						"D": []int64{20},
					},
					"file": map[string]interface{}{
						"D": []int64{101},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if strings.Contains(r.URL.Path, "/media/folder") {
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{
						{"id": 10, "name": "Root", "parentid": 0, "magic": true},
						{"id": 20, "name": "Documents", "parentid": 10, "magic": false},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if strings.Contains(r.URL.Path, "/media") {
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"media": []map[string]interface{}{
						{"id": 101, "name": "readme.txt", "size": 100, "folderid": 20, "url": "http://example.com/101"},
						{"id": 102, "name": "photo.jpg", "size": 200, "folderid": 20, "url": "http://example.com/102"},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		http.NotFound(w, r)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	client, err := sapi.NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "token",
		RefreshToken:  "refresh",
		ValidationKey: "vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	// 1. Initial Bootstrap Sync (forceFullBootstrap=true)
	changed, err := store.SyncChanges(client, true)
	if err != nil {
		t.Fatalf("SyncChanges bootstrap failed: %v", err)
	}
	if changed != 4 { // 2 folders + 2 media
		t.Fatalf("expected 4 changed items in bootstrap, got %d", changed)
	}

	if capturedFrom != "19700101T000000Z" {
		t.Errorf("expected cursor '19700101T000000Z', got %q", capturedFrom)
	}

	// Verify items stored
	root, err := store.GetRootFolder()
	if err != nil || root == nil || root.ID != 10 {
		t.Fatalf("root folder not synced: %+v (err=%v)", root, err)
	}

	docFolder, err := store.GetFolderByID(20)
	if err != nil || docFolder == nil || docFolder.Name != "Documents" {
		t.Fatalf("subfolder not synced: %+v (err=%v)", docFolder, err)
	}

	m101, err := store.GetMediaByID("101")
	if err != nil || m101 == nil || m101.Name != "readme.txt" {
		t.Fatalf("media 101 not synced: %+v (err=%v)", m101, err)
	}

	// Verify cursor was updated to formatted time
	cursor, err := store.GetSyncState("last_sync_cursor")
	if err != nil || cursor == "" {
		t.Fatalf("last_sync_cursor not updated: %q (err=%v)", cursor, err)
	}
	expectedTime := time.UnixMilli(1794389693000).UTC().Format("20060102T150405Z")
	if cursor != expectedTime {
		t.Errorf("expected cursor %q, got %q", expectedTime, cursor)
	}

	// 2. Incremental Sync (forceFullBootstrap=false)
	changedInc, err := store.SyncChanges(client, false)
	if err != nil {
		t.Fatalf("SyncChanges incremental failed: %v", err)
	}
	if changedInc != 2 { // 1 folder delete + 1 media delete
		t.Fatalf("expected 2 changed items in incremental sync, got %d", changedInc)
	}

	// Verify folder 20 and media 101 are gone
	docDeleted, err := store.GetFolderByID(20)
	if err != nil || docDeleted != nil {
		t.Fatalf("expected folder 20 deleted, got %+v", docDeleted)
	}
	m101Deleted, err := store.GetMediaByID("101")
	if err != nil || m101Deleted != nil {
		t.Fatalf("expected media 101 deleted, got %+v", m101Deleted)
	}
	// Media 102 was in folder 20, so recursive folder deletion also deleted 102
	m102Deleted, err := store.GetMediaByID("102")
	if err != nil || m102Deleted != nil {
		t.Fatalf("expected media 102 deleted via recursive folder delete, got %+v", m102Deleted)
	}

	// Cursor updated to second requesttime
	newCursor, _ := store.GetSyncState("last_sync_cursor")
	expectedTime2 := time.UnixMilli(1794389700000).UTC().Format("20060102T150405Z")
	if newCursor != expectedTime2 {
		t.Errorf("expected updated cursor %q, got %q", expectedTime2, newCursor)
	}

	// 3. Sync with no changes
	noChangesHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"requesttime":  1794389710000,
			"responsetime": 1794389711000,
			"data":         map[string]interface{}{},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv2 := httptest.NewServer(noChangesHandler)
	defer srv2.Close()
	client.SetBaseURLs(srv2.URL+"/sapi", srv2.URL)

	count, err := store.SyncChanges(client, false)
	if err != nil {
		t.Fatalf("SyncChanges with no changes failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected count 0, got %d", count)
	}
}

func TestGetFolderPath(t *testing.T) {
	store, _ := createTempStore(t)

	_ = store.SetSyncState("root_folder_id", "1")
	_ = store.UpsertFolder(sapi.FolderItem{ID: 1, Name: "/", ParentID: 0, Magic: true})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 10, Name: "Fotos", ParentID: 1})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 20, Name: "2026", ParentID: 10})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 30, Name: "Verano", ParentID: 20})

	tests := []struct {
		id   int64
		want string
	}{
		{1, "/"},
		{10, "/Fotos"},
		{20, "/Fotos/2026"},
		{30, "/Fotos/2026/Verano"},
		{999, "/"}, // non-existent
	}

	for _, tc := range tests {
		got, err := store.GetFolderPath(tc.id)
		if err != nil {
			t.Errorf("GetFolderPath(%d) returned error: %v", tc.id, err)
		}
		if got != tc.want {
			t.Errorf("GetFolderPath(%d) = %q; want %q", tc.id, got, tc.want)
		}
	}
}

func TestSyncChanges_UpdatesParentFolderModificationDateAndClearsTombstones(t *testing.T) {
	store, _ := createTempStore(t)

	_ = store.SetSyncState("root_folder_id", "1")
	_ = store.UpsertFolder(sapi.FolderItem{ID: 1, Name: "/", ParentID: 0, Magic: true})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 10, Name: "Backup", ParentID: 1, ModificationDate: "20260101T000000Z"})

	var clearedPaths []string
	store.SetTombstoneCleaner(func(path string) {
		clearedPaths = append(clearedPaths, path)
	})

	// Mock SAPI server that returns a new file in folder 10
	sapiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "profile/changes") {
			resp := map[string]interface{}{
				"requesttime":  1794389800000,
				"responsetime": 1794389801000,
				"data": map[string]interface{}{
					"file": map[string]interface{}{
						"N": []int64{500},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.Contains(r.URL.Path, "/media") {
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"media": []map[string]interface{}{
						{
							"id":               500,
							"name":             "backup.zip",
							"size":             1024,
							"folderid":         10,
							"modificationdate": "20261111T120000Z",
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(sapiHandler)
	defer srv.Close()

	client, _ := sapi.NewClient(config.AccountConfig{RefreshToken: "dummy"})
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	changed, err := store.SyncChanges(client, false)
	if err != nil {
		t.Fatalf("SyncChanges failed: %v", err)
	}
	if changed != 1 {
		t.Fatalf("expected 1 changed item, got %d", changed)
	}

	// 1. Verify parent folder 10 modification_date was updated
	f10, err := store.GetFolderByID(10)
	if err != nil || f10 == nil {
		t.Fatalf("GetFolderByID(10) failed: %v", err)
	}
	expectedMod := time.UnixMilli(1794389800000).UTC().Format("20060102T150405Z")
	if string(f10.ModificationDate) != expectedMod {
		t.Errorf("expected parent folder modification_date %q, got %q", expectedMod, f10.ModificationDate)
	}

	// 2. Verify tombstone was cleared for "/Backup/backup.zip"
	found := false
	for _, p := range clearedPaths {
		if strings.EqualFold(p, "/Backup/backup.zip") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected '/Backup/backup.zip' to be cleared in tombstoneCleaner, got: %v", clearedPaths)
	}
}
