package webdav

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

func setupTestWebDAVServerWithDB(t *testing.T) (*Server, *cache.MetadataStore, string, func()) {
	t.Helper()

	var uploadsCount atomic.Int32
	var manifestData []byte

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
			} else {
				folders = []map[string]interface{}{}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": folders,
				},
			})
		case p == "/sapi/media/folder" && action == "create":
			var body struct {
				Data struct {
					Name     string `json:"name"`
					ParentID int64  `json:"parentid"`
				} `json:"data"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"id":       999,
					"name":     body.Data.Name,
					"parentid": body.Data.ParentID,
				},
			})
		case p == "/sapi/upload" && action == "save":
			uploadsCount.Add(1)
			name := r.URL.Query().Get("name")
			bodyBytes, _ := io.ReadAll(r.Body)
			if strings.EqualFold(name, ManifestFileName) {
				manifestData = bodyBytes
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"id":   "888",
					"name": name,
					"url":  "/sapi/media/content?id=888",
				},
			})
		case p == "/sapi/media" && action == "get":
			var q struct {
				Data struct {
					IDs []string `json:"ids"`
				} `json:"data"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &q)
			media := make([]map[string]interface{}, 0)
			for _, id := range q.Data.IDs {
				media = append(media, map[string]interface{}{
					"id": id, "name": "file.txt", "size": 10, "folderid": 100,
					"url": "/sapi/media/content?id=" + id,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"media": media},
			})
		case p == "/sapi/media/content":
			if len(manifestData) > 0 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(manifestData)
			} else {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("content"))
			}
		case (strings.HasPrefix(p, "/sapi/media/") && action == "delete") || (p == "/sapi/media" && action == "softdelete"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))

	client, err := sapi.NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "test-vk",
	})
	if err != nil {
		mockSAPI.Close()
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(mockSAPI.URL+"/sapi", mockSAPI.URL)

	tmpDir, err := os.MkdirTemp("", "webdav_empty_test_*")
	if err != nil {
		mockSAPI.Close()
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	dbPath := filepath.Join(tmpDir, "test_metadata.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		mockSAPI.Close()
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("NewMetadataStore failed: %v", err)
	}

	// Bootstrap root in sqlite
	_ = ms.SetInitialSync("test-cursor")
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 100, Name: "/", Magic: true})
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 200, Name: "Backups", ParentID: 100})

	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})
	srv.SetMetadataStore(ms)

	cleanup := func() {
		mockSAPI.Close()
		_ = ms.Close()
		_ = os.RemoveAll(tmpDir)
	}

	return srv, ms, tmpDir, cleanup
}

