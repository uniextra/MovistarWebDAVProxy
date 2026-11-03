package webdav

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// fakeO2 simula el comportamiento observado en O2 Cloud: la respuesta de subida no
// incluye "url", la URL por defecto (/media/content?id=) no sirve el archivo, y la
// consulta del archivo por ID devuelve la URL de descarga real.
type fakeO2 struct {
	mu        sync.Mutex
	stored    []byte
	indexed   bool // si la consulta por ID ya conoce el archivo
	failQuery bool // si la consulta por ID responde con error HTTP 500
	queries   int  // número de consultas por ID recibidas
}

func (f *fakeO2) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}},
				},
			})
		case r.URL.Path == "/sapi/upload" && action == "save":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart inválido: %v", err)
			}
			if file, _, err := r.FormFile("file"); err == nil {
				b, _ := io.ReadAll(file)
				f.mu.Lock()
				f.stored = b
				f.mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"777","name":"pack.bin"}}`)) // sin "url"
		case r.URL.Path == "/sapi/media" && action == "get" && r.URL.Query().Get("origin") != "":
			var body struct {
				Data struct {
					IDs []string `json:"ids"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.queries++
			fail, indexed := f.failQuery, f.indexed
			f.mu.Unlock()
			if fail {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
			media := []map[string]interface{}{}
			if indexed && len(body.Data.IDs) == 1 && body.Data.IDs[0] == "777" {
				media = append(media, map[string]interface{}{
					"id": "777", "name": "pack.bin", "folderid": 100, "url": "/sapi/download/real?id=777",
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": media}})
		case r.URL.Path == "/sapi/download/real":
			f.mu.Lock()
			b := f.stored
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(b)
		case r.URL.Path == "/sapi/media/content":
			// URL por defecto: el servidor real no entrega el archivo aquí
			w.Header().Set("Content-Type", "text/html")
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	})
}

func (f *fakeO2) set(indexed, failQuery bool) {
	f.mu.Lock()
	f.indexed, f.failQuery = indexed, failQuery
	f.mu.Unlock()
}

func (f *fakeO2) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries
}

// uploadThenServer sube 300 KB (más del límite de 256 KB que se sirve desde memoria).
func uploadThenServer(t *testing.T, f *fakeO2) (*Server, []byte) {
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
	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})

	payload := make([]byte, 300*1024)
	_, _ = rand.Read(payload)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("PUT", "/pack.bin", bytes.NewReader(payload)))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("PUT esperado 201/204, obtenido %d", rec.Code)
	}
	return srv, payload
}

func readBack(srv *Server) []byte {
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/pack.bin", nil))
	return rec.Body.Bytes()
}

// Caso normal: SAPI ya conoce el archivo -> lectura inmediata correcta y la URL se
// recuerda (una sola consulta aunque se lea varias veces).
func TestFreshRead_FoundIsUsedAndRemembered(t *testing.T) {
	f := &fakeO2{}
	f.set(true, false)
	srv, payload := uploadThenServer(t, f)

	for i := 0; i < 3; i++ {
		if got := readBack(srv); !bytes.Equal(got, payload) {
			t.Fatalf("lectura %d: %d bytes, esperados %d", i+1, len(got), len(payload))
		}
	}
	if n := f.queryCount(); n != 1 {
		t.Errorf("esperada 1 consulta por ID, hubo %d", n)
	}
}

// Aún indexándose: no hay esperas ni se recuerda nada; la siguiente lectura vuelve a
// consultar y, en cuanto SAPI lo conoce, funciona.
func TestFreshRead_NotYetIndexedRetriesOnNextRead(t *testing.T) {
	f := &fakeO2{}
	f.set(false, false)
	srv, payload := uploadThenServer(t, f)

	start := time.Now()
	_ = readBack(srv) // URL conocida (rota): igual que antes del cambio
	if d := time.Since(start); d > time.Second {
		t.Errorf("la lectura no debe esperar; tardó %v", d)
	}

	f.set(true, false)
	if got := readBack(srv); !bytes.Equal(got, payload) {
		t.Fatalf("tras indexarse: %d bytes, esperados %d", len(got), len(payload))
	}
	if n := f.queryCount(); n != 2 {
		t.Errorf("esperadas 2 consultas (una por lectura), hubo %d", n)
	}
}

// Error de SAPI: se usa la URL conocida y no se vuelve a consultar ese archivo hasta
// pasado el intervalo de espera.
func TestFreshRead_ErrorBacksOffBeforeRetrying(t *testing.T) {
	old := urlLookupErrorBackoff
	urlLookupErrorBackoff = 200 * time.Millisecond
	defer func() { urlLookupErrorBackoff = old }()

	f := &fakeO2{}
	f.set(true, true) // la consulta por ID falla
	srv, payload := uploadThenServer(t, f)

	_ = readBack(srv)
	_ = readBack(srv) // dentro del intervalo: no debe consultar
	if n := f.queryCount(); n != 1 {
		t.Fatalf("dentro del intervalo de espera: esperada 1 consulta, hubo %d", n)
	}

	f.set(true, false)
	time.Sleep(250 * time.Millisecond)
	if got := readBack(srv); !bytes.Equal(got, payload) {
		t.Fatalf("tras el intervalo: %d bytes, esperados %d", len(got), len(payload))
	}
	if n := f.queryCount(); n != 2 {
		t.Errorf("tras el intervalo: esperadas 2 consultas, hubo %d", n)
	}
}
