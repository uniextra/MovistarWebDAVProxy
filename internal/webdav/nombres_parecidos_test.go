package webdav

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

// o2ConFotos simula una carpeta raíz con las fotos dadas (nombre → id) y cuenta los borrados.
func o2ConFotos(t *testing.T, fotos [][2]string) (*Server, *atomic.Int32, func()) {
	t.Helper()
	var borrados atomic.Int32
	o2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}}}})
		case r.URL.Path == "/sapi/media/folder" && action == "list":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"folders": []interface{}{}}})
		case r.URL.Path == "/sapi/media" && action == "get":
			var media []map[string]interface{}
			for _, f := range fotos {
				media = append(media, map[string]interface{}{"id": f[1], "name": f[0], "size": 5, "folderid": 100,
					"url": "/sapi/media/content?id=" + f[1], "creationdate": "20260101T000000Z", "modificationdate": "20260101T000000Z"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": media}})
		case (strings.HasPrefix(r.URL.Path, "/sapi/media/") && action == "delete") || (r.URL.Path == "/sapi/media" && action == "softdelete"):
			borrados.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		}
	}))
	client, err := sapi.NewClient(config.AccountConfig{Provider: "o2", Phone: "612345678",
		AccessToken: "a", RefreshToken: "r", ValidationKey: "v"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.SetBaseURLs(o2.URL+"/sapi", o2.URL)
	return NewServerWithAuth(client, config.AuthConfig{}), &borrados, o2.Close
}

func TestNoConfundeFotoConFoto1_AlBuscar(t *testing.T) {
	srv, borrados, cerrar := o2ConFotos(t, [][2]string{{"foto (1).jpg", "1"}})
	defer cerrar()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PROPFIND", "/foto.jpg", nil)
	req.Header.Set("Depth", "0")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foto.jpg no existe (solo foto (1).jpg) y PROPFIND respondió %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("DELETE", "/foto.jpg", nil))
	if n := borrados.Load(); n != 0 {
		t.Errorf("al borrar foto.jpg se borró otra foto (foto (1).jpg)")
	}
}

func TestNoConfundeFotoConFoto1_AlListar(t *testing.T) {
	// foto (1).jpg sale antes en la lista: antes se mostraba como "foto.jpg" y escondía la de verdad.
	srv, _, cerrar := o2ConFotos(t, [][2]string{{"foto (1).jpg", "1"}, {"foto.jpg", "2"}})
	defer cerrar()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Depth", "1")
	srv.ServeHTTP(rec, req)
	cuerpo := rec.Body.String()
	for _, n := range []string{"foto%20%281%29.jpg", "foto.jpg"} {
		if !strings.Contains(cuerpo, n) {
			t.Errorf("la lista de la carpeta no tiene %q", n)
		}
	}
}