func TestVirtualEmptyFiles_Lifecycle(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()

	// 1. PUT archivo de 0 bytes
	putReq := httptest.NewRequest("PUT", "/Backups/empty.txt", bytes.NewReader([]byte{}))
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusOK && putRec.Code != http.StatusNoContent {
		t.Fatalf("PUT 0-byte file failed: %d, body: %s", putRec.Code, putRec.Body.String())
	}

	// 2. Verificar que se guardó en SQLite como virtual empty file
	vf, err := ms.GetVirtualEmptyFile(200, "empty.txt")
	if err != nil || vf == nil {
		t.Fatalf("Virtual empty file not found in SQLite: %v", err)
	}
	if vf.Name != "empty.txt" {
		t.Fatalf("Expected name empty.txt, got %s", vf.Name)
	}

	// 3. PROPFIND Depth: 1 en /Backups debe listar empty.txt con tamaño 0
	propReq := httptest.NewRequest("PROPFIND", "/Backups", strings.NewReader(`<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:getcontentlength/>
    <D:resourcetype/>
  </D:prop>
</D:propfind>`))
	propReq.Header.Set("Depth", "1")
	propRec := httptest.NewRecorder()
	srv.ServeHTTP(propRec, propReq)
	if propRec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND failed: %d, body: %s", propRec.Code, propRec.Body.String())
	}
	propBody := propRec.Body.String()
	if !strings.Contains(propBody, "empty.txt") {
		t.Fatalf("PROPFIND response missing empty.txt: %s", propBody)
	}
	if !strings.Contains(propBody, "<D:getcontentlength>0</D:getcontentlength>") && !strings.Contains(propBody, "<d:getcontentlength>0</d:getcontentlength>") {
		t.Fatalf("PROPFIND response missing 0 getcontentlength for empty.txt: %s", propBody)
	}

	// 4. GET /Backups/empty.txt debe devolver 200 OK con Content-Length: 0 y cuerpo vacío
	getReq := httptest.NewRequest("GET", "/Backups/empty.txt", nil)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /Backups/empty.txt failed: %d", getRec.Code)
	}
	if getRec.Body.Len() != 0 {
		t.Fatalf("GET body not empty, got %d bytes", getRec.Body.Len())
	}
	if cl := getRec.Header().Get("Content-Length"); cl != "0" {
		t.Fatalf("Expected Content-Length 0, got '%s'", cl)
	}

	// 5. MOVE /Backups/empty.txt -> /Backups/empty_renamed.txt
	moveReq := httptest.NewRequest("MOVE", "/Backups/empty.txt", nil)
	moveReq.Header.Set("Destination", "/Backups/empty_renamed.txt")
	moveRec := httptest.NewRecorder()
	srv.ServeHTTP(moveRec, moveReq)
	if moveRec.Code != http.StatusCreated && moveRec.Code != http.StatusNoContent {
		t.Fatalf("MOVE failed: %d, body: %s", moveRec.Code, moveRec.Body.String())
	}

	// Comprobar que el viejo ya no existe y el nuevo sí
	oldVf, _ := ms.GetVirtualEmptyFile(200, "empty.txt")
	if oldVf != nil {
		t.Fatalf("Old virtual file still exists in SQLite after MOVE")
	}
	newVf, err := ms.GetVirtualEmptyFile(200, "empty_renamed.txt")
	if err != nil || newVf == nil {
		t.Fatalf("New virtual file not found in SQLite after MOVE: %v", err)
	}

	// 6. DELETE /Backups/empty_renamed.txt
	delReq := httptest.NewRequest("DELETE", "/Backups/empty_renamed.txt", nil)
	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent && delRec.Code != http.StatusOK {
		t.Fatalf("DELETE failed: %d", delRec.Code)
	}

	deletedVf, _ := ms.GetVirtualEmptyFile(200, "empty_renamed.txt")
	if deletedVf != nil {
		t.Fatalf("Virtual file still exists after DELETE")
	}
}

func TestVirtualEmptyFiles_OverwriteTransition(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()

	// 1. Subir archivo de 0 bytes
	putEmpty := httptest.NewRequest("PUT", "/Backups/file.bin", bytes.NewReader([]byte{}))
	recEmpty := httptest.NewRecorder()
	srv.ServeHTTP(recEmpty, putEmpty)
	if recEmpty.Code != http.StatusCreated && recEmpty.Code != http.StatusOK && recEmpty.Code != http.StatusNoContent {
		t.Fatalf("Initial empty PUT failed: %d", recEmpty.Code)
	}
	if vf, _ := ms.GetVirtualEmptyFile(200, "file.bin"); vf == nil {
		t.Fatalf("Expected virtual empty file in SQLite")
	}

	// 2. Sobrescribir con archivo NO vacío (10 bytes)
	putNonEmpty := httptest.NewRequest("PUT", "/Backups/file.bin", strings.NewReader("0123456789"))
	recNonEmpty := httptest.NewRecorder()
	srv.ServeHTTP(recNonEmpty, putNonEmpty)
	if recNonEmpty.Code != http.StatusCreated && recNonEmpty.Code != http.StatusOK && recNonEmpty.Code != http.StatusNoContent {
		t.Fatalf("Non-empty overwrite PUT failed: %d", recNonEmpty.Code)
	}

	// La entrada virtual debe haberse borrado
	if vf, _ := ms.GetVirtualEmptyFile(200, "file.bin"); vf != nil {
		t.Fatalf("Virtual empty file was NOT deleted after non-empty overwrite")
	}

	// 3. Sobrescribir de nuevo con 0 bytes
	putEmptyAgain := httptest.NewRequest("PUT", "/Backups/file.bin", bytes.NewReader([]byte{}))
	recEmptyAgain := httptest.NewRecorder()
	srv.ServeHTTP(recEmptyAgain, putEmptyAgain)
	if recEmptyAgain.Code != http.StatusCreated && recEmptyAgain.Code != http.StatusOK && recEmptyAgain.Code != http.StatusNoContent {
		t.Fatalf("Empty overwrite again PUT failed: %d", recEmptyAgain.Code)
	}
	if vf, _ := ms.GetVirtualEmptyFile(200, "file.bin"); vf == nil {
		t.Fatalf("Expected virtual empty file re-created in SQLite")
	}
}

