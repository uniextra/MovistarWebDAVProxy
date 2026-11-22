package webdav

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// streamCloud simula SAPI: la subida devuelve uploadStatus y guarda lo recibido.
func streamCloud(t *testing.T, uploadStatus int) (*Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var stored []byte
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}}}})
		case r.URL.Path == "/sapi/upload" && action == "save":
			if uploadStatus != http.StatusOK {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(uploadStatus)
				_, _ = io.WriteString(w, "<html>408 Request Timeout</html>")
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			if f, _, err := r.FormFile("file"); err == nil {
				b, _ := io.ReadAll(f)
				mu.Lock()
				stored = b
				mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"id":"777","name":"trozo.bin"}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	t.Cleanup(mock.Close)
	client, err := sapi.NewClient(config.AccountConfig{Provider: "movistar", Phone: "612345678", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	return NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true}), func() []byte { mu.Lock(); defer mu.Unlock(); return stored }
}

// Con WEBDAV_UPLOAD_SPOOL=false la subida va solo en flujo: no crea temporal (el
// directorio de temporales ni siquiera es escribible) y llega entera a la nube.
func TestStreamOnly_NoTempFile(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // subida en flujo del modo web (sin el envío de la app oficial)
	tmp := t.TempDir()
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })
	t.Setenv("WEBDAV_UPLOAD_SPOOL", "false")
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	t.Setenv("WEBDAV_TMPDIR", tmp)

	srv, stored := streamCloud(t, http.StatusOK)
	payload := make([]byte, 3<<20)
	_, _ = rand.Read(payload)
	req := httptest.NewRequest("PUT", "/trozo.bin", bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(stored(), payload) {
		t.Fatalf("la nube recibió %d bytes, esperados %d", len(stored()), len(payload))
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("no debía crear temporales: %d", len(entries))
	}
}

// Si la subida en flujo falla (p. ej. 408 del servidor de subida) y no hay copia
// local, el PUT responde 503 con Retry-After para que el cliente la repita.
func TestStreamOnly_FailureIs503(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // subida en flujo del modo web (sin el envío de la app oficial)
	t.Setenv("WEBDAV_UPLOAD_SPOOL", "false")
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	srv, _ := streamCloud(t, http.StatusRequestTimeout)
	payload := make([]byte, 3<<20)
	req := httptest.NewRequest("PUT", "/trozo.bin", bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("PUT = %d (Retry-After %q), esperado 503 con Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// Por defecto hay copia local en todos los modos (clientes como Hyper Backup la
// necesitan); WEBDAV_UPLOAD_SPOOL=false la quita.
func TestUploadSpool_DefaultOn(t *testing.T) {
	for _, profile := range []string{"browser", "desktop"} {
		t.Setenv("SAPI_CLIENT_PROFILE", profile)
		t.Setenv("WEBDAV_UPLOAD_SPOOL", "")
		if !uploadSpoolEnabled() {
			t.Fatalf("%s: por defecto debe haber copia local", profile)
		}
		t.Setenv("WEBDAV_UPLOAD_SPOOL", "false")
		if uploadSpoolEnabled() {
			t.Fatalf("%s: WEBDAV_UPLOAD_SPOOL=false debe quitarla", profile)
		}
	}
}

// Sin copia local, la subida a la nube no se abre hasta tener el búfer inicial
// (WEBDAV_BUFFER_MB) en memoria: con los primeros KB todavía no hay petición a SAPI.
func TestStreamOnly_WaitsForInitialBuffer(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // subida en flujo del modo web (sin el envío de la app oficial)
	t.Setenv("WEBDAV_UPLOAD_SPOOL", "false")
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	var uploads int32
	var mu sync.Mutex
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sapi/upload" {
			mu.Lock()
			uploads++
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"id":"777","name":"trozo.bin"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"folders":[{"id":100,"name":"Root","parentid":0,"magic":true}]}}`)
	}))
	t.Cleanup(mock.Close)
	client, err := sapi.NewClient(config.AccountConfig{Provider: "movistar", Phone: "612345678", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	mfs := NewMovistarFS(client)
	w := NewWebDAVFileWriter(mfs, "/trozo.bin", "trozo.bin", 100, "")
	w.guard = &uploadGuard{expected: 3 << 20}

	if _, err := w.Write(make([]byte, 32<<10)); err != nil {
		t.Fatal(err)
	}
	if w.streamOnly || w.streamDone != nil {
		t.Fatal("con 32 KB no debe abrirse todavía la subida a la nube")
	}
	if _, err := w.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if !w.streamOnly || w.streamDone == nil {
		t.Fatal("al pasar del búfer inicial debe empezar la subida en flujo")
	}
	if _, err := w.Write(make([]byte, 3<<20-(32<<10)-(1<<20))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if uploads != 1 {
		t.Errorf("subidas a SAPI = %d, esperada 1", uploads)
	}
}
