package webdav

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// cuerpoQueSeCorta entrega unos bytes y luego falla, como una conexión que se cae a mitad.
type cuerpoQueSeCorta struct{ enviado bool }

func (c *cuerpoQueSeCorta) Read(p []byte) (int, error) {
	if c.enviado {
		return 0, io.ErrUnexpectedEOF
	}
	c.enviado = true
	return copy(p, "solo un trozo"), nil
}

func (c *cuerpoQueSeCorta) Close() error { return nil }

// servidorQueCuentaSubidas monta el O2 de mentira y cuenta las subidas que le llegan.
func servidorQueCuentaSubidas(t *testing.T) (*Server, *atomic.Int32, func()) {
	t.Helper()
	base := newMockSAPIServer(t)
	var subidas atomic.Int32
	contador := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sapi/upload" {
			subidas.Add(1)
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
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
	client.SetBaseURLs(contador.URL+"/sapi", contador.URL)
	srv := NewServerWithAuth(client, config.AuthConfig{})
	return srv, &subidas, func() { contador.Close(); base.Close() }
}

func TestSubidaCortada_NoSeSubeNada(t *testing.T) {
	srv, subidas, cerrar := servidorQueCuentaSubidas(t)
	defer cerrar()

	req := httptest.NewRequest("PUT", "/cortada.bin", &cuerpoQueSeCorta{})
	req.ContentLength = 1000
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if n := subidas.Load(); n != 0 {
		t.Errorf("la conexión se cortó y aun así se subieron %d ficheros a la nube", n)
	}
	if rec.Code == http.StatusCreated || rec.Code == http.StatusNoContent {
		t.Errorf("una subida cortada no puede responder éxito, respondió %d", rec.Code)
	}
}

func TestSubidaCorta_FaltanBytes_NoSeSubeNada(t *testing.T) {
	srv, subidas, cerrar := servidorQueCuentaSubidas(t)
	defer cerrar()

	// Anuncia 1000 bytes pero solo llegan 10 (fin de datos antes de tiempo).
	req := httptest.NewRequest("PUT", "/corta.bin", io.NopCloser(strings.NewReader("0123456789")))
	req.ContentLength = 1000
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if n := subidas.Load(); n != 0 {
		t.Errorf("faltaban bytes y aun así se subieron %d ficheros a la nube", n)
	}
}

func TestSubidaEntera_SiSeSube(t *testing.T) {
	srv, subidas, cerrar := servidorQueCuentaSubidas(t)
	defer cerrar()

	req := httptest.NewRequest("PUT", "/entera.bin", strings.NewReader("hola mundo"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("una subida entera debe ir bien, respondió %d", rec.Code)
	}
	if n := subidas.Load(); n != 1 {
		t.Errorf("se esperaba 1 subida a la nube, hubo %d", n)
	}
}
