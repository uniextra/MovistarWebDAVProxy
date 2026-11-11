package webdav

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// rclone hace un PROPFIND de cada archivo justo después de subirlo, y x/net/webdav abre
// el archivo para leer sus propiedades. Eso no debe provocar ninguna llamada a O2: la
// URL de descarga solo se consulta si alguien lee el contenido.
func TestPropfindTrasSubida_NoLlamaAO2(t *testing.T) {
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

// Con O2 bloqueando por el cortafuegos, las escrituras deben responder 503 + Retry-After
// sin enviar nada a O2, en vez de 409/500 que rclone reintenta sin fin.
func TestEscriturasDuranteBloqueoWAF_503SinLlamarAO2(t *testing.T) {
	var hits int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	casos := []struct{ metodo, ruta string }{
		{"MKCOL", "/Fotos/"},
		{"PUT", "/Fotos/a.jpg"},
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
