package webdav

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// rclone hace un PROPFIND de cada archivo justo después de subirlo, y x/net/webdav abre
// el archivo para leer sus propiedades. Eso no debe provocar ninguna llamada a O2: la
// URL de descarga solo se consulta si alguien lee el contenido.
func TestPropfindTrasSubida_NoLlamaAO2(t *testing.T) {
	// cuenta las llamadas a O2 aparte de la relectura tras subir (SAPI_REFRESH_AFTER_UPLOAD)
	t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "false")
	f := &fakeO2{}
	f.set(true, false)
	srv, payload := uploadThenServer(t, f)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("PROPFIND", "/pack.bin", strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`))
		req.Header.Set("Depth", "0")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMultiStatus {
			t.Fatalf("PROPFIND %d: esperado 207, obtenido %d", i+1, rec.Code)
		}
	}
	if n := f.queryCount(); n != 0 {
		t.Fatalf("PROPFIND tras subir no debe consultar a O2; hubo %d consultas", n)
	}

	// Al leer el contenido sí se consulta (una vez) y se descarga bien.
	if got := readBack(srv); !bytes.Equal(got, payload) {
		t.Fatalf("lectura tras PROPFIND: %d bytes, esperados %d", len(got), len(payload))
	}
	if n := f.queryCount(); n != 1 {
		t.Errorf("al leer el contenido: esperada 1 consulta, hubo %d", n)
	}
}

// Con la API de O2 bloqueada por el cortafuegos, las escrituras que la necesitan deben
// responder 503 + Retry-After sin enviar nada a O2, en vez de 409/500 que rclone reintenta.
func TestEscriturasDuranteBloqueoWAF_503SinLlamarAO2(t *testing.T) {
	var hits int32
	mock := newShimServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Server", "CloudFront")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<TITLE>ERROR: The request could not be satisfied</TITLE><H1>403 ERROR</H1>")
	}))
	t.Cleanup(mock.Close)

	client, err := sapi.NewClient(config.AccountConfig{
		Provider: "o2", Phone: "612345678",
		AccessToken: "a", RefreshToken: "r", ValidationKey: "vk",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})

	// Primera escritura: llega a O2, que responde 403 del WAF; el cliente entra en pausa.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("MKCOL", "/Fotos/", nil))
	if client.WAFBlockedUntil().IsZero() {
		t.Fatalf("tras el 403 del WAF el cliente debe quedar en pausa (MKCOL devolvió %d)", rec.Code)
	}
	antes := atomic.LoadInt32(&hits)

	// Con la API en pausa: crear una carpeta nueva, borrar y mover necesitan la API -> 503.
	// (PUT no: va al servidor de subidas; ver TestSubidaDuranteBloqueoDeLaAPI.)
	casos := []struct{ metodo, ruta string }{
		{"MKCOL", "/Fotos/"},
		{"DELETE", "/Fotos/a.jpg"},
		{"MOVE", "/Fotos/a.jpg"},
	}
	for _, c := range casos {
		rec := httptest.NewRecorder()
		var body io.Reader
		if c.metodo == "PUT" {
			body = bytes.NewReader([]byte("foto"))
		}
		req := httptest.NewRequest(c.metodo, c.ruta, body)
		if c.metodo == "MOVE" {
			req.Header.Set("Destination", "/Fotos/b.jpg")
		}
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s durante el bloqueo: esperado 503, obtenido %d", c.metodo, rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s durante el bloqueo: falta Retry-After", c.metodo)
		}
	}
	if n := atomic.LoadInt32(&hits); n != antes {
		t.Errorf("durante el bloqueo no debe salir ninguna petición a O2; salieron %d", n-antes)
	}
}

// La API de O2 (detrás del WAF) y el servidor de subidas tienen frenos separados. Medido el
// 12/11/2026: 113 subidas seguidas sin un 403 propio mientras la API se bloqueaba cada ~10
// llamadas; con un freno único, cada bloqueo de la API paraba también las subidas.
// Con la API en pausa, un PUT a una carpeta que ya existe debe subir el archivo (al
// servidor de subidas) sin hacer ninguna llamada a la API.
func TestSubidaDuranteBloqueoDeLaAPI(t *testing.T) {
	var apiHits, uploadHits int32
	mock := newShimServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sapi/upload") {
			atomic.AddInt32(&uploadHits, 1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"777","name":"a.jpg"}}`))
			return
		}
		atomic.AddInt32(&apiHits, 1)
		w.Header().Set("Server", "CloudFront")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<TITLE>ERROR: The request could not be satisfied</TITLE><H1>403 ERROR</H1>")
	}))
	t.Cleanup(mock.Close)

	ms, err := cache.NewMetadataStore(filepath.Join(t.TempDir(), "meta_waf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 1, Name: "/", Magic: true})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 10, Name: "Fotos", ParentID: 1})

	client, err := sapi.NewClient(config.AccountConfig{
		Provider: "o2", Phone: "612345678",
		AccessToken: "a", RefreshToken: "r", ValidationKey: "vk",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true}, ms)

	// Crear una carpeta nueva llama a la API, que responde 403: la API queda en pausa.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("MKCOL", "/Nueva/", nil))
	if client.WAFBlockedUntil().IsZero() {
		t.Fatalf("tras el 403 de la API, la API debe quedar en pausa (MKCOL devolvió %d)", rec.Code)
	}
	if !client.WAFUploadBlockedUntil().IsZero() {
		t.Fatal("un 403 de la API no debe pausar las subidas")
	}
	apiAntes := atomic.LoadInt32(&apiHits)

	// MKCOL de una carpeta que ya existe (rclone lo hace antes de cada subida): local, sin 503.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("MKCOL", "/Fotos/", nil))
	if rec.Code == http.StatusServiceUnavailable {
		t.Errorf("MKCOL de una carpeta existente no debe dar 503 con la API en pausa")
	}

	// La subida va al servidor de subidas y debe completarse.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("PUT", "/Fotos/a.jpg", bytes.NewReader([]byte("foto de prueba"))))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("PUT con la API en pausa: esperado 201/204, obtenido %d (%s)", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&uploadHits); n != 1 {
		t.Errorf("esperada 1 subida al servidor de subidas, hubo %d", n)
	}
	if n := atomic.LoadInt32(&apiHits); n != apiAntes {
		t.Errorf("con la API en pausa no debe salir ninguna llamada a la API; salieron %d", n-apiAntes)
	}
}

