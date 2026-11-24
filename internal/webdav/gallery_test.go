package webdav

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

type fakeGalleryCloud struct {
	mu           sync.Mutex
	rootSubdirs  []map[string]interface{}
	filesByID    map[string][]byte
	metaPayloads []map[string]interface{}
	deletedCalls []string
	nextID       int64
}

func newFakeGalleryServer(t *testing.T, realConflict bool) (*Server, *cache.MetadataStore, *fakeGalleryCloud) {
	t.Helper()
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("SAPI_OFFICIAL_CLIENT", "true")

	cloud := &fakeGalleryCloud{
		filesByID: map[string][]byte{},
		nextID:    9000,
	}
	if realConflict {
		cloud.rootSubdirs = []map[string]interface{}{
			{"id": 500, "name": "_gallery", "parentid": 100, "modificationdate": "20261120T100000Z"},
		}
	}

	var srvURL string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true, "modificationdate": "20261120T100000Z"}},
			}})
		case r.URL.Path == "/sapi/media/folder" && action == "list":
			cloud.mu.Lock()
			var subs []map[string]interface{}
			if r.URL.Query().Get("parentid") == "100" {
				subs = cloud.rootSubdirs
			}
			cloud.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"folders": subs}})
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save-metadata":
			var body struct {
				Data map[string]interface{} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			cloud.mu.Lock()
			cloud.nextID++
			id := strconv.FormatInt(cloud.nextID, 10)
			if existing, ok := body.Data["id"].(string); ok && existing != "" {
				id = existing
			}
			cloud.metaPayloads = append(cloud.metaPayloads, body.Data)
			cloud.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": "Media metadata saved successfully",
				"id":      id,
			})
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save":
			id := r.Header.Get("X-Funambol-Id")
			b, _ := io.ReadAll(r.Body)
			cloud.mu.Lock()
			if id == "" {
				cloud.nextID++
				id = strconv.FormatInt(cloud.nextID, 10)
			}
			cloud.filesByID[id] = b
			cloud.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": "Media uploaded successfully",
				"id":      id,
				"status":  "C",
				"etag":    "etag-" + id,
			})
		case r.URL.Path == "/sapi/media" && action == "get":
			var body struct {
				Data struct {
					IDs []json.RawMessage `json:"ids"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var out []map[string]interface{}
			cloud.mu.Lock()
			for _, raw := range body.Data.IDs {
				id := strings.Trim(string(raw), `"`)
				if content, ok := cloud.filesByID[id]; ok {
					out = append(out, map[string]interface{}{
						"id":   id,
						"size": len(content),
						"url":  srvURL + "/download/" + id,
						"etag": "etag-" + id,
					})
				}
			}
			cloud.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": out}})
		case strings.HasPrefix(r.URL.Path, "/download/"):
			id := strings.TrimPrefix(r.URL.Path, "/download/")
			cloud.mu.Lock()
			content, ok := cloud.filesByID[id]
			cloud.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(content)
		case strings.HasPrefix(r.URL.Path, "/sapi/media") && (action == "softdelete" || action == "delete"):
			id := r.URL.Query().Get("id")
			b, _ := io.ReadAll(r.Body)
			if id == "" {
				id = strings.TrimSpace(string(b))
			}
			cloud.mu.Lock()
			cloud.deletedCalls = append(cloud.deletedCalls, r.URL.Path+"?"+r.URL.RawQuery+":"+id)
			cloud.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":"Item successfully moved to trash"}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	srvURL = mock.URL
	t.Cleanup(mock.Close)

	dir := t.TempDir()
	ms, err := cache.NewMetadataStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })

	_ = ms.SetSyncState("root_folder_id", "100")
	_ = ms.SetInitialSync("test-cursor")
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 100, Name: "/", Magic: true, ParentID: 0})
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 200, Name: "Documentos", ParentID: 100, ModificationDate: "20261120T100000Z"})
	if realConflict {
		_ = ms.UpsertFolder(sapi.FolderItem{ID: 500, Name: "_gallery", ParentID: 100, ModificationDate: "20261120T100000Z"})
	}

	client, err := sapi.NewClient(config.AccountConfig{
		Provider: "movistar", Phone: "612345678",
		AccessToken: "at", RefreshToken: "rt", ValidationKey: "vk", JSessionID: "js", DeviceID: "fol-abc",
	}, sapi.WithValidationPolling(5*time.Millisecond, 5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	client.SetPacingInterval(0)

	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true}, ms)
	if fs := srv.FS(); fs != nil {
		fs.RecordSyncTime(time.Now())
	}
	return srv, ms, cloud
}

