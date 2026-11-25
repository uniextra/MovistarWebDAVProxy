package webdav

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func seedLargeFolderStore(t *testing.T, count int) *cache.MetadataStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "large_propfind.db")
	ms, err := cache.NewMetadataStore(dbPath)
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })

	if err := ms.UpsertFolder(sapi.FolderItem{
		ID: 100, Name: "Root", ParentID: 0, Magic: true, ModificationDate: "20261124T100000Z",
	}); err != nil {
		t.Fatalf("UpsertFolder root: %v", err)
	}
	if err := ms.UpsertFolder(sapi.FolderItem{
		ID: 200, Name: "Duplicati", ParentID: 100, ModificationDate: "20261124T100000Z",
	}); err != nil {
		t.Fatalf("UpsertFolder Duplicati: %v", err)
	}
	if err := ms.SetSyncState("root_folder_id", "100"); err != nil {
		t.Fatalf("SetSyncState: %v", err)
	}
	if err := ms.SetInitialSync(); err != nil {
		t.Fatalf("SetInitialSync: %v", err)
	}

	items := make([]sapi.MediaItem, 0, count)
	for i := 0; i < count; i++ {
		items = append(items, sapi.MediaItem{
			ID:               fmt.Sprintf("dup-%05d", i),
			Name:             fmt.Sprintf("duplicati-b%05d.dblock.zip.aes", i),
			Size:             50 * 1024 * 1024,
			FolderID:         200,
			URL:              fmt.Sprintf("/sapi/media/content?id=dup-%05d", i),
			CreationDate:     "20261124T100000Z",
			ModificationDate: "20261124T100000Z",
		})
	}
	if err := ms.UpsertMediaBatch(items); err != nil {
		t.Fatalf("UpsertMediaBatch: %v", err)
	}
	return ms
}

func TestWebDAV_LargeFolderPropfindAndQuotaFirstOnly(t *testing.T) {
	const numFiles = 3000
	ms := seedLargeFolderStore(t, numFiles)

	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()
	srv.fs.SetMetadataStore(ms)
	srv.fs.RecordSyncTime(time.Now())

	propBody := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
	req := httptest.NewRequest("PROPFIND", "/Duplicati/", strings.NewReader(propBody))
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()

	start := time.Now()
	srv.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /Duplicati/ got %d, want 207: %s", rec.Code, rec.Body.String())
	}

	xmlResp := rec.Body.String()
	if gotCL := rec.Header().Get("Content-Length"); gotCL != strconv.Itoa(len(xmlResp)) {
		t.Fatalf("Content-Length=%q, want %d", gotCL, len(xmlResp))
	}
	gotResponses := strings.Count(xmlResp, "<D:response>")
	if gotResponses != numFiles+1 {
		t.Fatalf("got %d <D:response> blocks, want %d", gotResponses, numFiles+1)
	}
	if gotQuota := strings.Count(xmlResp, ":quota-available-bytes>"); gotQuota != 2 {
		// apertura + cierre de tag en el primer <D:response> únicamente
		t.Fatalf("expected quota-available-bytes once (2 tag occurrences), got %d", gotQuota)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("PROPFIND of %d files took too long: %s", numFiles, elapsed)
	}
}

func TestPropfindDirCache_AvoidsSQLitePerChildAfterReaddir(t *testing.T) {
	ms := seedLargeFolderStore(t, 200)
	mfs := NewMovistarFS(nil, ms)
	mfs.RecordSyncTime(time.Now())

	ctx := withPropfindCache(t.Context())
	dirFile, err := mfs.OpenFile(ctx, "/Duplicati", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(/Duplicati): %v", err)
	}
	entries, err := dirFile.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	if len(entries) != 200 {
		t.Fatalf("expected 200 entries, got %d", len(entries))
	}
	_ = dirFile.Close()

	// Cerramos SQLite por completo: Stat y OpenFile dentro del mismo ctx PROPFIND
	// deben resolverse al 100 % desde propfindDirCache sin tocar SQLite.
	_ = ms.Close()

	target := "/Duplicati/duplicati-b00123.dblock.zip.aes"
	fi, err := mfs.Stat(ctx, target)
	if err != nil {
		t.Fatalf("Stat(%s) tras cerrar SQLite debe usar caché PROPFIND: %v", target, err)
	}
	if fi.Size() != 50*1024*1024 {
		t.Fatalf("unexpected size: %d", fi.Size())
	}

	f, err := mfs.OpenFile(ctx, target, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(%s) tras cerrar SQLite debe usar caché PROPFIND: %v", target, err)
	}
	_ = f.Close()
}

