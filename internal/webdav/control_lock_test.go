package webdav

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func TestControlLockVirtualEmulation(t *testing.T) {
	var sapiFolderCreateCount atomic.Int32
	var sapiFolderDeleteCount atomic.Int32
	var sapiUploadCount atomic.Int32

	mockSAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		action := r.URL.Query().Get("action")

		switch {
		case p == "/sapi/media" && action == "get-storage-space":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"used":    1000,
					"quota":   0,
					"nolimit": true,
				},
			})
		case p == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{
						{"id": 100, "name": "Root", "parentid": 0, "magic": true},
					},
				},
			})
		case p == "/sapi/media/folder" && action == "list":
			parentID := r.URL.Query().Get("parentid")
			var folders []map[string]interface{}
			if parentID == "100" || parentID == "" {
				folders = []map[string]interface{}{
					{"id": 200, "name": "Backups", "parentid": 100, "creationdate": "20260101T000000Z", "modificationdate": "20260101T000000Z"},
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": folders,
				},
			})
		case p == "/sapi/media/folder" && action == "save":
			sapiFolderCreateCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"id":   300,
					"name": "folder",
				},
			})
		case p == "/sapi/media/folder" && (action == "delete" || action == "softdelete"):
			sapiFolderDeleteCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
			})
		case p == "/sapi/upload":
			sapiUploadCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"id":   "real_media_123",
					"name": "file",
					"size": 100,
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{}})
		}
	}))
	defer mockSAPI.Close()

	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test_meta.db"
	store, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create metadata store: %v", err)
	}
	defer store.Close()

	_ = store.SetInitialSync("test-cursor")
	_ = store.UpsertFolder(sapi.FolderItem{ID: 100, Name: "/", Magic: true})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 200, Name: "Backups", ParentID: 100})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 250, Name: "HPE.hbk", ParentID: 200})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 260, Name: "Control", ParentID: 250})

	sapiClient, err := sapi.NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	sapiClient.SetBaseURLs(mockSAPI.URL+"/sapi", mockSAPI.URL)

	srv := NewServerWithAuth(sapiClient, config.AuthConfig{AuthDisabled: true})
	srv.SetMetadataStore(store)

	// 1. MKCOL en Control/@reader.uuid/ debe crear la carpeta real en SAPI
	req := httptest.NewRequest("MKCOL", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/", nil)
	req.SetBasicAuth("user", "pass")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("MKCOL expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}
	if sapiFolderCreateCount.Load() != 1 {
		t.Fatalf("expected 1 SAPI folder create for @reader, got %d", sapiFolderCreateCount.Load())
	}

	// 2a. PUT de 0 bytes v1.cformat debe emularse localmente (sin subir a SAPI)
	putReq := httptest.NewRequest("PUT", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/v1.cformat", bytes.NewReader(nil))
	putReq.SetBasicAuth("user", "pass")
	wPut := httptest.NewRecorder()
	srv.ServeHTTP(wPut, putReq)
	if wPut.Code != http.StatusCreated && wPut.Code != http.StatusNoContent {
		t.Fatalf("PUT v1.cformat expected 201/204, got %d", wPut.Code)
	}
	if sapiUploadCount.Load() != 0 {
		t.Fatalf("expected 0 SAPI uploads for 0-byte control file, got %d", sapiUploadCount.Load())
	}

	// 2b. PUT con contenido (>0 bytes) dentro de @reader debe subirse realmente a SAPI
	putNonEmptyReq := httptest.NewRequest("PUT", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/sequence_id", bytes.NewReader([]byte("format-version-1")))
	putNonEmptyReq.SetBasicAuth("user", "pass")
	wPutNE := httptest.NewRecorder()
	srv.ServeHTTP(wPutNE, putNonEmptyReq)
	if wPutNE.Code != http.StatusCreated && wPutNE.Code != http.StatusNoContent {
		t.Fatalf("PUT non-empty sequence_id expected 201/204, got %d: %s", wPutNE.Code, wPutNE.Body.String())
	}
	if sapiUploadCount.Load() != 1 {
		t.Fatalf("expected 1 SAPI upload for non-empty file, got %d", sapiUploadCount.Load())
	}

	// 3. PROPFIND on folder lists v1.cformat
	pfReq := httptest.NewRequest("PROPFIND", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333", nil)
	pfReq.Header.Set("Depth", "1")
	pfReq.SetBasicAuth("user", "pass")
	wPF := httptest.NewRecorder()
	srv.ServeHTTP(wPF, pfReq)
	if wPF.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND expected 207 MultiStatus, got %d", wPF.Code)
	}
	if !strings.Contains(wPF.Body.String(), "v1.cformat") {
		t.Fatalf("PROPFIND output does not contain v1.cformat: %s", wPF.Body.String())
	}

	// 4. GET v1.cformat reads back the 0-byte content
	getReq := httptest.NewRequest("GET", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/v1.cformat", nil)
	getReq.SetBasicAuth("user", "pass")
	wGet := httptest.NewRecorder()
	srv.ServeHTTP(wGet, getReq)
	if wGet.Code != http.StatusOK {
		t.Fatalf("GET expected 200 OK, got %d", wGet.Code)
	}
	if wGet.Body.String() != "" {
		t.Fatalf("GET content mismatch: expected empty body, got %q", wGet.Body.String())
	}

	// 5. DELETE folder @reader.1111-2222-3333/ deletes it in SAPI
	delReq := httptest.NewRequest("DELETE", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/", nil)
	delReq.SetBasicAuth("user", "pass")
	wDel := httptest.NewRecorder()
	srv.ServeHTTP(wDel, delReq)
	if wDel.Code != http.StatusNoContent && wDel.Code != http.StatusOK {
		t.Fatalf("DELETE expected 204/200, got %d", wDel.Code)
	}
	if sapiFolderDeleteCount.Load() != 1 {
		t.Fatalf("expected 1 SAPI folder delete for @reader folder, got %d", sapiFolderDeleteCount.Load())
	}
}

func TestHyperBackupWriterStateIsPersistent(t *testing.T) {
	persistent := []string{
		"/Backups/HPE.hbk/Control/@writer",
		"/Backups/HPE.hbk/Control/@writer/sequence_id",
		"/Backups/HPE.hbk/Control/@writer/sequence_id.temp",
		"/Backups/HPE.hbk/Control/@writer/seq_mapping",
		"/Backups/HPE.hbk/Control/@writer/v1.cformat",
		"/Backups/HPE.hbk/Control/@writer/v1.34.-1.0.0.none.none.cinfo",
	}
	for _, raw := range persistent {
		if isControlOrLockFile(raw) {
			t.Errorf("persistent writer state %q was classified as ephemeral", raw)
		}
	}

	ephemeral := []string{
		"/Backups/HPE.hbk/Control/lock/lock_keep_alive.@writer_version_0.abc",
		"/Backups/HPE.hbk/Control/@reader.1111-2222-3333/v1.cformat",
	}
	for _, raw := range ephemeral {
		if !isControlOrLockFile(raw) {
			t.Errorf("ephemeral control path %q was classified as persistent", raw)
		}
	}
}

func TestAutoHealHyperBackupWriterCinfo(t *testing.T) {
	tmpDir := t.TempDir()
	seqDBPath := filepath.Join(tmpDir, "seq_mapping.db")

	seqDB, err := sql.Open("sqlite", seqDBPath)
	if err != nil {
		t.Fatalf("failed to create seq_mapping db: %v", err)
	}
	if _, err := seqDB.Exec(`
		CREATE TABLE sequence_id_mapping (
			relative_path TEXT PRIMARY KEY,
			seq_id TEXT
		);
		INSERT INTO sequence_id_mapping (relative_path, seq_id) VALUES
			('Pool/0/0/1.bucket', '1'),
			('Pool/0/0/2.bucket', '15'),
			('Pool/0/0/3.bucket', '36');
	`); err != nil {
		_ = seqDB.Close()
		t.Fatalf("failed to seed seq_mapping db: %v", err)
	}
	_ = seqDB.Close()

	seqBytes, err := os.ReadFile(seqDBPath)
	if err != nil {
		t.Fatalf("failed to read seq_mapping bytes: %v", err)
	}

	mockSAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		action := r.URL.Query().Get("action")

		switch {
		case p == "/sapi/media/content":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(seqBytes)
		case p == "/sapi/media" && action == "get-storage-space":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"used": 1000, "quota": 0, "nolimit": true},
			})
		case p == "/sapi/media/folder/root" && action == "get":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{
						{"id": 100, "name": "Root", "parentid": 0, "magic": true},
					},
				},
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{}})
		}
	}))
	defer mockSAPI.Close()

	store, err := cache.NewMetadataStore(filepath.Join(tmpDir, "test_meta.db"))
	if err != nil {
		t.Fatalf("failed to create metadata store: %v", err)
	}
	defer store.Close()

	_ = store.SetInitialSync("test-cursor")
	_ = store.UpsertFolder(sapi.FolderItem{ID: 100, Name: "/", Magic: true})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 200, Name: "Backups", ParentID: 100})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 250, Name: "HPE.hbk", ParentID: 200})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 260, Name: "Control", ParentID: 250})
	_ = store.UpsertFolder(sapi.FolderItem{ID: 270, Name: "@writer", ParentID: 260})
	_ = store.UpsertMedia(sapi.MediaItem{
		ID:               "seq_map_1",
		Name:             "seq_mapping",
		Size:             int64(len(seqBytes)),
		FolderID:         270,
		URL:              mockSAPI.URL + "/sapi/media/content?id=seq_map_1",
		ModificationDate: "20260101T000000Z",
	})

	sapiClient, err := sapi.NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	sapiClient.SetBaseURLs(mockSAPI.URL+"/sapi", mockSAPI.URL)

	srv := NewServerWithAuth(sapiClient, config.AuthConfig{AuthDisabled: true})
	srv.SetMetadataStore(store)

	pfReq := httptest.NewRequest("PROPFIND", "/Backups/HPE.hbk/Control/@writer", nil)
	pfReq.Header.Set("Depth", "1")
	wPF := httptest.NewRecorder()
	srv.ServeHTTP(wPF, pfReq)
	if wPF.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND expected 207 MultiStatus, got %d: %s", wPF.Code, wPF.Body.String())
	}

	body := wPF.Body.String()
	if !strings.Contains(body, "v1.cformat") {
		t.Fatalf("expected auto-healed v1.cformat in PROPFIND response, got: %s", body)
	}
	if !strings.Contains(body, "v1.36.-1.0.0.none.none.cinfo") {
		t.Fatalf("expected auto-healed v1.36.-1.0.0.none.none.cinfo in PROPFIND response, got: %s", body)
	}

	vFiles, err := store.ListVirtualEmptyFilesInFolder(270)
	if err != nil {
		t.Fatalf("ListVirtualEmptyFilesInFolder failed: %v", err)
	}
	found := make(map[string]bool, len(vFiles))
	for _, vf := range vFiles {
		found[vf.Name] = true
	}
	if !found["v1.cformat"] || !found["v1.36.-1.0.0.none.none.cinfo"] {
		t.Fatalf("expected both virtual empty files persisted in SQLite, got: %+v", vFiles)
	}
}
