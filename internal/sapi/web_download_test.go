package sapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/config"
)

type fakeWebSession struct{}

func (fakeWebSession) CheckLogin(context.Context) bool { return true }
func (fakeWebSession) LoggedIn() bool                  { return true }

const webTestContent = "0123456789abcdefghijklmnopqrstuvwxyz"

// webDownloadServer simula SAPI: /dl/old es una URL firmada caducada (403),
// /dl/new la renovada (admite rangos), /dl/norange ignora el rango (200 completo)
// y la consulta por ID devuelve /dl/new.
func webDownloadServer(t *testing.T, oldHits, lookups *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/dl/old":
			atomic.AddInt32(oldHits, 1)
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/dl/new":
			var off int
			if rg := r.Header.Get("Range"); rg != "" {
				_, _ = fmt.Sscanf(rg, "bytes=%d-", &off)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(webTestContent)-1, len(webTestContent)))
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = io.WriteString(w, webTestContent[off:])
		case r.URL.Path == "/dl/norange":
			_, _ = io.WriteString(w, webTestContent)
		case r.URL.Path == "/sapi/media" && r.URL.Query().Get("action") == "get":
			atomic.AddInt32(lookups, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"data":{"media":[{"id":"42","name":"a.bin","size":%d,"url":"http://%s/dl/new"}]}}`, len(webTestContent), r.Host)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newWebTestClient(t *testing.T, base string) *Client {
	t.Helper()
	c, err := NewClient(config.AccountConfig{Provider: "movistar"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURLs(base+"/sapi", base)
	c.EnableWebSession(fakeWebSession{}, http.DefaultTransport)
	return c
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Una URL firmada caducada se renueva por ID, se reintenta y se recuerda para las
// lecturas siguientes (sin volver a probar la caducada).
func TestWebDownload_ExpiredURLIsRefreshedAndRemembered(t *testing.T) {
	var oldHits, lookups int32
	srv := webDownloadServer(t, &oldHits, &lookups)
	c := newWebTestClient(t, srv.URL)
	item := MediaItem{ID: "42", Name: "a.bin", Size: int64(len(webTestContent)), URL: srv.URL + "/dl/old"}

	rc, err := c.DownloadFileStreamRange(item, 10)
	if err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	if got := readAll(t, rc); got != webTestContent[10:] {
		t.Fatalf("contenido %q, esperado %q", got, webTestContent[10:])
	}

	rc, err = c.DownloadFileStreamRange(item, 20)
	if err != nil {
		t.Fatalf("segunda lectura: %v", err)
	}
	if got := readAll(t, rc); got != webTestContent[20:] {
		t.Fatalf("contenido %q, esperado %q", got, webTestContent[20:])
	}
	if oldHits != 1 || lookups != 1 {
		t.Errorf("esperada 1 petición a la URL caducada y 1 renovación; hubo %d y %d", oldHits, lookups)
	}
}

// Si el servidor ignora el rango (200 con el archivo completo) no se entregan datos
// del sitio equivocado.
func TestWebDownload_IgnoredRangeIsAnError(t *testing.T) {
	var oldHits, lookups int32
	srv := webDownloadServer(t, &oldHits, &lookups)
	c := newWebTestClient(t, srv.URL)
	// La renovación devuelve /dl/new, que sí respeta el rango: se debe acabar leyendo bien.
	item := MediaItem{ID: "42", Name: "a.bin", Size: int64(len(webTestContent)), URL: srv.URL + "/dl/norange"}

	rc, err := c.DownloadFileStreamRange(item, 5)
	if err != nil {
		t.Fatalf("lectura: %v", err)
	}
	if got := readAll(t, rc); got != webTestContent[5:] {
		t.Fatalf("contenido %q, esperado %q (nunca desde el byte 0)", got, webTestContent[5:])
	}
	if lookups != 1 {
		t.Errorf("esperada 1 renovación de URL, hubo %d", lookups)
	}
}
