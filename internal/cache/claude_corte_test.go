// Prueba: sincronización inicial cortada a mitad por un 403 (no debe quedarse incompleta).
package cache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func TestClaude_SincronizacionInicialCortada(t *testing.T) {
	store, _ := createTempStore(t)
	const total = 2500
	var fallar atomic.Bool
	fallar.Store(true)

	ids := make([]int64, total)
	for i := range ids {
		ids[i] = int64(1000 + i)
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/profile/changes") && r.URL.Query().Get("from") != "0":
			// Como O2 de verdad: pidiendo «cambios desde ahora» no hay nada nuevo
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"requesttime": int64(1794389793000), "responsetime": int64(1794389795000), "data": map[string]interface{}{}})
		case strings.Contains(r.URL.Path, "/profile/changes"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"requesttime": int64(1794389693000), "responsetime": int64(1794389695000),
				"data": map[string]interface{}{
					"folder": map[string]interface{}{"N": []int64{10, 20}},
					"file":   map[string]interface{}{"N": ids},
				},
			})
		case strings.Contains(r.URL.Path, "/media/folder"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{
					{"id": 10, "name": "Root", "parentid": 0, "magic": true},
					{"id": 20, "name": "Restic_QNAP", "parentid": 10},
				}}})
		case strings.Contains(r.URL.Path, "/media"):
			var body struct {
				Data struct {
					IDs []int64 `json:"ids"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			// Simula el bloqueo de O2 (CloudFront) a partir del elemento 1000
			if fallar.Load() && len(body.Data.IDs) > 0 && body.Data.IDs[0] >= 2000 {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("<H1>403 ERROR</H1> Request blocked."))
				return
			}
			var media []map[string]interface{}
			for _, id := range body.Data.IDs {
				media = append(media, map[string]interface{}{"id": id, "name": "f" + itoa(id), "size": 10, "folderid": 20, "url": "http://x/"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": media}})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	client, err := sapi.NewClient(config.AccountConfig{Provider: "o2", Phone: "600000000", AccessToken: "t", RefreshToken: "r", ValidationKey: "v"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	// 1) Primera sincronización: O2 bloquea a mitad
	_, err1 := store.SyncChanges(client, false)
	m1, _ := store.ListMediaInFolder(20)
	t.Logf("1.ª pasada: error=%v | ficheros guardados=%d de %d | HasInitialSync=%v", err1 != nil, len(m1), total, store.HasInitialSync())

	// 2) O2 vuelve a responder; el programa reintenta como hace cada minuto
	fallar.Store(false)
	_, err2 := store.SyncChanges(client, false)
	m2, _ := store.ListMediaInFolder(20)
	t.Logf("2.ª pasada: error=%v | ficheros guardados=%d de %d | HasInitialSync=%v", err2, len(m2), total, store.HasInitialSync())

	if len(m2) != total {
		t.Errorf("FALLO CONFIRMADO: tras reintentar faltan %d de %d ficheros y el programa da la lista por completa", total-len(m2), total)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
