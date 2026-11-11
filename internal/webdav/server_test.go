package webdav

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func newMockSAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		action := r.URL.Query().Get("action")

		switch {
		case path == "/sapi/media" && action == "get-storage-space":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"used":    104857600, // 100 MB
					"quota":   0,
					"nolimit": true,
				},
			})
		case path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{
						{"id": 100, "name": "Root", "parentid": 0, "magic": true},
					},
				},
			})
		case path == "/sapi/media/folder" && action == "list":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{
						{"id": 200, "name": "Backups", "parentid": 100, "creationdate": "20260101T000000Z", "modificationdate": "20260101T000000Z"},
					},
				},
			})
		case path == "/sapi/media" && action == "get":
			// GetMediaByID llega aquí con un cuerpo {data:{ids:[...]}}. Se devuelve
			// cada id solicitado (modela también el destino recién re-subido, id 777),
			// con el mismo tamaño y contenido que sirve /sapi/media/content ("hello world").
			var q struct {
				Data struct {
					IDs []string `json:"ids"`
				} `json:"data"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &q)
			if len(q.Data.IDs) > 0 {
				media := make([]map[string]interface{}, 0, len(q.Data.IDs))
				for _, id := range q.Data.IDs {
					name := "hello.txt"
					if id == "777" {
						name = "hello.md"
					}
					media = append(media, map[string]interface{}{
						"id": id, "name": name, "size": 11, "folderid": 100,
						"url":              "/sapi/media/content?id=" + id,
						"creationdate":     "20260101T000000Z",
						"modificationdate": "20260101T000000Z",
					})
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{"media": media},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"media": []map[string]interface{}{
						{"id": "555", "name": "hello.txt", "size": 11, "folderid": 100, "url": "/sapi/media/content?id=555", "creationdate": "20260101T000000Z", "modificationdate": "20260101T000000Z"},
					},
				},
			})
		case path == "/sapi/media/content":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello world"))
		case path == "/sapi/upload" && action == "save":
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"id":   "777",
					"name": "uploaded.bin",
					"url":  "/sapi/media/content?id=777",
				},
			})
		case (strings.HasPrefix(path, "/sapi/media/") && action == "delete") || (path == "/sapi/media" && action == "softdelete"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case path == "/sapi/media/folder" && action == "softdelete":
			if r.URL.Query().Get("id") == "" {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{
						"code":    "COM-1011",
						"message": "Missing required parameter",
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": "Folders have been softdeleted"})
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
}

func setupTestWebDAVServer(t *testing.T, authCfg config.AuthConfig) (*Server, func()) {
	t.Helper()
	mockSAPI := newMockSAPIServer(t)
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
	srv := NewServerWithAuth(client, authCfg)
	return srv, mockSAPI.Close
}

func TestWebDAV_BasicAuthAndRateLimit(t *testing.T) {
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{
		Username: "davuser",
		Password: "davpass",
	})
	defer cleanup()

	// 1. Petición sin credenciales -> 401 con WWW-Authenticate
	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.RemoteAddr = "198.51.100.25:12345"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), `Basic realm="WebDAV"`) {
		t.Errorf("missing or invalid WWW-Authenticate header: %q", rec.Header().Get("WWW-Authenticate"))
	}

	// 2. OPTIONS sin credenciales se permite para descubrimiento de Windows WebClient
	optReq := httptest.NewRequest("OPTIONS", "/", nil)
	optReq.RemoteAddr = "198.51.100.25:12345"
	optRec := httptest.NewRecorder()
	srv.ServeHTTP(optRec, optReq)
	if optRec.Code != http.StatusOK {
		t.Errorf("expected OPTIONS without auth to return 200, got %d", optRec.Code)
	}
	if optRec.Header().Get("MS-Author-Via") != "DAV" {
		t.Errorf("expected MS-Author-Via: DAV header")
	}

	// 3. Petición con credenciales válidas (Depth: 1 -> ejercita Readdir con caché fría) -> 207 Multi-Status
	authReq := httptest.NewRequest("PROPFIND", "/", nil)
	authReq.Header.Set("Depth", "1")
	authReq.SetBasicAuth("DESKTOP-PC\\davuser", "davpass")
	authReq.RemoteAddr = "198.51.100.25:12345"
	authRec := httptest.NewRecorder()
	srv.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusMultiStatus {
		t.Fatalf("expected 207 Multi-Status with valid domain\\user auth, got %d", authRec.Code)
	}
	if !strings.Contains(authRec.Body.String(), "hello.txt") || !strings.Contains(authRec.Body.String(), "Backups") {
		t.Errorf("expected PROPFIND Depth:1 to list folder contents without deadlock, got:\n%s", authRec.Body.String())
	}

	// 4. Exceder rate-limit de fallos (10 intentos fallidos -> 429)
	attackerIP := "[2001:db8::99]:54321"
	for i := 0; i < 10; i++ {
		badReq := httptest.NewRequest("PROPFIND", "/", nil)
		badReq.SetBasicAuth("davuser", "wrongpass")
		badReq.RemoteAddr = attackerIP
		badRec := httptest.NewRecorder()
		srv.ServeHTTP(badRec, badReq)
		if badRec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i+1, badRec.Code)
		}
	}

	blockedReq := httptest.NewRequest("PROPFIND", "/", nil)
	blockedReq.SetBasicAuth("davuser", "davpass") // Incluso con clave correcta una vez bloqueado
	blockedReq.RemoteAddr = attackerIP
	blockedRec := httptest.NewRecorder()
	srv.ServeHTTP(blockedRec, blockedReq)
	if blockedRec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 Too Many Requests after 10 failures, got %d", blockedRec.Code)
	}
}

func TestWebDAV_RejectPUTOnCollectionAndQuotaRewrite(t *testing.T) {
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()

	// 1. PUT con barra final -> 405 Method Not Allowed
	reqSlash := httptest.NewRequest("PUT", "/Backups/", strings.NewReader("data"))
	recSlash := httptest.NewRecorder()
	srv.ServeHTTP(recSlash, reqSlash)
	if recSlash.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 on PUT /Backups/, got %d", recSlash.Code)
	}

	// 2. PUT sobre carpeta existente sin barra final -> 405 Method Not Allowed
	reqFolder := httptest.NewRequest("PUT", "/Backups", strings.NewReader("data"))
	recFolder := httptest.NewRecorder()
	srv.ServeHTTP(recFolder, reqFolder)
	if recFolder.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 on PUT /Backups (existing folder), got %d", recFolder.Code)
	}

	// 3. PROPFIND solicitando cuotas RFC 4331 -> debe inyectar quota-available-bytes y quota-used-bytes sin 404 vacío
	propfindBody := `<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:quota-available-bytes/>
    <D:quota-used-bytes/>
  </D:prop>
</D:propfind>`
	propReq := httptest.NewRequest("PROPFIND", "/", strings.NewReader(propfindBody))
	propReq.Header.Set("Depth", "0")
	propReq.Header.Set("Content-Type", "application/xml")
	propRec := httptest.NewRecorder()
	srv.ServeHTTP(propRec, propReq)

	if propRec.Code != http.StatusMultiStatus {
		t.Fatalf("expected 207 on PROPFIND, got %d", propRec.Code)
	}
	xmlOut := propRec.Body.String()
	if !strings.Contains(xmlOut, "quota-available-bytes>") || !strings.Contains(xmlOut, "<D:quota-used-bytes>104857600</D:quota-used-bytes>") {
		t.Errorf("expected RFC 4331 quota tags in PROPFIND response, got:\n%s", xmlOut)
	}
	if strings.Contains(xmlOut, "404 Not Found") {
		t.Errorf("expected empty 404 propstat to be stripped from PROPFIND response, got:\n%s", xmlOut)
	}
}

func TestWebDAV_UploadMaxBytesAndOperations(t *testing.T) {
	t.Setenv("MAX_UPLOAD_MB", "1") // 1 MB límite para el test
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()

	// 1. Subida dentro del límite (100 KB) -> 201 Created
	smallPayload := bytes.Repeat([]byte("A"), 100*1024)
	putReq := httptest.NewRequest("PUT", "/uploaded.bin", bytes.NewReader(smallPayload))
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusNoContent && putRec.Code != http.StatusOK {
		t.Fatalf("expected PUT success (201/204), got %d", putRec.Code)
	}

	// 2. Subida que supera 1 MB sin Content-Length conocido (streaming chunked) -> 413 Request Entity Too Large
	largePayload := bytes.Repeat([]byte("B"), 1024*1024+512)
	streamReq := httptest.NewRequest("PUT", "/too_big_stream.bin", io.NopCloser(bytes.NewReader(largePayload)))
	streamReq.ContentLength = -1
	streamRec := httptest.NewRecorder()
	srv.ServeHTTP(streamRec, streamReq)
	if streamRec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 Request Entity Too Large on streaming oversize PUT, got %d", streamRec.Code)
	}

	// 3. Descarga GET de archivo existente -> 200 OK con contenido "hello world"
	getReq := httptest.NewRequest("GET", "/hello.txt", nil)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK || getRec.Body.String() != "hello world" {
		t.Errorf("unexpected GET response: code=%d body=%q", getRec.Code, getRec.Body.String())
	}

	// 4. DELETE de archivo existente -> 204 No Content
	delReq := httptest.NewRequest("DELETE", "/hello.txt", nil)
	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent && delRec.Code != http.StatusOK {
		t.Errorf("expected DELETE 204/200, got %d", delRec.Code)
	}

	// 5. DELETE de carpeta existente -> 204 No Content
	delFolderReq := httptest.NewRequest("DELETE", "/Backups", nil)
	delFolderRec := httptest.NewRecorder()
	srv.ServeHTTP(delFolderRec, delFolderReq)
	if delFolderRec.Code != http.StatusNoContent && delFolderRec.Code != http.StatusOK {
		t.Errorf("expected DELETE folder 204/200, got %d (body: %s)", delFolderRec.Code, delFolderRec.Body.String())
	}
}

func TestWebDAV_AllMethodsAuthAndWindowsCompatibility(t *testing.T) {
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{
		Username: "davuser",
		Password: "dav:pass:with:colons",
	})
	defer cleanup()

	// 1. Todos los métodos WebDAV sin credenciales deben devolver 401 Unauthorized y WWW-Authenticate: Basic realm="WebDAV"
	protectedMethods := []string{
		"PROPFIND", "GET", "HEAD", "PUT", "DELETE",
		"MKCOL", "MOVE", "COPY", "LOCK", "UNLOCK",
	}

	for _, m := range protectedMethods {
		t.Run("Unauthenticated_"+m, func(t *testing.T) {
			req := httptest.NewRequest(m, "/hello.txt", nil)
			req.RemoteAddr = "192.168.1.100:12345"
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("method %s: expected 401, got %d", m, rec.Code)
			}
			authHeader := rec.Header().Get("WWW-Authenticate")
			if authHeader != `Basic realm="WebDAV"` {
				t.Errorf("method %s: expected WWW-Authenticate: Basic realm=\"WebDAV\", got %q", m, authHeader)
			}
		})
	}

	// 2. OPTIONS sin credenciales devuelve 200 OK para descubrimiento de Windows WebClient
	t.Run("OPTIONS_Discovery_NoAuth", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/", nil)
		req.RemoteAddr = "192.168.1.100:12345"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for OPTIONS discovery, got %d", rec.Code)
		}
		if rec.Header().Get("MS-Author-Via") != "DAV" {
			t.Errorf("expected MS-Author-Via: DAV")
		}
		if dav := rec.Header().Get("DAV"); !strings.Contains(dav, "1") {
			t.Errorf("expected DAV compliance header containing 1, got %q", dav)
		}
	})

	// 3. OPTIONS con credenciales incorrectas debe devolver 401
	t.Run("OPTIONS_BadAuth", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/", nil)
		req.SetBasicAuth("davuser", "badpass")
		req.RemoteAddr = "192.168.1.100:12345"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for OPTIONS with bad auth, got %d", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") != `Basic realm="WebDAV"` {
			t.Errorf("expected WWW-Authenticate: Basic realm=\"WebDAV\", got %q", rec.Header().Get("WWW-Authenticate"))
		}
	})

	// 4. Compatibilidad con Windows: normalización de DOMAIN\user, \user y contraseñas con dos puntos
	testUsers := []string{
		`CORP\davuser`,
		`\davuser`,
		`   davuser   `,
		`davuser`,
	}

	for _, u := range testUsers {
		t.Run("WindowsAuth_"+u, func(t *testing.T) {
			req := httptest.NewRequest("PROPFIND", "/", nil)
			req.Header.Set("Depth", "0")
			req.SetBasicAuth(u, "dav:pass:with:colons")
			req.RemoteAddr = "192.168.1.100:12345"
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusMultiStatus {
				t.Fatalf("user %q: expected 207 MultiStatus, got %d", u, rec.Code)
			}
		})
	}
}

func TestWebDAV_RenameExtensionChange_MoveByReupload(t *testing.T) {
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()

	// 1. MOVE con cambio de extensión (.txt -> .md) debe disparar moveByReupload y responder 201 Created o 204 No Content
	moveReq := httptest.NewRequest("MOVE", "/hello.txt", nil)
	moveReq.Header.Set("Destination", "/hello.md")
	moveReq.Header.Set("Overwrite", "T")
	moveRec := httptest.NewRecorder()
	srv.ServeHTTP(moveRec, moveReq)

	if moveRec.Code != http.StatusCreated && moveRec.Code != http.StatusNoContent {
		t.Fatalf("expected 201 Created or 204 No Content on MOVE with extension change, got %d (body: %s)", moveRec.Code, moveRec.Body.String())
	}
}

func TestWebDAV_DuplicatiAccessPrivilegesProbe(t *testing.T) {
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()

	probePath := "/duplicati-access-privileges-test.tmp"
	payload := []byte("duplicati probe content 12345")

	// 1. PUT probe -> 201 Created
	putReq := httptest.NewRequest("PUT", probePath, bytes.NewReader(payload))
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusOK && putRec.Code != http.StatusNoContent {
		t.Fatalf("PUT probe: expected 201 Created, got %d", putRec.Code)
	}

	// 2. GET probe -> 200 OK with payload
	getReq := httptest.NewRequest("GET", probePath, nil)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET probe: expected 200 OK, got %d", getRec.Code)
	}
	if getRec.Body.String() != string(payload) {
		t.Fatalf("GET probe: payload mismatch: got %q, want %q", getRec.Body.String(), string(payload))
	}

	// 3. PROPFIND probe -> 207 Multi-Status
	propReq := httptest.NewRequest("PROPFIND", probePath, nil)
	propReq.Header.Set("Depth", "0")
	propRec := httptest.NewRecorder()
	srv.ServeHTTP(propRec, propReq)
	if propRec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND probe: expected 207 MultiStatus, got %d", propRec.Code)
	}

	// 4. DELETE probe -> 204 No Content
	delReq := httptest.NewRequest("DELETE", probePath, nil)
	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent && delRec.Code != http.StatusOK {
		t.Fatalf("DELETE probe: expected 204 No Content, got %d", delRec.Code)
	}

	// 5. GET probe after DELETE -> 404 Not Found
	getReq2 := httptest.NewRequest("GET", probePath, nil)
	getRec2 := httptest.NewRecorder()
	srv.ServeHTTP(getRec2, getReq2)
	if getRec2.Code != http.StatusNotFound {
		t.Fatalf("GET probe after delete: expected 404, got %d", getRec2.Code)
	}
}

func TestWebDAV_NTLM_Offer_Selective(t *testing.T) {
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{
		Username: "davuser",
		Password: "davpassword",
	})
	defer cleanup()

	// 1. Generic Linux/curl client -> should only get Basic, never NTLM (Issue #15 fix)
	linuxReq := httptest.NewRequest("PROPFIND", "/", nil)
	linuxReq.Header.Set("User-Agent", "gvfs/1.50.0")
	linuxReq.RemoteAddr = "192.168.1.100:12345"
	linuxRec := httptest.NewRecorder()
	srv.ServeHTTP(linuxRec, linuxReq)

	if linuxRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", linuxRec.Code)
	}
	authHeaders := linuxRec.Header().Values("WWW-Authenticate")
	for _, h := range authHeaders {
		if strings.Contains(strings.ToUpper(h), "NTLM") {
			t.Errorf("Linux/GVFS client must NOT be offered NTLM, got header: %s", h)
		}
	}
	if linuxRec.Header().Get("WWW-Authenticate") != `Basic realm="WebDAV"` {
		t.Errorf("expected Basic realm=\"WebDAV\", got %q", linuxRec.Header().Get("WWW-Authenticate"))
	}

	// 2. Windows native client (Microsoft-WebDAV-MiniRedir) -> should get both NTLM and Basic
	winReq := httptest.NewRequest("PROPFIND", "/", nil)
	winReq.Header.Set("User-Agent", "Microsoft-WebDAV-MiniRedir/10.0.19045")
	winReq.RemoteAddr = "192.168.1.100:12345"
	winRec := httptest.NewRecorder()
	srv.ServeHTTP(winRec, winReq)

	if winRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", winRec.Code)
	}
	winAuthHeaders := winRec.Header().Values("WWW-Authenticate")
	hasNTLM := false
	hasBasic := false
	for _, h := range winAuthHeaders {
		if strings.Contains(strings.ToUpper(h), "NTLM") {
			hasNTLM = true
		}
		if strings.Contains(h, "Basic") {
			hasBasic = true
		}
	}
	if !hasNTLM || !hasBasic {
		t.Errorf("Windows client must be offered both NTLM and Basic, got headers: %v", winAuthHeaders)
	}
}


func TestWebDAV_ProppatchDoesNotTruncateUploadedFile(t *testing.T) {
	srv, subidas, cerrar := servidorQueCuentaSubidas(t)
	defer cerrar()

	content := "contenido de video de prueba para verificar que PROPPATCH no lo trunca a 0 bytes"
	putReq := httptest.NewRequest("PUT", "/video_proppatch.mp4", strings.NewReader(content))
	putReq.ContentLength = int64(len(content))
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusNoContent {
		t.Fatalf("PUT failed: got %d", putRec.Code)
	}
	if n := subidas.Load(); n != 1 {
		t.Fatalf("expected 1 upload after PUT, got %d", n)
	}

	// Windows WebClient envía inmediatamente un PROPPATCH con propiedades Win32*
	proppatchBody := `<?xml version="1.0" encoding="utf-8" ?>
<D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:">
  <D:set>
    <D:prop>
      <Z:Win32CreationTime>Sat, 07 Nov 2026 08:10:00 GMT</Z:Win32CreationTime>
      <Z:Win32LastAccessTime>Sat, 07 Nov 2026 08:10:00 GMT</Z:Win32LastAccessTime>
      <Z:Win32LastModifiedTime>Sat, 07 Nov 2026 08:10:00 GMT</Z:Win32LastModifiedTime>
      <Z:Win32FileAttributes>00000020</Z:Win32FileAttributes>
    </D:prop>
  </D:set>
</D:propertyupdate>`
	patchReq := httptest.NewRequest("PROPPATCH", "/video_proppatch.mp4", strings.NewReader(proppatchBody))
	patchReq.Header.Set("Content-Type", "text/xml; charset=utf-8")
	patchRec := httptest.NewRecorder()
	srv.ServeHTTP(patchRec, patchReq)

	if patchRec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH expected 207 MultiStatus, got %d (body: %s)", patchRec.Code, patchRec.Body.String())
	}
	// PROPPATCH jamás debe provocar una segunda subida (de 0 bytes) a SAPI
	if n := subidas.Load(); n != 1 {
		t.Fatalf("PROPPATCH triggered an unexpected SAPI upload! total uploads = %d (want 1)", n)
	}

	// Verificar que el archivo conserva su tamaño y contenido íntegro tras PROPPATCH
	getReq := httptest.NewRequest("GET", "/video_proppatch.mp4", nil)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET after PROPPATCH failed: %d", getRec.Code)
	}
	if getRec.Body.String() != content {
		t.Fatalf("file content corrupted/truncated after PROPPATCH: got %d bytes (%q), want %d bytes (%q)",
			getRec.Body.Len(), getRec.Body.String(), len(content), content)
	}
}

func TestWebDAV_OrphanedLockAutoReleasedOnUnconditionalDelete(t *testing.T) {
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()

	// 1. Subir archivo inicial
	putReq := httptest.NewRequest("PUT", "/locked_video.mp4", strings.NewReader("datos"))
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusNoContent {
		t.Fatalf("initial PUT failed: %d", putRec.Code)
	}

	// 2. Adquirir LOCK exclusivo (como hace Windows WebClient antes de subir)
	lockBody := `<?xml version="1.0" encoding="utf-8" ?>
<D:lockinfo xmlns:D="DAV:">
  <D:lockscope><D:exclusive/></D:lockscope>
  <D:locktype><D:write/></D:locktype>
  <D:owner><D:href>PC\user</D:href></D:owner>
</D:lockinfo>`
	lockReq := httptest.NewRequest("LOCK", "/locked_video.mp4", strings.NewReader(lockBody))
	lockReq.Header.Set("Timeout", "Second-3600")
	lockRec := httptest.NewRecorder()
	srv.ServeHTTP(lockRec, lockReq)
	if lockRec.Code != http.StatusOK && lockRec.Code != http.StatusCreated {
		t.Fatalf("LOCK failed: %d", lockRec.Code)
	}

	// 3. Enviar DELETE sin cabecera If (simulando limpieza de Windows tras timeout o borrado manual)
	delReq := httptest.NewRequest("DELETE", "/locked_video.mp4", nil)
	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent && delRec.Code != http.StatusOK {
		t.Fatalf("unconditional DELETE on orphaned lock should auto-release and succeed, got %d", delRec.Code)
	}
}

func TestWebDAV_StreamUploadLargeFile(t *testing.T) {
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	srv, subidas, cerrar := servidorQueCuentaSubidas(t)
	defer cerrar()

	// Payload de 2 MB (> 1 MB de WEBDAV_BUFFER_MB) para ejercitar la subida en flujo + spool
	payload := strings.Repeat("ABCD", 512*1024)
	putReq := httptest.NewRequest("PUT", "/stream_2mb.bin", strings.NewReader(payload))
	putReq.ContentLength = int64(len(payload))
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)

	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusNoContent {
		t.Fatalf("stream PUT failed: got %d", putRec.Code)
	}
	if n := subidas.Load(); n != 1 {
		t.Fatalf("expected 1 upload to SAPI, got %d", n)
	}
}

func TestWebDAV_SmallFileDownloadCachedInMemory(t *testing.T) {
	var downloads int
	mockSAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{
						{"id": 100, "name": "Root", "parentid": 0, "magic": true},
					},
				},
			})
		case r.URL.Path == "/sapi/media/folder" && action == "list":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"folders": []map[string]interface{}{}},
			})
		case r.URL.Path == "/sapi/media" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"media": []map[string]interface{}{
						{"id": "888", "name": "config", "size": 17, "folderid": 100, "url": "/sapi/download/file?id=888", "creationdate": "20260101T000000Z", "modificationdate": "20260101T000000Z"},
					},
				},
			})
		case r.URL.Path == "/sapi/download/file":
			downloads++
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("restic-config-v2\n"))
		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	defer mockSAPI.Close()

	client, err := sapi.NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "test-vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(mockSAPI.URL+"/sapi", mockSAPI.URL)
	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/config", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "restic-config-v2\n" {
			t.Fatalf("GET #%d failed: code=%d body=%q", i+1, rec.Code, rec.Body.String())
		}
	}

	if downloads != 1 {
		t.Errorf("expected small file (/config) to be downloaded from SAPI only 1 time across 5 GETs, got %d", downloads)
	}
}