func doDAV(t *testing.T, srv *Server, method, target string, body []byte, depth string, headers ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func seedGallerySampleData(t *testing.T, ms *cache.MetadataStore, cloud *fakeGalleryCloud) {
	t.Helper()
	cloud.mu.Lock()
	cloud.filesByID["100"] = []byte("bytes-foto-100")
	cloud.filesByID["200"] = []byte("bytes-foto-200")
	cloud.filesByID["300"] = []byte("bytes-invierno-300")
	cloud.filesByID["400"] = []byte("bytes-sin-fecha-400")
	cloud.filesByID["500"] = []byte("bytes-en-carpeta-500")
	cloud.mu.Unlock()

	rows := []sapi.MediaItem{
		{ID: "100", Name: "foto.jpg", Size: int64(len("bytes-foto-100")), URL: "", FolderID: 0, CreationDate: "20261120T100000Z", ModificationDate: "20261120T100000Z"},
		{ID: "200", Name: "foto.jpg", Size: int64(len("bytes-foto-200")), URL: "", FolderID: 0, CreationDate: "20261120T100000Z", ModificationDate: "20261120T100000Z"},
		{ID: "300", Name: "enero.mp4", Size: int64(len("bytes-invierno-300")), URL: "", FolderID: 0, CreationDate: "20260105T080000Z", ModificationDate: "20260105T080000Z"},
		{ID: "400", Name: "sin_fecha.png", Size: int64(len("bytes-sin-fecha-400")), URL: "", FolderID: 0, CreationDate: "-62135596800000", ModificationDate: "0"},
		{ID: "500", Name: "privado.jpg", Size: int64(len("bytes-en-carpeta-500")), URL: "", FolderID: 200, CreationDate: "20261120T100000Z", ModificationDate: "20261120T100000Z"},
	}
	for _, r := range rows {
		if err := ms.UpsertMedia(r); err != nil {
			t.Fatalf("UpsertMedia(%s): %v", r.ID, err)
		}
	}
}

func TestGallery_DisabledByDefault(t *testing.T) {
	t.Setenv("GALLERY_ENABLED", "")
	srv, ms, cloud := newFakeGalleryServer(t, false)
	seedGallerySampleData(t, ms, cloud)

	rec := doDAV(t, srv, "PROPFIND", "/", nil, "1")
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "_gallery") {
		t.Errorf("PROPFIND / no debe incluir _gallery por defecto: %s", rec.Body.String())
	}

	rec2 := doDAV(t, srv, "PROPFIND", "/_gallery", nil, "0")
	if rec2.Code != http.StatusNotFound {
		t.Errorf("PROPFIND /_gallery = %d, esperado 404", rec2.Code)
	}
}

func TestGallery_ConflictWithRealRootFolder(t *testing.T) {
	t.Setenv("GALLERY_ENABLED", "true")
	t.Setenv("GALLERY_UPLOAD", "true")
	srv, ms, cloud := newFakeGalleryServer(t, true)
	seedGallerySampleData(t, ms, cloud)
	if err := ms.UpsertMedia(sapi.MediaItem{
		ID: "555", Name: "real_doc.txt", Size: 4, FolderID: 500, ModificationDate: "20261120T100000Z",
	}); err != nil {
		t.Fatal(err)
	}

	rec := doDAV(t, srv, "PROPFIND", "/_gallery", nil, "1")
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /_gallery = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "real_doc.txt") {
		t.Errorf("debe prevalecer la carpeta real _gallery: %s", body)
	}
	if strings.Contains(body, "foto.jpg") || strings.Contains(body, "/_gallery/2026") || strings.Contains(body, "/_gallery/unknown") {
		t.Errorf("no debe montar galería virtual si hay carpeta real con el mismo nombre: %s", body)
	}
}

