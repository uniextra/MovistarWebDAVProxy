package sapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

// También en modo normal (sin WebMode) una URL firmada caducada (403) se renueva por ID.
func TestNormalDownload_ExpiredURLIsRefreshedAndRemembered(t *testing.T) {
	var oldHits, lookups int32
	srv := webDownloadServer(t, &oldHits, &lookups)
	c, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "a",
		RefreshToken:  "r",
		ValidationKey: "vk",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURLs(srv.URL+"/sapi", srv.URL)
	item := MediaItem{ID: "42", Name: "a.bin", Size: int64(len(webTestContent)), URL: srv.URL + "/dl/old"}

	rc, err := c.DownloadFileStreamRange(item, 0)
	if err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	if got := readAll(t, rc); got != webTestContent {
		t.Fatalf("contenido %q, esperado %q", got, webTestContent)
	}

	rc, err = c.DownloadFileStreamRange(item, 10)
	if err != nil {
		t.Fatalf("segunda lectura: %v", err)
	}
	if got := readAll(t, rc); got != webTestContent[10:] {
		t.Fatalf("contenido %q, esperado %q", got, webTestContent[10:])
	}
	if oldHits != 1 || lookups != 1 {
		t.Errorf("esperada 1 petición a la URL caducada y 1 renovación; hubo %d y %d", oldHits, lookups)
	}
}

// Un error transitorio de subida (MED-1048) reintenta rebobinando el cuerpo y acaba completando la subida.
func TestUploadFileToFolder_TransientMED1048RetriesAndSucceeds(t *testing.T) {
	// Subida de la web (multipart); la del perfil desktop se prueba en upload_oficial_test.go.
	t.Setenv("SAPI_CLIENT_PROFILE", "browser")
	const payload = "test-payload-bytes-0123456789abcdef"
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sapi/upload" || r.URL.Query().Get("action") != "save" {
			http.NotFound(w, r)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("attempt %d: FormFile error: %v", attempts.Load()+1, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotBytes, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil || string(gotBytes) != payload {
			t.Errorf("attempt %d: uploaded file bytes %q, want %q (err=%v)", attempts.Load()+1, string(gotBytes), payload, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"error":{"code":"MED-1048","message":"Multipart upload request without valid binary"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"998877","name":"bucket.bin"}`)
	}))
	defer srv.Close()

	c, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "a",
		RefreshToken:  "r",
		ValidationKey: "vk",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	item, err := c.UploadFileToFolder("bucket.bin", bytes.NewReader([]byte(payload)), int64(len(payload)), 100, "application/octet-stream", "", time.Time{})
	if err != nil {
		t.Fatalf("expected MED-1048 retry to succeed: %v", err)
	}
	if item == nil || item.ID != "998877" {
		t.Fatalf("unexpected item: %+v", item)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 upload attempts, got %d", got)
	}
}