func TestWebDAV_PropfindCaseInsensitivePreservesExactCollision(t *testing.T) {
	t.Setenv("WEBDAV_CASE_INSENSITIVE", "true")
	ms := seedLargeFolderStore(t, 0)
	if err := ms.UpsertMediaBatch([]sapi.MediaItem{
		{ID: "case-upper", Name: "A.txt", Size: 111, FolderID: 200, CreationDate: "20261124T100000Z", ModificationDate: "20261124T100000Z"},
		{ID: "case-lower", Name: "a.txt", Size: 222, FolderID: 200, CreationDate: "20261124T100000Z", ModificationDate: "20261124T100000Z"},
	}); err != nil {
		t.Fatalf("UpsertMediaBatch: %v", err)
	}

	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()
	srv.fs.SetMetadataStore(ms)
	srv.fs.RecordSyncTime(time.Now())

	propBody := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
	req := httptest.NewRequest("PROPFIND", "/Duplicati/", strings.NewReader(propBody))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<D:href>/Duplicati/A.txt</D:href>",
		"<D:getcontentlength>111</D:getcontentlength>",
		"<D:href>/Duplicati/a.txt</D:href>",
		"<D:getcontentlength>222</D:getcontentlength>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in response:\n%s", want, body)
		}
	}
}

func TestWebDAV_PropfindRecentUploadOverridesSQLiteMedia(t *testing.T) {
	ms := seedLargeFolderStore(t, 1)
	if err := ms.UpsertMedia(sapi.MediaItem{
		ID:               "old-id-1",
		Name:             "report.bin",
		Size:             100,
		FolderID:         200,
		CreationDate:     "20261124T100000Z",
		ModificationDate: "20261124T100000Z",
	}); err != nil {
		t.Fatalf("UpsertMedia: %v", err)
	}

	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()
	srv.fs.SetMetadataStore(ms)
	srv.fs.RecordSyncTime(time.Now())

	srv.fs.cacheMu.Lock()
	srv.fs.uploadCache[cacheKey("/Duplicati/report.bin")] = &MemoryFile{
		ID:       "old-id-1",
		Name:     "report.bin",
		FolderID: 200,
		Size:     9876,
		ModTime:  time.Now().UTC(),
		CachedAt: time.Now(),
	}
	srv.fs.cacheMu.Unlock()

	propBody := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
	req := httptest.NewRequest("PROPFIND", "/Duplicati/", strings.NewReader(propBody))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<D:getcontentlength>9876</D:getcontentlength>") {
		t.Fatalf("expected fresher uploadCache size 9876 in response:\n%s", body)
	}
	if strings.Contains(body, "<D:getcontentlength>100</D:getcontentlength>") {
		t.Fatalf("stale SQLite size 100 must not be served when uploadCache is newer:\n%s", body)
	}
}

func TestWebDAV_PropfindUppercaseQuotaCleanedInChildren(t *testing.T) {
	ms := seedLargeFolderStore(t, 2)
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()
	srv.fs.SetMetadataStore(ms)
	srv.fs.RecordSyncTime(time.Now())

	propBody := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:QUOTA-AVAILABLE-BYTES/><D:QUOTA-USED-BYTES/></D:prop></D:propfind>`
	req := httptest.NewRequest("PROPFIND", "/Duplicati/", strings.NewReader(propBody))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "QUOTA-") {
		t.Fatalf("uppercase QUOTA-* tags should be cleaned from child responses, got:\n%s", body)
	}
	if got := strings.Count(body, ":quota-available-bytes>"); got != 2 {
		t.Fatalf("expected exactly 1 quota-available-bytes pair (2 occurrences), got %d:\n%s", got, body)
	}
}