func TestGallery_YearMonthAndFlatLayoutsAndDisambiguationAndGet(t *testing.T) {
	t.Setenv("GALLERY_ENABLED", "true")
	t.Setenv("GALLERY_LAYOUT", "year-month")
	srv, ms, cloud := newFakeGalleryServer(t, false)
	seedGallerySampleData(t, ms, cloud)

	// 1. Raíz incluye _gallery
	recRoot := doDAV(t, srv, "PROPFIND", "/", nil, "1")
	if recRoot.Code != http.StatusMultiStatus || !strings.Contains(recRoot.Body.String(), "_gallery") {
		t.Fatalf("PROPFIND / (%d) no incluye _gallery: %s", recRoot.Code, recRoot.Body.String())
	}

	// 2. /_gallery lista años y unknown
	recYears := doDAV(t, srv, "PROPFIND", "/_gallery", nil, "1")
	if recYears.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /_gallery = %d", recYears.Code)
	}
	yearsXML := recYears.Body.String()
	if !strings.Contains(yearsXML, "/_gallery/2026") || !strings.Contains(yearsXML, "/_gallery/unknown") {
		t.Errorf("años faltantes en /_gallery: %s", yearsXML)
	}

	// 3. /_gallery/2026 lista 01 y 11
	recMonths := doDAV(t, srv, "PROPFIND", "/_gallery/2026", nil, "1")
	if recMonths.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /_gallery/2026 = %d", recMonths.Code)
	}
	monthsXML := recMonths.Body.String()
	if !strings.Contains(monthsXML, "/_gallery/2026/01") || !strings.Contains(monthsXML, "/_gallery/2026/11") {
		t.Errorf("meses faltantes en /_gallery/2026: %s", monthsXML)
	}

	// 4. /_gallery/2026/11 desambigua duplicados y excluye folder_id=200
	recNov := doDAV(t, srv, "PROPFIND", "/_gallery/2026/11", nil, "1")
	if recNov.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /_gallery/2026/11 = %d", recNov.Code)
	}
	novXML := recNov.Body.String()
	if !strings.Contains(novXML, "foto.jpg") || !strings.Contains(novXML, "foto%20%28200%29.jpg") && !strings.Contains(novXML, "foto (200).jpg") {
		t.Errorf("duplicado no desambiguado en /_gallery/2026/11: %s", novXML)
	}
	if strings.Contains(novXML, "privado.jpg") {
		t.Errorf("privado.jpg (folder_id=200) no debe aparecer en galería")
	}

	// 5. GET de original y duplicado desambiguado devuelven el contenido exacto
	getOrig := doDAV(t, srv, "GET", "/_gallery/2026/11/foto.jpg", nil, "")
	if getOrig.Code != http.StatusOK || getOrig.Body.String() != "bytes-foto-100" {
		t.Errorf("GET orig = %d %q", getOrig.Code, getOrig.Body.String())
	}
	getDup := doDAV(t, srv, "GET", "/_gallery/2026/11/foto%20(200).jpg", nil, "")
	if getDup.Code != http.StatusOK || getDup.Body.String() != "bytes-foto-200" {
		t.Errorf("GET dup = %d %q", getDup.Code, getDup.Body.String())
	}
	getUnknown := doDAV(t, srv, "GET", "/_gallery/unknown/sin_fecha.png", nil, "")
	if getUnknown.Code != http.StatusOK || getUnknown.Body.String() != "bytes-sin-fecha-400" {
		t.Errorf("GET unknown = %d %q", getUnknown.Code, getUnknown.Body.String())
	}

	// 6. Layout flat
	t.Setenv("GALLERY_LAYOUT", "flat")
	recFlat := doDAV(t, srv, "PROPFIND", "/_gallery", nil, "1")
	if recFlat.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND flat /_gallery = %d", recFlat.Code)
	}
	flatXML := recFlat.Body.String()
	for _, want := range []string{"foto.jpg", "200", "enero.mp4", "sin_fecha.png"} {
		if !strings.Contains(flatXML, want) {
			t.Errorf("flat XML sin %q: %s", want, flatXML)
		}
	}
	if strings.Contains(flatXML, "privado.jpg") {
		t.Errorf("flat no debe incluir folder_id>0")
	}

	// 7. Vista HTML navegable desde navegador web (/, carpetas normales y /_gallery)
	t.Setenv("GALLERY_LAYOUT", "year-month")
	recRootHTML := doDAV(t, srv, "GET", "/", nil, "")
	if recRootHTML.Code != http.StatusOK || !strings.Contains(recRootHTML.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / HTML = %d, Content-Type=%q", recRootHTML.Code, recRootHTML.Header().Get("Content-Type"))
	}
	if !strings.Contains(recRootHTML.Body.String(), `href="/Documentos"`) || !strings.Contains(recRootHTML.Body.String(), `href="/_gallery"`) {
		t.Errorf("HTML / debe enlazar a /Documentos y /_gallery: %s", recRootHTML.Body.String())
	}
	recDocsHTML := doDAV(t, srv, "GET", "/Documentos", nil, "")
	if recDocsHTML.Code != http.StatusOK || !strings.Contains(recDocsHTML.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET /Documentos HTML = %d, Content-Type=%q", recDocsHTML.Code, recDocsHTML.Header().Get("Content-Type"))
	}
	if !strings.Contains(recDocsHTML.Body.String(), `href="/Documentos/privado.jpg"`) || !strings.Contains(recDocsHTML.Body.String(), `href="/"`) {
		t.Errorf("HTML /Documentos debe incluir /Documentos/privado.jpg y enlace padre /: %s", recDocsHTML.Body.String())
	}
	recDocsFile := doDAV(t, srv, "GET", "/Documentos/privado.jpg", nil, "")
	if recDocsFile.Code != http.StatusOK || recDocsFile.Body.String() != "bytes-en-carpeta-500" {
		t.Errorf("GET /Documentos/privado.jpg = %d %q", recDocsFile.Code, recDocsFile.Body.String())
	}
	recHTML := doDAV(t, srv, "GET", "/_gallery", nil, "")
	if recHTML.Code != http.StatusOK || !strings.Contains(recHTML.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET /_gallery HTML = %d, Content-Type=%q", recHTML.Code, recHTML.Header().Get("Content-Type"))
	}
	if !strings.Contains(recHTML.Body.String(), "/_gallery/2026") || !strings.Contains(recHTML.Body.String(), `href="/"`) {
		t.Errorf("HTML /_gallery sin enlace a /_gallery/2026 o padre /: %s", recHTML.Body.String())
	}
	recHTMLNov := doDAV(t, srv, "GET", "/_gallery/2026/11", nil, "")
	if recHTMLNov.Code != http.StatusOK || !strings.Contains(recHTMLNov.Body.String(), "foto.jpg") {
		t.Errorf("HTML /_gallery/2026/11 = %d, sin foto.jpg", recHTMLNov.Code)
	}
}

