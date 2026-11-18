package webdav

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// Subida grande (en flujo directo hacia O2) cuyo envío se corta a mitad: el proxy
// debe continuar desde el disco sobre el MISMO ID, preguntando a O2 cuánto tiene
// (308 + Range) y mandando solo el resto, como la app oficial. Nada de crear un
// segundo elemento ni de dejar uno a medias.
func TestPut_StreamCutContinuesFromDiskOnSameID(t *testing.T) {
	t.Setenv("WEBDAV_STREAM_UPLOAD", "1") // el flujo directo es opcional (interbloqueo)
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	t.Setenv("WEBDAV_TMPDIR", t.TempDir())

	payload := make([]byte, 3<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []byte
	metas, posts, queries, deletes := 0, 0, 0, 0
	var ranges []string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}}},
			})
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save-metadata":
			mu.Lock()
			metas++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"success":"Media metadata saved successfully","id":"1597720466"}`))
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save" && r.Method == http.MethodGet:
			mu.Lock()
			queries++
			if len(got) > 0 {
				w.Header().Set("Range", fmt.Sprintf("0-%d", len(got)-1))
			}
			mu.Unlock()
			w.WriteHeader(http.StatusPermanentRedirect)
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save":
			mu.Lock()
			posts++
			n := posts
			ranges = append(ranges, r.Header.Get("content-range"))
			if r.Header.Get("content-range") == "" {
				got = nil
			}
			mu.Unlock()
			if n == 1 {
				buf := make([]byte, 1<<20)
				k, _ := io.ReadFull(r.Body, buf)
				mu.Lock()
				got = append(got, buf[:k]...)
				mu.Unlock()
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			got = append(got, b...)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"success":"Media uploaded successfully","id":"1597720466","status":"V","etag":"x"}`))
		case action == "softdelete" || action == "delete":
			mu.Lock()
			deletes++
			mu.Unlock()
			_, _ = w.Write([]byte(`"Item successfully moved to trash"`))
		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	t.Cleanup(mock.Close)

	client, err := sapi.NewClient(config.AccountConfig{Provider: "o2", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})

	req := httptest.NewRequest("PUT", "/grande.bin", bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if metas != 1 {
		t.Errorf("save-metadata = %d; el reintento debe continuar sobre el mismo ID", metas)
	}
	if deletes != 0 {
		t.Errorf("se retiró el elemento (%d) aunque la subida terminó bien", deletes)
	}
	if queries < 1 {
		t.Errorf("no se preguntó a O2 cuánto tenía (308 + Range)")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("O2 tiene %d bytes distintos del original (%d)", len(got), len(payload))
	}
	want := fmt.Sprintf("bytes %d-%d/%d", 1<<20, len(payload)-1, len(payload))
	if len(ranges) != 2 || ranges[1] != want {
		t.Errorf("envíos = %q; la continuación debía ser %q", ranges, want)
	}
}
