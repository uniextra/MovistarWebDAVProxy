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
	// el cuerpo dice qué pasó (rclone lo muestra), no «Method Not Allowed» del 405 original
	body := rec.Body.String()
	if strings.Contains(body, "Method Not Allowed") || !strings.Contains(body, "MED-1099") || !strings.Contains(body, "reinténtalo") {
		t.Errorf("cuerpo del 503 = %q", body)
	}
	if strings.Contains(body, "vk") {
		t.Errorf("el cuerpo no debe llevar datos de sesión: %q", body)
	}
	if discarded.Load() != 1 {
		t.Errorf("el elemento a medias debe retirarse (retirados: %d)", discarded.Load())
	}
}

// Una imagen cuyo contenido no lo es (MED-1050) es un rechazo definitivo, como para la
// app oficial: 415 sin Retry-After, un solo envío (ni reintento desde disco ni
// reanudación) y el elemento creado se retira.
func TestPut_O2RejectsMediaContent_Returns415WithoutRetries(t *testing.T) {
	t.Run("disco", func(t *testing.T) { putRejectedMedia(t, 300000) })
	t.Run("flujo", func(t *testing.T) {
		t.Setenv("WEBDAV_STREAM_UPLOAD", "1")
		t.Setenv("WEBDAV_BUFFER_MB", "1")
		t.Setenv("WEBDAV_TMPDIR", t.TempDir())
		putRejectedMedia(t, 3<<20)
	})
}

func putRejectedMedia(t *testing.T, size int) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	var discarded, sends atomic.Int32
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
			sends.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"error":{"code":"MED-1050","message":"The file extension or the format is not supported."}}`))
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

	req := httptest.NewRequest("PUT", "/falsa.jpg", bytes.NewReader(bytes.Repeat([]byte("x"), size)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("PUT de una imagen que O2 no admite = %d, se esperaba 415", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Error("un rechazo definitivo no lleva Retry-After")
	}
	if body := rec.Body.String(); !strings.Contains(body, "MED-1050") || strings.Contains(body, "Method Not Allowed") {
		t.Errorf("cuerpo del 415 = %q", body)
	}
	if sends.Load() != 1 {
		t.Errorf("envíos del contenido = %d, se esperaba 1", sends.Load())
	}
	if discarded.Load() != 1 {
		t.Errorf("el elemento creado debe retirarse (retirados: %d)", discarded.Load())
	}
}