func TestGallery_Phase1ReadOnlyBlocksAllWrites(t *testing.T) {
	t.Setenv("GALLERY_ENABLED", "true")
	t.Setenv("GALLERY_LAYOUT", "year-month")
	t.Setenv("GALLERY_UPLOAD", "false")
	t.Setenv("GALLERY_DELETE", "false")
	srv, ms, cloud := newFakeGalleryServer(t, false)
	seedGallerySampleData(t, ms, cloud)

	if rec := doDAV(t, srv, "PUT", "/_gallery/2026/11/bloqueada.jpg", []byte("jpg"), ""); rec.Code != http.StatusForbidden {
		t.Errorf("PUT fase 1 = %d, esperado 403", rec.Code)
	}
	if rec := doDAV(t, srv, "DELETE", "/_gallery/2026/11/foto.jpg", nil, ""); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE file fase 1 = %d, esperado 403", rec.Code)
	}
	if rec := doDAV(t, srv, "DELETE", "/_gallery/2026", nil, ""); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE dir fase 1 = %d, esperado 403", rec.Code)
	}
	if rec := doDAV(t, srv, "MKCOL", "/_gallery/sub", nil, ""); rec.Code != http.StatusForbidden {
		t.Errorf("MKCOL fase 1 = %d, esperado 403", rec.Code)
	}
	if rec := doDAV(t, srv, "MOVE", "/_gallery/2026/11/foto.jpg", nil, "", [2]string{"Destination", "http://example.com/Documentos/foto.jpg"}); rec.Code != http.StatusForbidden {
		t.Errorf("MOVE out fase 1 = %d, esperado 403", rec.Code)
	}
	if rec := doDAV(t, srv, "COPY", "/privado.jpg", nil, "", [2]string{"Destination", "http://example.com/_gallery/privado.jpg"}); rec.Code != http.StatusForbidden {
		t.Errorf("COPY into fase 1 = %d, esperado 403", rec.Code)
	}
}

