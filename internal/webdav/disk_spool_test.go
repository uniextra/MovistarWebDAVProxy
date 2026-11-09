package webdav

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// Backend controlado: conserva los bytes recibidos y permite releerlos sin caché.
func storedBackend(t *testing.T, fail bool) (*sapi.Client, *atomic.Int32) {
	t.Helper()
	base := newMockSAPIServer(t)
	t.Cleanup(base.Close)
	var mu sync.Mutex
	var stored []byte
	var name string
	var uploads atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sapi/upload" {
			uploads.Add(1)
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Errorf("multipart: %v", err)
				http.Error(w, "invalid multipart", 400)
				return
			}
			defer r.MultipartForm.RemoveAll()
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Errorf("file: %v", err)
				http.Error(w, "missing file", 400)
				return
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if fail {
				_, _ = io.WriteString(w, `{"error":{"code":"MED-1000","message":"backend rejected upload"}}`)
				return
			}
			mu.Lock()
			stored, name = data, header.Filename
			mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"id":"777","url":"/stored"}}`)
			return
		}
		if r.URL.Path == "/sapi/media" && r.URL.Query().Get("action") == "get" {
			mu.Lock()
			defer mu.Unlock()
			media := []map[string]interface{}{}
			if name != "" {
				media = append(media, map[string]interface{}{"id": "777", "name": name, "size": len(stored), "folderid": 100, "url": "/stored"})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": media}})
			return
		}
		if r.URL.Path == "/stored" {
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(stored)
			return
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(backend.Close)
	client, err := sapi.NewClient(config.AccountConfig{Provider: "movistar", Phone: "612345678", AccessToken: "test-access", RefreshToken: "test-refresh", ValidationKey: "test-vk"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(backend.URL+"/sapi", backend.URL)
	return client, &uploads
}

func TestDiskSpool_NormalFileSurvivesFreshServer(t *testing.T) {
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	t.Setenv("WEBDAV_TMPDIR", t.TempDir())
	client, uploads := storedBackend(t, false)
	payload := bytes.Repeat([]byte("backup"), 200000)
	srv := NewServer(client)
	put := httptest.NewRecorder()
	srv.ServeHTTP(put, httptest.NewRequest("PUT", "/backup.bin", bytes.NewReader(payload)))
	if put.Code != http.StatusCreated || uploads.Load() != 1 {
		t.Fatalf("PUT: status=%d uploads=%d", put.Code, uploads.Load())
	}
	// Instancia nueva, no InvalidateCache: también hay que descartar uploadCache.
	fresh := NewServer(client)
	get := httptest.NewRecorder()
	fresh.ServeHTTP(get, httptest.NewRequest("GET", "/backup.bin", nil))
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), payload) {
		t.Fatalf("GET sin caché: status=%d bytes=%d, esperados %d", get.Code, get.Body.Len(), len(payload))
	}
	entries, err := os.ReadDir(getUploadTempDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("spool sin limpiar: %v, %v", entries, err)
	}
}

func TestDiskSpool_UsesConfiguredDirectoryAndCleansAbort(t *testing.T) {
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	dir := t.TempDir()
	t.Setenv("WEBDAV_TMPDIR", dir)
	writer := NewWebDAVFileWriter(nil, "/backup.bin", "backup.bin", 100, "")
	writer.guard = &uploadGuard{expected: -1}
	payload := bytes.Repeat([]byte("x"), 1024*1024+1)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if writer.tmpFile == nil || filepath.Dir(writer.tmpFile.Name()) != dir {
		t.Fatal("el spool no está en WEBDAV_TMPDIR")
	}
	info, err := writer.tmpFile.Stat()
	if err != nil || info.Size() != int64(len(payload)) {
		t.Fatalf("spool completo: info=%v err=%v", info, err)
	}
	writer.guard.broken.Store(true)
	if err := writer.Close(); err != ErrUploadIncomplete {
		t.Fatalf("abort: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spool abandonado: %v, %v", entries, err)
	}
}

func TestDiskSpool_InvalidDirectoryDoesNotSucceed(t *testing.T) {
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	t.Setenv("WEBDAV_TMPDIR", filepath.Join(t.TempDir(), "missing"))
	client, uploads := storedBackend(t, false)
	rec := httptest.NewRecorder()
	NewServer(client).ServeHTTP(rec, httptest.NewRequest("PUT", "/backup.bin", strings.NewReader(strings.Repeat("x", 1024*1024+1))))
	if rec.Code < 400 || uploads.Load() != 0 {
		t.Fatalf("fallo de spool no debe tener éxito: status=%d uploads=%d", rec.Code, uploads.Load())
	}
}

func TestPersistence_BackendFailureCannotPassProbe(t *testing.T) {
	for _, name := range []string{"backup.bin", "backup.tmp", "duplicati-access-privileges-test.tmp"} {
		t.Run(name, func(t *testing.T) {
			client, _ := storedBackend(t, true)
			srv := NewServer(client)
			put := httptest.NewRecorder()
			srv.ServeHTTP(put, httptest.NewRequest("PUT", "/"+name, strings.NewReader("backup data")))
			if put.Code < 400 {
				t.Fatalf("backend rechaza datos, PUT no puede pasar: %d", put.Code)
			}
			for _, server := range []*Server{srv, NewServer(client)} {
				get := httptest.NewRecorder()
				server.ServeHTTP(get, httptest.NewRequest("GET", "/"+name, nil))
				if get.Code != http.StatusNotFound {
					t.Fatalf("datos no persistidos visibles: %d", get.Code)
				}
			}
		})
	}
}
