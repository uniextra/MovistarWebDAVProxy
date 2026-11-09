package webdav

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// SAPI rechaza .tmp (issue #13): no prometer almacenamiento ni truncar a 10 MiB.
func TestTmpPUT_NoSimulatedSuccess(t *testing.T) {
	for _, name := range []string{"backup.tmp", "duplicati-access-privileges-test.tmp", "Duplicati-other.TMP", "backup.tmp/.", "backup.tmp/child/..", "backup.tmp/%2e"} {
		for _, body := range []string{"small", "large", "read-error", "short"} {
			t.Run(name+"/"+body, func(t *testing.T) {
				srv, uploads, closeBackend := servidorQueCuentaSubidas(t)
				defer closeBackend()
				var reader io.Reader = strings.NewReader("backup data")
				switch body {
				case "large":
					reader = bytes.NewReader(bytes.Repeat([]byte("x"), 10*1024*1024+1))
				case "read-error":
					reader = &cuerpoQueSeCorta{}
				}
				req := httptest.NewRequest(http.MethodPut, "/"+name, reader)
				if body == "short" {
					req.ContentLength = 1000
				}
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnsupportedMediaType {
					t.Errorf("PUT sin persistencia: esperado 415, obtenido %d", rec.Code)
				}
				if uploads.Load() != 0 {
					t.Errorf("no se deben enviar extensiones no soportadas a SAPI")
				}
				// Comprobar tanto la misma instancia como una nueva con caché vacía.
				for _, server := range []*Server{srv, NewServer(srv.fs.client)} {
					get := httptest.NewRecorder()
					server.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/"+name, nil))
					if get.Code != http.StatusNotFound {
						t.Errorf("GET tras PUT rechazado: esperado 404, obtenido %d", get.Code)
					}
				}
			})
		}
	}
}

func TestTmpFileSystem_NoVolatileWriter(t *testing.T) {
	srv, _, closeBackend := servidorQueCuentaSubidas(t)
	defer closeBackend()
	file, err := srv.fs.OpenFile(context.Background(), "/backup.tmp", os.O_CREATE|os.O_WRONLY, 0600)
	if err == nil {
		_ = file.Close()
		t.Fatal("OpenFile no debe ofrecer un escritor volátil que simula persistencia")
	}
}

func TestTmpCopyMove_NoSuccessToUnsupportedDestination(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		t.Run(method, func(t *testing.T) {
			srv, uploads, closeBackend := servidorQueCuentaSubidas(t)
			defer closeBackend()
			req := httptest.NewRequest(method, "/hello.txt", nil)
			req.Header.Set("Destination", "/backup.tmp")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code < 400 || uploads.Load() != 0 {
				t.Fatalf("%s a .tmp: status=%d uploads=%d", method, rec.Code, uploads.Load())
			}
			get := httptest.NewRecorder()
			srv.ServeHTTP(get, httptest.NewRequest("GET", "/hello.txt", nil))
			if get.Code != http.StatusOK || get.Body.String() != "hello world" {
				t.Fatalf("%s no debe alterar el origen: status=%d body=%q", method, get.Code, get.Body.String())
			}
		})
	}
}