func TestGallery_Phase2UploadAndDelete(t *testing.T) {
	t.Setenv("GALLERY_ENABLED", "true")
	t.Setenv("GALLERY_LAYOUT", "year-month")
	t.Setenv("GALLERY_UPLOAD", "true")
	t.Setenv("GALLERY_DELETE", "true")
	srv, ms, cloud := newFakeGalleryServer(t, false)
	seedGallerySampleData(t, ms, cloud)

	// 1. Validaciones PUT: tipo inválido / vacío / directorio virtual
	if rec := doDAV(t, srv, "PUT", "/_gallery/2026/11/texto.txt", []byte("hola"), ""); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("PUT .txt = %d, esperado 415", rec.Code)
	}
	if rec := doDAV(t, srv, "PUT", "/_gallery/2026/11/vacia.jpg", []byte{}, ""); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("PUT 0B = %d, esperado 415", rec.Code)
	}
	if rec := doDAV(t, srv, "PUT", "/_gallery/2026/11", []byte("abc"), ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT dir = %d, esperado 405", rec.Code)
	}

	// 2. Subida válida a /_gallery/2026/11/nueva.jpg -> save-metadata sin folderid, upsert con folder_id=0
	jpgPayload := []byte("valid-jpeg-content-12345")
	recPut := doDAV(t, srv, "PUT", "/_gallery/2026/11/nueva.jpg", jpgPayload, "", [2]string{"X-OC-Mtime", "1795450000"})
	if recPut.Code != http.StatusCreated && recPut.Code != http.StatusNoContent {
		t.Fatalf("PUT nueva.jpg = %d: %s", recPut.Code, recPut.Body.String())
	}

	cloud.mu.Lock()
	if len(cloud.metaPayloads) != 1 {
		cloud.mu.Unlock()
		t.Fatalf("metaPayloads=%d, esperado 1", len(cloud.metaPayloads))
	}
	if _, hasFolder := cloud.metaPayloads[0]["folderid"]; hasFolder {
		cloud.mu.Unlock()
		t.Fatalf("PUT en galería no debe enviar folderid: %v", cloud.metaPayloads[0])
	}
	cloud.mu.Unlock()

	recProp := doDAV(t, srv, "PROPFIND", "/_gallery/2026/11", nil, "1")
	if recProp.Code != http.StatusMultiStatus || !strings.Contains(recProp.Body.String(), "nueva.jpg") {
		t.Errorf("nueva.jpg debe aparecer tras PUT: %d %s", recProp.Code, recProp.Body.String())
	}
	recGet := doDAV(t, srv, "GET", "/_gallery/2026/11/nueva.jpg", nil, "")
	if recGet.Code != http.StatusOK || !bytes.Equal(recGet.Body.Bytes(), jpgPayload) {
		t.Errorf("GET nueva.jpg = %d %q", recGet.Code, recGet.Body.String())
	}

	// 2b. Subida de JPEG con EXIF SIN X-OC-Mtime -> conserva DateTimeOriginal en creationdate y modificationdate
	exifJPG := buildTestJPEGWithEXIF(t, "2019:07:14 18:30:05", "")
	recExif := doDAV(t, srv, "PUT", "/_gallery/con_exif.jpg", exifJPG, "")
	if recExif.Code != http.StatusCreated && recExif.Code != http.StatusNoContent {
		t.Fatalf("PUT con_exif.jpg = %d: %s", recExif.Code, recExif.Body.String())
	}
	cloud.mu.Lock()
	lastMeta := cloud.metaPayloads[len(cloud.metaPayloads)-1]
	cloud.mu.Unlock()
	if lastMeta["creationdate"] != "20190714T183005Z" || lastMeta["modificationdate"] != "20190714T183005Z" {
		t.Errorf("esperaba fecha EXIF 20190714T183005Z, recibido creationdate=%v modificationdate=%v",
			lastMeta["creationdate"], lastMeta["modificationdate"])
	}
	recExifGet := doDAV(t, srv, "GET", "/_gallery/2019/07/con_exif.jpg", nil, "")
	if recExifGet.Code != http.StatusOK || !bytes.Equal(recExifGet.Body.Bytes(), exifJPG) {
		t.Errorf("GET /_gallery/2019/07/con_exif.jpg = %d (esperaba 200 con la foto en su año/mes EXIF)", recExifGet.Code)
	}

	// 3. Subida directa en raíz /_gallery/directa.jpg en modo year-month -> accesible inmediatamente
	directPayload := []byte("direct-jpeg-content")
	recDirect := doDAV(t, srv, "PUT", "/_gallery/directa.jpg", directPayload, "")
	if recDirect.Code != http.StatusCreated && recDirect.Code != http.StatusNoContent {
		t.Fatalf("PUT /_gallery/directa.jpg = %d: %s", recDirect.Code, recDirect.Body.String())
	}
	recGetDirect := doDAV(t, srv, "GET", "/_gallery/directa.jpg", nil, "")
	if recGetDirect.Code != http.StatusOK || !bytes.Equal(recGetDirect.Body.Bytes(), directPayload) {
		t.Errorf("GET /_gallery/directa.jpg = %d %q", recGetDirect.Code, recGetDirect.Body.String())
	}

	// 4. DELETE en directorios virtuales y MKCOL/MOVE siguen dando 403
	for _, p := range []string{"/_gallery", "/_gallery/2026", "/_gallery/2026/11", "/_gallery/unknown"} {
		if rec := doDAV(t, srv, "DELETE", p, nil, ""); rec.Code != http.StatusForbidden {
			t.Errorf("DELETE %s = %d, esperado 403", p, rec.Code)
		}
	}
	if rec := doDAV(t, srv, "MKCOL", "/_gallery/otra", nil, ""); rec.Code != http.StatusForbidden {
		t.Errorf("MKCOL fase 2 = %d, esperado 403", rec.Code)
	}
	if rec := doDAV(t, srv, "MOVE", "/_gallery/2026/11/foto.jpg", nil, "", [2]string{"Destination", "http://example.com/Documentos/foto.jpg"}); rec.Code != http.StatusForbidden {
		t.Errorf("MOVE fase 2 = %d, esperado 403", rec.Code)
	}

	// 5. DELETE sobre duplicado desambiguado borra solo el ID 200 y conserva ID 100
	recDel := doDAV(t, srv, "DELETE", "/_gallery/2026/11/foto%20(200).jpg", nil, "")
	if recDel.Code != http.StatusNoContent && recDel.Code != http.StatusOK {
		t.Fatalf("DELETE foto (200).jpg = %d: %s", recDel.Code, recDel.Body.String())
	}
	if m200, _ := ms.GetMediaByID("200"); m200 != nil {
		t.Errorf("ID 200 debía haberse borrado de SQLite: %+v", m200)
	}
	if m100, _ := ms.GetMediaByID("100"); m100 == nil {
		t.Errorf("ID 100 debía seguir intacto en SQLite")
	}
	cloud.mu.Lock()
	if len(cloud.deletedCalls) != 1 || !strings.Contains(cloud.deletedCalls[0], "200") {
		t.Errorf("llamadas de borrado SAPI inesperadas: %v", cloud.deletedCalls)
	}
	cloud.mu.Unlock()

	// Verifica también que DeleteFile pasa el nombre real de la nube cuando usa endpoint por tipo
	t.Run("DeleteFile_BrowserProfile", func(t *testing.T) {
		var deletedPath string
		t.Setenv("SAPI_CLIENT_PROFILE", "browser")
		srvBrowser, msBrowser, cloudBrowser := newFakeGalleryServer(t, false)
		t.Setenv("SAPI_CLIENT_PROFILE", "browser")
		t.Setenv("GALLERY_ENABLED", "true")
		t.Setenv("GALLERY_LAYOUT", "year-month")
		t.Setenv("GALLERY_DELETE", "true")
		seedGallerySampleData(t, msBrowser, cloudBrowser)
		recDelBrowser := doDAV(t, srvBrowser, "DELETE", "/_gallery/2026/11/foto%20(200).jpg", nil, "")
		if recDelBrowser.Code != http.StatusNoContent && recDelBrowser.Code != http.StatusOK {
			t.Fatalf("DELETE browser = %d", recDelBrowser.Code)
		}
		cloudBrowser.mu.Lock()
		if len(cloudBrowser.deletedCalls) > 0 {
			deletedPath = cloudBrowser.deletedCalls[0]
		}
		cloudBrowser.mu.Unlock()
		if !strings.Contains(deletedPath, "/sapi/media/picture?action=delete") {
			t.Errorf("DELETE browser esperaba /sapi/media/picture?action=delete, got %q", deletedPath)
		}
	})

	// 6. Captura PNG sin EXIF ni X-OC-Mtime -> deduce fecha del nombre y coloca en /YYYY/MM
	pngBytes := []byte("\x89PNG\r\n\x1a\nscreenshot-payload")
	recPNG := doDAV(t, srv, "PUT", "/_gallery/Captura%20de%20pantalla%202022-03-10%20091530.png", pngBytes, "")
	if recPNG.Code != http.StatusCreated && recPNG.Code != http.StatusNoContent {
		t.Fatalf("PUT PNG sin EXIF = %d: %s", recPNG.Code, recPNG.Body.String())
	}
	cloud.mu.Lock()
	pngMeta := cloud.metaPayloads[len(cloud.metaPayloads)-1]
	cloud.mu.Unlock()
	if pngMeta["creationdate"] != "20220310T091530Z" {
		t.Errorf("PNG sin EXIF esperaba fecha del nombre 20220310T091530Z, got %v", pngMeta["creationdate"])
	}
	recGetPNG := doDAV(t, srv, "GET", "/_gallery/2022/03/Captura%20de%20pantalla%202022-03-10%20091530.png", nil, "")
	if recGetPNG.Code != http.StatusOK || !bytes.Equal(recGetPNG.Body.Bytes(), pngBytes) {
		t.Errorf("GET PNG por fecha de nombre = %d", recGetPNG.Code)
	}

	// 7. Sobrescritura desde raíz /_gallery/PRUEBA.png y sobre item ya indexado (/enero.mp4) sin duplicados
	v1 := []byte("prueba-version-1")
	v2 := []byte("prueba-version-2-actualizada")
	if r1 := doDAV(t, srv, "PUT", "/_gallery/PRUEBA.png", v1, ""); r1.Code != http.StatusCreated && r1.Code != http.StatusNoContent {
		t.Fatalf("PUT PRUEBA v1 = %d", r1.Code)
	}
	if r2 := doDAV(t, srv, "PUT", "/_gallery/PRUEBA.png", v2, ""); r2.Code != http.StatusCreated && r2.Code != http.StatusNoContent {
		t.Fatalf("PUT PRUEBA v2 = %d", r2.Code)
	}
	nowDir := fmt.Sprintf("/_gallery/%04d/%02d", time.Now().UTC().Year(), int(time.Now().UTC().Month()))
	recNow := doDAV(t, srv, "PROPFIND", nowDir, nil, "1")
	if recNow.Code != http.StatusMultiStatus || strings.Contains(recNow.Body.String(), "PRUEBA (") {
		t.Errorf("sobrescritura de PRUEBA.png en raíz no debía dejar duplicados en %s: %s", nowDir, recNow.Body.String())
	}
	recEneroOW := doDAV(t, srv, "PUT", "/_gallery/enero.mp4", []byte("enero-reemplazado"), "")
	if recEneroOW.Code != http.StatusCreated && recEneroOW.Code != http.StatusNoContent {
		t.Fatalf("PUT overwrite /_gallery/enero.mp4 = %d", recEneroOW.Code)
	}
	cloud.mu.Lock()
	eneroMeta := cloud.metaPayloads[len(cloud.metaPayloads)-1]
	cloud.mu.Unlock()
	if eneroMeta["id"] != "300" {
		t.Errorf("sobrescritura de enero.mp4 desde raíz debía reutilizar ID 300, got %v", eneroMeta["id"])
	}

	// 8. Borrar el único elemento de /_gallery/2022/03 poda las carpetas virtuales vacías (03 y 2022)
	recDelPNG := doDAV(t, srv, "DELETE", "/_gallery/2022/03/Captura%20de%20pantalla%202022-03-10%20091530.png", nil, "")
	if recDelPNG.Code != http.StatusNoContent && recDelPNG.Code != http.StatusOK {
		t.Fatalf("DELETE PNG = %d", recDelPNG.Code)
	}
	recGalRoot := doDAV(t, srv, "PROPFIND", "/_gallery", nil, "1")
	if strings.Contains(recGalRoot.Body.String(), "/_gallery/2022") {
		t.Errorf("carpeta vacía /_gallery/2022 debía podarse tras borrar su único fichero: %s", recGalRoot.Body.String())
	}
	if recYear := doDAV(t, srv, "PROPFIND", "/_gallery/2022", nil, "0"); recYear.Code != http.StatusNotFound {
		t.Errorf("PROPFIND /_gallery/2022 tras borrado = %d, esperado 404", recYear.Code)
	}
}

