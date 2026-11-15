package webdav

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

type fakeWebLogin struct{}

func (fakeWebLogin) CheckLogin(context.Context) bool { return true }
func (fakeWebLogin) LoggedIn() bool                  { return true }

// copyCloud simula SAPI con /a.bin en la raíz (100) y la carpeta /dst (200).
// La copia en la nube responde copyReply; si va bien, la copia aparece en /dst.
func copyCloud(t *testing.T, copyReply string) (*Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var copies []string
	copied := false
	media := func(id, name string, folder int) map[string]interface{} {
		return map[string]interface{}{"id": id, "name": name, "size": 10, "folderid": folder,
			"url": "/sapi/download/" + id, "creationdate": "20260101T000000Z", "modificationdate": "20260101T000000Z"}
	}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}}}})
		case r.URL.Path == "/sapi/media/folder" && action == "list":
			folders := []interface{}{}
			if r.URL.Query().Get("parentid") == "100" {
				folders = append(folders, map[string]interface{}{"id": 200, "name": "dst", "parentid": 100})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"folders": folders}})
		case r.URL.Path == "/sapi/media" && action == "get":
			items := []interface{}{}
			switch r.URL.Query().Get("folderid") {
			case "100":
				items = append(items, media("555", "a.bin", 100))
			case "200":
				if copied {
					items = append(items, media("999", "b.bin", 200))
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": items}})
		case r.URL.Path == "/sapi/media/file" && action == "copy":
			b, _ := io.ReadAll(r.Body)
			copies = append(copies, string(b))
			if strings.Contains(copyReply, `"id"`) {
				copied = true
			}
			_, _ = io.WriteString(w, copyReply)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	t.Cleanup(mock.Close)
	client, err := sapi.NewClient(config.AccountConfig{Provider: "movistar"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	client.EnableWebSession(fakeWebLogin{}, http.DefaultTransport)
	return NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true}), &copies
}

func copyReq(dst string) *http.Request {
	req := httptest.NewRequest("COPY", "/a.bin", nil)
	req.Header.Set("Destination", "http://example.com"+dst)
	req.Host = "example.com"
	return req
}

// COPY de un archivo en modo web: una sola llamada de copia en la nube, como la web.
func TestCopy_WebModeCopiesInCloud(t *testing.T) {
	srv, copies := copyCloud(t, `{"data":{"id":"999"}}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, copyReq("/dst/b.bin"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("COPY = %d, esperado 201", rec.Code)
	}
	if len(*copies) != 1 || (*copies)[0] != `{"data":{"id":555,"name":"b.bin","parentid":200}}` {
		t.Errorf("copias = %q", *copies)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("PROPFIND", "/dst/b.bin", nil)
	req.Header.Set("Depth", "0")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Errorf("la copia no aparece: PROPFIND = %d", rec.Code)
	}
}

// Si la nube rechaza la copia, o el destino ya existe, se deja al manejador normal.
func TestCopy_FallsBackWhenRejectedOrExisting(t *testing.T) {
	srv, copies := copyCloud(t, `{"error":{"code":"MED-1000","message":"x"}}`)
	if srv.tryServerCopy(httptest.NewRecorder(), copyReq("/dst/b.bin")) {
		t.Error("con la copia rechazada no debe darse por atendida")
	}
	if len(*copies) != 1 {
		t.Errorf("esperado 1 intento de copia, hubo %d", len(*copies))
	}
	if srv.tryServerCopy(httptest.NewRecorder(), copyReq("/a.bin")) {
		t.Error("copiar sobre sí mismo no debe atenderse")
	}
	if srv.tryServerCopy(httptest.NewRecorder(), copyReq("/dst")) {
		t.Error("destino existente (carpeta) no debe atenderse")
	}
	if len(*copies) != 1 {
		t.Errorf("no debía haber más copias, hubo %d", len(*copies))
	}
}
