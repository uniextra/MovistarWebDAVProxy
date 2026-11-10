package webdav

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func TestPropfindDoesNotReturnEmptySuccessWhenProviderFails(t *testing.T) {
	// Simular un backend SAPI que falla con error 503 o 403 al listar subcarpetas o media
	mockSAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		action := r.URL.Query().Get("action")

		switch {
		case path == "/sapi/media" && action == "get-storage-space":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"used":    104857600,
					"quota":   0,
					"nolimit": true,
				},
			})
		case path == "/sapi/media/folder/root" && action == "get":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{
						{"id": 100, "name": "Root", "parentid": 0, "magic": true},
					},
				},
			})
		case path == "/sapi/media/folder" && action == "list":
			// Falla SAPI al listar subcarpetas
			http.Error(w, "SAPI listing service temporarily unavailable", http.StatusServiceUnavailable)
		case path == "/sapi/media" && action == "list":
			// O falla SAPI al listar media
			http.Error(w, "SAPI media service temporarily unavailable", http.StatusServiceUnavailable)
		default:
			http.Error(w, "Not found", http.StatusNotFound)
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

	srv := NewServerWithAuth(client, config.AuthConfig{})

	// PROPFIND con Depth: 1 sobre la raíz
	propfindBody := `<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:displayname/>
    <D:resourcetype/>
  </D:prop>
</D:propfind>`

	req := httptest.NewRequest("PROPFIND", "/", strings.NewReader(propfindBody))
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	// Issue #24: Si el backend falla en frío (sin caché), la respuesta NUNCA debe ser 207 Multi-Status
	// simulando una carpeta vacía, porque los clientes de backup interpretarían que sus archivos remotos fueron borrados.
	if rec.Code == http.StatusMultiStatus {
		t.Fatalf("expected failure status (e.g. 500 Internal Server Error) when SAPI listing fails on cold cache, but got 207 Multi-Status (falsely indicating empty directory):\n%s", rec.Body.String())
	}

	if rec.Code < 400 {
		t.Fatalf("expected HTTP >= 400 error status, got %d", rec.Code)
	}
}