func TestBrowserUI_PermissionsAndBreadcrumbs(t *testing.T) {
	t.Setenv("GALLERY_ENABLED", "true")
	t.Setenv("GALLERY_LAYOUT", "year-month")
	t.Setenv("GALLERY_UPLOAD", "false")
	t.Setenv("GALLERY_DELETE", "false")
	srv, ms, cloud := newFakeGalleryServer(t, false)
	seedGallerySampleData(t, ms, cloud)

	// 1. Raíz / con galería activa (fase lectura): CSP con nonce, cero <img>, permisos diferenciados
	recRoot := doDAV(t, srv, "GET", "/", nil, "")
	if recRoot.Code != http.StatusOK || !strings.Contains(recRoot.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d, ct=%q", recRoot.Code, recRoot.Header().Get("Content-Type"))
	}
	csp := recRoot.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'nonce-") || !strings.Contains(csp, "font-src 'self'") || strings.Contains(csp, "img-src") {
		t.Errorf("CSP inesperado: %q", csp)
	}
	rootBody := recRoot.Body.String()
	if strings.Contains(strings.ToLower(rootBody), "<img") {
		t.Errorf("el listado HTML no debe incluir etiquetas <img> ni miniaturas")
	}
	if !strings.Contains(rootBody, `id="bootLoader"`) || !strings.Contains(rootBody, `id="navProgress"`) {
		t.Errorf("el HTML debe incluir el loader inicial y la barra de navegación: %s", rootBody)
	}
	if !strings.Contains(rootBody, `data-can-upload="true"`) || !strings.Contains(rootBody, `data-can-mkcol="true"`) {
		t.Errorf("raíz debe habilitar subida y nueva carpeta: %s", rootBody)
	}
	if !strings.Contains(rootBody, `data-href="/Documentos" data-dir="true" data-can-rename="true" data-can-delete="true"`) {
		t.Errorf("carpeta real Documentos debe permitir renombrar y borrar: %s", rootBody)
	}
	if !strings.Contains(rootBody, `data-href="/_gallery" data-dir="true" data-can-rename="false" data-can-delete="false"`) {
		t.Errorf("carpeta virtual _gallery en / no debe permitir renombrar ni borrar: %s", rootBody)
	}

	// 2. /_gallery/2026/11 en Fase 1 (solo lectura): breadcrumbs completos, sin upload/delete
	recMonthRO := doDAV(t, srv, "GET", "/_gallery/2026/11", nil, "")
	if recMonthRO.Code != http.StatusOK {
		t.Fatalf("GET /_gallery/2026/11 RO = %d", recMonthRO.Code)
	}
	roBody := recMonthRO.Body.String()
	for _, want := range []string{`href="/"`, `href="/_gallery"`, `href="/_gallery/2026"`, `data-can-upload="false"`, `data-can-mkcol="false"`} {
		if !strings.Contains(roBody, want) {
			t.Errorf("HTML RO sin %q", want)
		}
	}
	if !strings.Contains(roBody, `data-href="/_gallery/2026/11/foto.jpg" data-dir="false" data-can-rename="false" data-can-delete="false"`) {
		t.Errorf("foto.jpg en RO debe bloquear rename y delete: %s", roBody)
	}

	// 3. /_gallery/2026/11, /_gallery/2026 y /_gallery en Fase 2
	t.Setenv("GALLERY_UPLOAD", "true")
	t.Setenv("GALLERY_DELETE", "true")

	recMonthRW := doDAV(t, srv, "GET", "/_gallery/2026/11", nil, "")
	if recMonthRW.Code != http.StatusOK {
		t.Fatalf("GET /_gallery/2026/11 RW = %d", recMonthRW.Code)
	}
	rwBody := recMonthRW.Body.String()
	if !strings.Contains(rwBody, `data-upload-dir="/_gallery/2026/11"`) || !strings.Contains(rwBody, `data-can-upload="true"`) {
		t.Errorf("subida en /_gallery/2026/11 debe estar activa: %s", rwBody)
	}
	if !strings.Contains(rwBody, `data-href="/_gallery/2026/11/foto.jpg" data-dir="false" data-can-rename="false" data-can-delete="true"`) {
		t.Errorf("foto.jpg en Fase 2 debe permitir delete y bloquear rename: %s", rwBody)
	}

	recYearRW := doDAV(t, srv, "GET", "/_gallery/2026", nil, "")
	if recYearRW.Code != http.StatusOK {
		t.Fatalf("GET /_gallery/2026 RW = %d", recYearRW.Code)
	}
	if !strings.Contains(recYearRW.Body.String(), `data-upload-dir="/_gallery"`) {
		t.Errorf("subida dentro de /_gallery/2026 debe apuntar a /_gallery para clasificación EXIF")
	}

	recGalRW := doDAV(t, srv, "GET", "/_gallery", nil, "")
	if recGalRW.Code != http.StatusOK {
		t.Fatalf("GET /_gallery RW = %d", recGalRW.Code)
	}
	if !strings.Contains(recGalRW.Body.String(), `data-href="/_gallery/2026" data-dir="true" data-can-rename="false" data-can-delete="false"`) {
		t.Errorf("subcarpeta virtual /_gallery/2026 nunca debe permitir rename ni delete: %s", recGalRW.Body.String())
	}
}
