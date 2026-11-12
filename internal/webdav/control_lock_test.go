package webdav

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		case p == "/sapi/media/folder" && action == "delete":
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

	// 1. MKCOL en Control/@reader.uuid/
	req := httptest.NewRequest("MKCOL", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/", nil)
	req.SetBasicAuth("user", "pass")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("MKCOL expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}
	if sapiFolderCreateCount.Load() != 0 {
		t.Fatalf("expected 0 SAPI folder creates for @reader, got %d", sapiFolderCreateCount.Load())
	}

	// 2. PUT non-empty file v1.cformat inside virtual folder @reader.1111-2222-3333
	putReq := httptest.NewRequest("PUT", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/v1.cformat", bytes.NewReader([]byte("format-version-1")))
	putReq.SetBasicAuth("user", "pass")
	wPut := httptest.NewRecorder()
	srv.ServeHTTP(wPut, putReq)
	if wPut.Code != http.StatusCreated && wPut.Code != http.StatusNoContent {
		t.Fatalf("PUT v1.cformat expected 201/204, got %d", wPut.Code)
	}
	if sapiUploadCount.Load() != 0 {
		t.Fatalf("expected 0 SAPI uploads for control file, got %d", sapiUploadCount.Load())
	}

	// 3. PROPFIND on virtual folder
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

	// 4. GET v1.cformat reads back the exact content
	getReq := httptest.NewRequest("GET", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/v1.cformat", nil)
	getReq.SetBasicAuth("user", "pass")
	wGet := httptest.NewRecorder()
	srv.ServeHTTP(wGet, getReq)
	if wGet.Code != http.StatusOK {
		t.Fatalf("GET expected 200 OK, got %d", wGet.Code)
	}
	if wGet.Body.String() != "format-version-1" {
		t.Fatalf("GET content mismatch: expected 'format-version-1', got '%s'", wGet.Body.String())
	}

	// 5. DELETE virtual folder @reader.1111-2222-3333/
	delReq := httptest.NewRequest("DELETE", "/Backups/HPE.hbk/Control/@reader.1111-2222-3333/", nil)
	delReq.SetBasicAuth("user", "pass")
	wDel := httptest.NewRecorder()
	srv.ServeHTTP(wDel, delReq)
	if wDel.Code != http.StatusNoContent && wDel.Code != http.StatusOK {
		t.Fatalf("DELETE expected 204/200, got %d", wDel.Code)
	}
	if sapiFolderDeleteCount.Load() != 0 {
		t.Fatalf("expected 0 SAPI folder deletes for virtual folder, got %d", sapiFolderDeleteCount.Load())
	}
}