// Con la base SQLite ya sincronizada, comprobar si el destino de un PUT existe se hace en
// local: subir un archivo nuevo no debe hacer ninguna llamada a la API (antes, 2 por
// archivo: ListAllSubfolders + ListAllMediaInFolder, lo que disparaba el WAF).
func TestSubidaNuevaSinConsultarALaAPI(t *testing.T) {
	// cuenta las llamadas a O2 aparte de la relectura tras subir (SAPI_REFRESH_AFTER_UPLOAD)
	t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "false")
	var apiHits, uploadHits int32
	mock := newShimServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sapi/upload") {
			atomic.AddInt32(&uploadHits, 1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"888","name":"nueva.jpg"}}`))
			return
		}
		atomic.AddInt32(&apiHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(mock.Close)

	ms, err := cache.NewMetadataStore(filepath.Join(t.TempDir(), "meta_put.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 1, Name: "/", Magic: true})
	_ = ms.SetSyncState("root_folder_id", "1")
	_ = ms.SetInitialSync()
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 10, Name: "Fotos", ParentID: 1})

	client, err := sapi.NewClient(config.AccountConfig{
		Provider: "o2", Phone: "612345678",
		AccessToken: "a", RefreshToken: "r", ValidationKey: "vk",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true}, ms)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("PUT", "/Fotos/nueva.jpg", bytes.NewReader([]byte("foto nueva"))))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("PUT: esperado 201/204, obtenido %d (%s)", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&uploadHits); n != 1 {
		t.Errorf("esperada 1 subida, hubo %d", n)
	}
	if n := atomic.LoadInt32(&apiHits); n != 0 {
		t.Errorf("con la base sincronizada, subir un archivo nuevo no debe llamar a la API; hubo %d llamadas", n)
	}
}
