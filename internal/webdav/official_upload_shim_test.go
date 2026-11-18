package webdav

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// newShimServer es httptest.NewServer con officialUploadShim delante: los
// simuladores de SAPI de estas pruebas hablan la subida de la web (multipart a
// /sapi/upload?action=save) y el perfil desktop sube como la app oficial
// (save-metadata + contenido en bruto a /sapi/upload/<tipo>?action=save).
func newShimServer(h http.Handler) *httptest.Server {
	return httptest.NewServer(officialUploadShim(h))
}

// officialUploadShim traduce la subida de la app oficial a la de la web para el
// simulador que envuelve. El resto de peticiones pasan tal cual.
func officialUploadShim(next http.Handler) http.Handler {
	var mu sync.Mutex
	seq := 0
	pending := map[string]map[string]interface{}{}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		if !strings.HasPrefix(r.URL.Path, "/sapi/upload/") {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case r.Method == http.MethodPost && action == "save-metadata" && r.Header.Get("Content-Type") == "application/octet-stream":
			var body struct {
				Data map[string]interface{} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body.Data["size"]; !ok {
				// save-metadata de renombrar/mover: lo atiende el simulador.
				r.Body = io.NopCloser(bytes.NewReader(mustJSON(body)))
				next.ServeHTTP(w, r)
				return
			}
			mu.Lock()
			key := fmt.Sprint(body.Data["id"])
			if body.Data["id"] == nil {
				seq++
				key = fmt.Sprintf("shim-%d", seq)
			}
			pending[key] = body.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": "Media metadata saved successfully", "id": key})
		case r.Method == http.MethodGet && action == "save":
			w.WriteHeader(http.StatusPermanentRedirect)
		case r.Method == http.MethodPost && action == "save":
			id := r.Header.Get("x-funambol-id")
			mu.Lock()
			meta := pending[id]
			mu.Unlock()
			if meta == nil {
				http.Error(w, `{"error":{"code":"MED-1000","message":"unknown id"}}`, http.StatusOK)
				return
			}
			content, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			data := map[string]interface{}{}
			for k, v := range meta {
				data[k] = v
			}
			if strings.HasPrefix(id, "shim-") {
				delete(data, "id")
			}
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, _ := mw.CreateFormField("data")
			_, _ = fw.Write(mustJSON(map[string]interface{}{"data": data}))
			ff, _ := mw.CreateFormFile("file", fmt.Sprint(data["name"]))
			_, _ = ff.Write(content)
			_ = mw.Close()

			inner, _ := http.NewRequest(http.MethodPost, "/sapi/upload?"+r.URL.RawQuery, &buf)
			inner.Header = r.Header.Clone()
			inner.Header.Set("Content-Type", mw.FormDataContentType())
			inner.ContentLength = int64(buf.Len())
			q := inner.URL.Query()
			q.Set("action", "save")
			inner.URL.RawQuery = q.Encode()
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, inner)

			var res struct {
				ID    interface{}            `json:"id"`
				Data  map[string]interface{} `json:"data"`
				Error interface{}            `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &res)
			if rec.Code != http.StatusOK || res.Error != nil {
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
				return
			}
			newID := res.ID
			if newID == nil && res.Data != nil {
				newID = res.Data["id"]
			}
			if newID == nil {
				newID = id
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": "Media uploaded successfully", "id": fmt.Sprint(newID), "status": "U", "etag": "shim",
			})
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
