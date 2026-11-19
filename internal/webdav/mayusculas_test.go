package webdav

import (
	"bytes"
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

// nubeConMayusculas simula una carpeta raíz de O2 que, como O2, admite nombres que
// solo difieren en mayúsculas.
type nubeConMayusculas struct {
	mu      sync.Mutex
	next    int
	nombres map[string]string // id → nombre
	datos   map[string][]byte
	metas   []map[string]interface{}
	borrado []string

	fallarRenombrado bool // O2 responde con un error a renombrar
}

func nuevaNubeConMayusculas(t *testing.T) (*Server, *nubeConMayusculas) {
	t.Helper()
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	n := &nubeConMayusculas{next: 1000, nombres: map[string]string{}, datos: map[string][]byte{}}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		n.mu.Lock()
		defer n.mu.Unlock()
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}}}})
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save-metadata":
			var body struct {
				Data map[string]interface{} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			n.metas = append(n.metas, body.Data)
			if n.fallarRenombrado && body.Data["id"] != nil {
				_, _ = w.Write([]byte(`{"error":{"code":"MED-1000","message":"Unknown exception in media handling"}}`))
				return
			}
			id := fmt.Sprint(body.Data["id"])
			if body.Data["id"] == nil {
				n.next++
				id = fmt.Sprint(n.next)
			}
			n.nombres[id] = fmt.Sprint(body.Data["name"])
			fmt.Fprintf(w, `{"success":"ok","id":"%s"}`, id)
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save":
			id := r.Header.Get("x-funambol-id")
			b, _ := io.ReadAll(r.Body)
			n.datos[id] = b
			fmt.Fprintf(w, `{"success":"Media uploaded successfully","id":"%s","status":"U"}`, id)
		case r.URL.Path == "/sapi/media" && action == "get":
			var items []map[string]interface{}
			for id, name := range n.nombres {
				items = append(items, map[string]interface{}{"id": id, "name": name, "size": len(n.datos[id]), "folderid": 100,
					"url": "/sapi/media/content?id=" + id, "creationdate": "20260101T000000Z", "modificationdate": "20260101T000000Z"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": items}})
		case r.URL.Path == "/sapi/media/content":
			_, _ = w.Write(n.datos[r.URL.Query().Get("id")])
		case action == "softdelete" || action == "delete":
			n.borrado = append(n.borrado, r.URL.Query().Get("id"))
			delete(n.nombres, r.URL.Query().Get("id"))
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
	return NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true}), n
}

func put(t *testing.T, srv *Server, p, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("PUT", p, bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("PUT %s = %d", p, rec.Code)
	}
}

func get(t *testing.T, srv *Server, p string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", p, rec.Code)
	}
	return rec.Body.String()
}

// O2 guarda "Cancion.mp3" y "cancion.mp3" como dos archivos. Antes, subir el
// segundo sobrescribía el primero (y en cada copia se pisaban el uno al otro).
func TestMayusculas_DosArchivosQueSoloDifierenEnMayusculas(t *testing.T) {
	srv, nube := nuevaNubeConMayusculas(t)
	put(t, srv, "/Cancion.txt", "MAYUSCULA")
	put(t, srv, "/cancion.txt", "minuscula")

	nube.mu.Lock()
	if len(nube.metas) != 2 {
		t.Fatalf("save-metadata = %d", len(nube.metas))
	}
	if _, sobrescribe := nube.metas[1]["id"]; sobrescribe {
		t.Fatalf("subir 'cancion.txt' sobrescribió 'Cancion.txt': %v", nube.metas[1])
	}
	nube.mu.Unlock()

	if got := get(t, srv, "/Cancion.txt"); got != "MAYUSCULA" {
		t.Errorf("GET /Cancion.txt = %q", got)
	}
	if got := get(t, srv, "/cancion.txt"); got != "minuscula" {
		t.Errorf("GET /cancion.txt = %q", got)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Depth", "1")
	srv.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "Cancion.txt") || !strings.Contains(body, "cancion.txt") {
		t.Errorf("el listado debe mostrar los dos archivos:\n%s", body)
	}

	// Volver a subir uno de ellos sobrescribe solo ese.
	put(t, srv, "/cancion.txt", "minuscula v2")
	nube.mu.Lock()
	last := nube.metas[len(nube.metas)-1]
	nube.mu.Unlock()
	if fmt.Sprint(last["name"]) != "cancion.txt" || last["id"] == nil {
		t.Errorf("la nueva versión debe sobrescribir 'cancion.txt' por su ID: %v", last)
	}
	if got := get(t, srv, "/Cancion.txt"); got != "MAYUSCULA" {
		t.Errorf("'Cancion.txt' cambió al sobrescribir 'cancion.txt': %q", got)
	}
}

