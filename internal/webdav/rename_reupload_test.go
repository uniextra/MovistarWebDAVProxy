package webdav

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// reuploadO2 simula lo observado en O2 Cloud al renombrar con cambio de extensión:
//   - la respuesta de subida no incluye "url" y la URL por defecto (/media/content?id=)
//     responde con un error JSON (COM-1005) en vez del archivo;
//   - la consulta por ID no conoce un archivo recién subido hasta pasadas unas
//     consultas (en O2 Cloud, unos segundos) y después devuelve la URL real;
//   - el listado de la carpeta tarda en incluir los archivos recién subidos.
type reuploadO2 struct {
	mu      sync.Mutex
	nextID  int
	content map[string][]byte // ID -> contenido
	names   map[string]string // ID -> nombre
	listed  map[string]bool   // IDs que ya aparecen en el listado de la carpeta
	deleted []string
	lag     int            // consultas por ID que responden "no encontrado" tras subir
	queries map[string]int // ID -> consultas por ID recibidas
	reads   int            // descargas solicitadas (URL real o por defecto)
}

func newReuploadO2() *reuploadO2 {
	return &reuploadO2{nextID: 1000, lag: 2, content: map[string][]byte{}, names: map[string]string{}, listed: map[string]bool{}, queries: map[string]int{}}
}

// seed añade un archivo ya asentado (visible en el listado).
func (f *reuploadO2) seed(name string, b []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	f.content[id], f.names[id], f.listed[id] = b, name, true
	f.queries[id] = f.lag // asentado: la consulta por ID ya lo conoce
	return id
}

func (f *reuploadO2) wasDeleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.deleted {
		if d == id {
			return true
		}
	}
	return false
}

func (f *reuploadO2) idOf(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, n := range f.names {
		if n == name {
			return id
		}
	}
	return ""
}

func (f *reuploadO2) handler(t *testing.T) http.Handler {
	jsonOut := func(w http.ResponseWriter, v interface{}) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		action := q.Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			jsonOut(w, map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}},
			}})
		case r.URL.Path == "/sapi/media/folder" && action == "list":
			jsonOut(w, map[string]interface{}{"data": map[string]interface{}{"folders": []interface{}{}}})
		case r.URL.Path == "/sapi/upload" && action == "save":
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				t.Errorf("multipart inválido: %v", err)
			}
			var meta struct {
				Data struct {
					Name string `json:"name"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(r.FormValue("data")), &meta)
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("subida sin archivo: %v", err)
				return
			}
			b, _ := io.ReadAll(file)
			f.mu.Lock()
			f.nextID++
			id := strconv.Itoa(f.nextID)
			f.content[id], f.names[id] = b, meta.Data.Name // no listado todavía
			f.mu.Unlock()
			jsonOut(w, map[string]interface{}{"data": map[string]interface{}{"id": id, "name": meta.Data.Name}}) // sin "url"
		case r.URL.Path == "/sapi/media" && action == "get" && q.Get("origin") != "":
			var body struct {
				Data struct {
					IDs []string `json:"ids"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			media := []map[string]interface{}{}
			f.mu.Lock()
			for _, id := range body.Data.IDs {
				f.queries[id]++
				if b, ok := f.content[id]; ok && f.queries[id] > f.lag {
					media = append(media, map[string]interface{}{"id": id, "name": f.names[id], "folderid": 100, "size": len(b), "url": "/sapi/download/real?id=" + id})
				}
			}
			f.mu.Unlock()
			jsonOut(w, map[string]interface{}{"data": map[string]interface{}{"media": media}})
		case r.URL.Path == "/sapi/media" && action == "get" && q.Get("folderid") != "":
			media := []map[string]interface{}{}
			f.mu.Lock()
			if q.Get("offset") == "" {
				for id, b := range f.content {
					if f.listed[id] {
						media = append(media, map[string]interface{}{"id": id, "name": f.names[id], "size": len(b), "url": "/sapi/download/real?id=" + id})
					}
				}
			}
			f.mu.Unlock()
			jsonOut(w, map[string]interface{}{"data": map[string]interface{}{"media": media}})
		case r.URL.Path == "/sapi/download/real":
			f.mu.Lock()
			f.reads++
			b, ok := f.content[q.Get("id")]
			f.mu.Unlock()
			if !ok {
				jsonOut(w, map[string]interface{}{"error": map[string]interface{}{"code": "MED-1005", "message": "Unable to retrieve media with given id"}})
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(b)
		case r.URL.Path == "/sapi/media/content":
			// URL por defecto: el servidor real responde con un error JSON
			f.mu.Lock()
			f.reads++
			f.mu.Unlock()
			jsonOut(w, map[string]interface{}{"error": map[string]interface{}{"code": "COM-1005", "message": "Unsupported operation"}})
		case r.URL.Path == "/sapi/media" && action == "softdelete":
			id := q.Get("id")
			f.mu.Lock()
			f.deleted = append(f.deleted, id)
			delete(f.content, id)
			delete(f.names, id)
			delete(f.listed, id)
			f.mu.Unlock()
			jsonOut(w, map[string]interface{}{"data": map[string]interface{}{}})
		default:
			jsonOut(w, map[string]interface{}{"data": map[string]interface{}{}})
		}
	})
}

