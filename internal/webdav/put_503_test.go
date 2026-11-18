package webdav

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// Si O2 no acepta una subida cuyo cuerpo llegó entero (sesión caducada, archivo en
// validación MED-1017, corte…), el PUT no debe acabar en 405: Hyper Backup lo
// muestra como «No permission to access the backup destination» (issues #60 y del
// MED-1017). Se responde 503 con Retry-After para que lo reintente.
func TestPut_O2RejectsUpload_Returns503NotMethodNotAllowed(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	var discarded atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}}},
			})
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save-metadata":
			_, _ = w.Write([]byte(`{"success":"Media metadata saved successfully","id":"1597568700"}`))
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"error":{"code":"MED-1099","message":"rechazado"}}`))
		case r.URL.Path == "/sapi/media" && action == "softdelete":
			discarded.Add(1)
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

	req := httptest.NewRequest("PUT", "/_Syno_TaskConfig", bytes.NewReader([]byte("config")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT rechazado por O2 = %d, se esperaba 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("falta Retry-After")
	}
	if discarded.Load() != 1 {
		t.Errorf("el elemento a medias debe retirarse (retirados: %d)", discarded.Load())
	}
}