// Cambiar solo las mayúsculas de un nombre es un renombrado, no un «destino que ya
// existe»: el archivo no se puede borrar después de moverlo.
func TestMayusculas_RenombrarSoloMayusculasNoBorraElArchivo(t *testing.T) {
	srv, nube := nuevaNubeConMayusculas(t)
	put(t, srv, "/Informe.txt", "datos")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("MOVE", "/Informe.txt", nil)
	req.Header.Set("Destination", "http://example.com/informe.txt")
	req.Host = "example.com"
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("MOVE = %d", rec.Code)
	}
	nube.mu.Lock()
	defer nube.mu.Unlock()
	if len(nube.borrado) != 0 {
		t.Fatalf("se borró %v al cambiar solo las mayúsculas", nube.borrado)
	}
	var nombres []string
	for _, n := range nube.nombres {
		nombres = append(nombres, n)
	}
	if len(nombres) != 1 || nombres[0] != "informe.txt" {
		t.Fatalf("en la nube: %v, se esperaba solo 'informe.txt'", nombres)
	}
}

// Por defecto los nombres son exactos, como en O2: pedir «foto.jpg» cuando solo
// existe «Foto.JPG» es «no existe». Si el proxy respondiera con «Foto.JPG», rclone
// daría por subido un archivo que no lo está, o en un cambio solo de mayúsculas
// (moveto b.txt -> B.txt) creería que origen y destino son el mismo y BORRARÍA el
// origen (visto en la prueba real del 18/11/2026).
func TestMayusculas_PorDefectoNombresExactos(t *testing.T) {
	srv, _ := nuevaNubeConMayusculas(t)
	put(t, srv, "/b.txt", "tres")
	for _, m := range []string{"PROPFIND", "GET"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(m, "/B.txt", nil)
		req.Header.Set("Depth", "0")
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s /B.txt = %d; con solo 'b.txt' debe ser 404", m, rec.Code)
		}
	}
}

// Con WEBDAV_CASE_INSENSITIVE=true (Explorador de Windows), un nombre con otras
// mayúsculas sigue encontrando el archivo si no hay otro con ese nombre exacto.
func TestMayusculas_ModoOpcionalSinDistinguir(t *testing.T) {
	t.Setenv("WEBDAV_CASE_INSENSITIVE", "true")
	srv, _ := nuevaNubeConMayusculas(t)
	put(t, srv, "/Foto.JPG", "jpg")
	if got := get(t, srv, "/foto.jpg"); got != "jpg" {
		t.Errorf("GET /foto.jpg = %q", got)
	}
}

// Con "Overwrite: T", x/net/webdav borraba el «destino» (el propio origen) antes de moverlo.
func TestMayusculas_RenombrarSoloMayusculasConOverwrite(t *testing.T) {
	srv, nube := nuevaNubeConMayusculas(t)
	put(t, srv, "/Datos.bin", "contenido")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("MOVE", "/Datos.bin", nil)
	req.Header.Set("Destination", "http://example.com/DATOS.bin")
	req.Header.Set("Overwrite", "T")
	req.Host = "example.com"
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("MOVE = %d", rec.Code)
	}
	nube.mu.Lock()
	borrados := append([]string(nil), nube.borrado...)
	nube.mu.Unlock()
	if len(borrados) != 0 {
		t.Fatalf("se borró el archivo: %v", borrados)
	}
	if got := get(t, srv, "/DATOS.bin"); got != "contenido" {
		t.Errorf("GET tras renombrar = %q", got)
	}
}

// Si O2 falla al cambiar solo las mayúsculas (un fallo suyo, no un permiso), el
// cliente recibe 503 + Retry-After para reintentar, no un 403 que lo daría por
// imposible.
func TestMayusculas_RenombrarSoloMayusculasFalloDeO2PideReintentar(t *testing.T) {
	srv, nube := nuevaNubeConMayusculas(t)
	put(t, srv, "/Informe.txt", "datos")
	nube.mu.Lock()
	nube.fallarRenombrado = true
	nube.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("MOVE", "/Informe.txt", nil)
	req.Header.Set("Destination", "http://example.com/informe.txt")
	req.Host = "example.com"
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("MOVE = %d (Retry-After %q), esperado 503 con Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
}