func newReuploadServer(t *testing.T, f *reuploadO2) *Server {
	t.Helper()
	mock := httptest.NewServer(f.handler(t))
	t.Cleanup(mock.Close)
	client, err := sapi.NewClient(config.AccountConfig{
		Provider: "movistar", Phone: "612345678",
		AccessToken: "a", RefreshToken: "r", ValidationKey: "vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	oldPoll := reuploadSourcePoll
	reuploadSourcePoll = time.Millisecond
	t.Cleanup(func() { reuploadSourcePoll = oldPoll })
	return NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})
}

func do(srv *Server, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// moveAndCheck renombra /<from> a /<to> y comprueba que el destino se lee en seguida
// con el contenido correcto y que borrarlo borra el archivo nuevo en la nube.
func moveAndCheck(t *testing.T, srv *Server, f *reuploadO2, from, to string, payload []byte) {
	t.Helper()
	oldID := f.idOf(from)
	if oldID == "" {
		t.Fatalf("el archivo de origen %q no está en la nube simulada", from)
	}

	rec := do(srv, "MOVE", "/"+from, nil, map[string]string{"Destination": "/" + to, "Overwrite": "T"})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("MOVE esperado 201/204, obtenido %d: %s", rec.Code, rec.Body.String())
	}
	newID := f.idOf(to)
	if newID == "" {
		t.Fatalf("no se subió %q a la nube", to)
	}
	if !f.wasDeleted(oldID) {
		t.Errorf("el original %q (ID %s) debería haberse borrado", from, oldID)
	}

	// Inmediatamente, aunque el listado de SAPI aún no lo incluya
	if rec := do(srv, "PROPFIND", "/"+to, nil, map[string]string{"Depth": "0"}); rec.Code != http.StatusMultiStatus {
		t.Errorf("PROPFIND del destino esperado 207, obtenido %d", rec.Code)
	}
	// Se lee en cuanto SAPI lo sirve, como cualquier subida reciente
	var got []byte
	for i := 0; i <= 2*f.lag && !bytes.Equal(got, payload); i++ {
		got = do(srv, "GET", "/"+to, nil, nil).Body.Bytes()
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("lectura del destino: %d bytes, esperados %d", len(got), len(payload))
	}

	// Borrarlo debe borrar el archivo nuevo, no el ID ya eliminado
	if rec := do(srv, "DELETE", "/"+to, nil, nil); rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Errorf("DELETE del destino esperado 204, obtenido %d", rec.Code)
	}
	if !f.wasDeleted(newID) {
		t.Errorf("DELETE del destino no borró el archivo nuevo (ID %s)", newID)
	}
}

// Recién subido y renombrado al momento con otra extensión (p. ej. Duplicati).
func TestRename_ExtensionChange_FreshUpload(t *testing.T) {
	for _, size := range []int{100 * 1024, 1024 * 1024} {
		t.Run(fmt.Sprintf("%dKB", size/1024), func(t *testing.T) {
			f := newReuploadO2()
			srv := newReuploadServer(t, f)
			payload := randomBytes(size)
			if rec := do(srv, "PUT", "/a.part", payload, nil); rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
				t.Fatalf("PUT esperado 201/204, obtenido %d", rec.Code)
			}
			f.mu.Lock()
			f.reads = 0
			f.mu.Unlock()
			moveAndCheck(t, srv, f, "a.part", "a.zip.aes", payload)
			if size <= 256*1024 {
				// El pequeño se re-sube desde la copia en memoria: la única descarga
				// es la lectura de comprobación, que también sale de memoria.
				f.mu.Lock()
				reads := f.reads
				f.mu.Unlock()
				if reads != 0 {
					t.Errorf("re-subida de un archivo pequeño recién subido: esperadas 0 descargas, hubo %d", reads)
				}
			}
		})
	}
}

// Archivo ya asentado (fuera de la caché de subidas).
func TestRename_ExtensionChange_SettledFile(t *testing.T) {
	f := newReuploadO2()
	srv := newReuploadServer(t, f)
	payload := randomBytes(1024 * 1024)
	f.seed("c.part", payload)
	moveAndCheck(t, srv, f, "c.part", "c.zip.aes", payload)
}
