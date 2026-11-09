package webdav

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestTmpCopyMove_ExistingDestinationIsNeverDeleted(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		for _, destination := range []string{"/backup.tmp", "http://example.com/backup.TMP", "/backup%2etmp", "/backup.tmp/.", "/backup.tmp/child/..", "/backup.tmp/%2e", "/backup.tmp/child/%2e%2e"} {
			for _, overwrite := range []string{"", "T", "F"} {
				t.Run(method+"/"+destination+"/"+overwrite, func(t *testing.T) {
					srv, _, closeBackend := servidorQueCuentaSubidas(t)
					defer closeBackend()
					base := newMockSAPIServer(t)
					defer base.Close()
					var deletes atomic.Int32
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Query().Get("action") == "softdelete" {
							deletes.Add(1)
						}
						if r.URL.Path == "/sapi/media" && r.URL.Query().Get("action") == "get" {
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": []map[string]interface{}{
								{"id": "555", "name": "hello.txt", "size": 11, "folderid": 100, "url": "/sapi/media/content?id=555"},
								{"id": "888", "name": "backup.tmp", "size": 11, "folderid": 100, "url": "/sapi/media/content?id=888"},
							}}})
							return
						}
						base.Config.Handler.ServeHTTP(w, r)
					}))
					defer backend.Close()
					srv.fs.client.SetBaseURLs(backend.URL+"/sapi", backend.URL)
					req := httptest.NewRequest(method, "/hello.txt", nil)
					req.Header.Set("Destination", destination)
					if overwrite != "" {
						req.Header.Set("Overwrite", overwrite)
					}
					rec := httptest.NewRecorder()
					srv.ServeHTTP(rec, req)
					if rec.Code != http.StatusUnsupportedMediaType || deletes.Load() != 0 {
						t.Fatalf("rechazo debe preceder a cualquier borrado: status=%d deletes=%d", rec.Code, deletes.Load())
					}
					for _, name := range []string{"hello.txt", "backup.tmp"} {
						get := httptest.NewRecorder()
						srv.ServeHTTP(get, httptest.NewRequest("GET", "/"+name, nil))
						if get.Code != http.StatusOK || get.Body.String() != "hello world" {
							t.Fatalf("%s alterado: status=%d body=%q", name, get.Code, get.Body.String())
						}
					}
				})
			}
		}
	}
}