func TestDeterministicDuplicates_NewestServed(t *testing.T) {
	_, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()

	// Insertar dos duplicados con mismo nombre pero distinta fecha
	_ = ms.UpsertMedia(sapi.MediaItem{
		ID:               "1001",
		Name:             "test.dat",
		FolderID:         200,
		Size:             100,
		ModificationDate: "20260101T100000Z",
	})
	_ = ms.UpsertMedia(sapi.MediaItem{
		ID:               "1002",
		Name:             "test.dat",
		FolderID:         200,
		Size:             200,
		ModificationDate: "20260102T100000Z", // Más reciente
	})

	// GetMediaByFolderAndName debe devolver la más reciente (1002)
	item, err := ms.GetMediaByFolderAndName(200, "test.dat")
	if err != nil || item == nil {
		t.Fatalf("GetMediaByFolderAndName failed: %v", err)
	}
	if item.ID != "1002" {
		t.Fatalf("Expected newest ID 1002, got %s", item.ID)
	}
	if item.Size != 200 {
		t.Fatalf("Expected newest size 200, got %d", item.Size)
	}

	// ListMediaInFolder debe devolver solo 1 entrada (la más reciente)
	list, err := ms.ListMediaInFolder(200)
	if err != nil {
		t.Fatalf("ListMediaInFolder failed: %v", err)
	}
	count := 0
	for _, m := range list {
		if strings.EqualFold(m.Name, "test.dat") {
			count++
			if m.ID != "1002" {
				t.Fatalf("ListMediaInFolder picked older duplicate ID %s", m.ID)
			}
		}
	}
	if count != 1 {
		t.Fatalf("Expected 1 unique item in list, found %d", count)
	}
}

func TestWebDAV_QuotaOnlyInFirstResponse(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()

	// Insertar varios archivos en /Backups
	for i := 1; i <= 10; i++ {
		_ = ms.UpsertMedia(sapi.MediaItem{
			ID:               fmt.Sprintf("file_%d", i),
			Name:             fmt.Sprintf("backup_%02d.dat", i),
			FolderID:         200,
			Size:             1024,
			ModificationDate: "20260101T000000Z",
		})
	}

	req := httptest.NewRequest("PROPFIND", "/Backups", strings.NewReader(`<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:quota-available-bytes/>
    <D:quota-used-bytes/>
    <D:getcontentlength/>
  </D:prop>
</D:propfind>`))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND failed: %d", rec.Code)
	}

	body := rec.Body.String()
	quotaCount := strings.Count(body, "quota-available-bytes")
	if quotaCount != 2 { // Open and close tag = 2 occurrences in the whole response
		t.Fatalf("Expected exactly 1 quota-available-bytes tag pair (2 occurrences), got %d in:\n%s", quotaCount, body)
	}
}

func TestVirtualEmptyFiles_InternalFolderHidden(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()

	// Insertar carpeta interna en SQLite
	_ = ms.UpsertFolder(sapi.FolderItem{
		ID:       500,
		Name:     ProxyInternalDir,
		ParentID: 100,
	})

	// PROPFIND Depth: 1 en raíz (/)
	req := httptest.NewRequest("PROPFIND", "/", strings.NewReader(`<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:">
  <D:prop><D:resourcetype/></D:prop>
</D:propfind>`))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND root failed: %d", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, ProxyInternalDir) {
		t.Fatalf("ProxyInternalDir %s must be hidden from PROPFIND, but was found in response: %s", ProxyInternalDir, body)
	}
}
