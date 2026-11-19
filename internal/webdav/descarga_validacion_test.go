package webdav

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// Un archivo recién subido no se puede descargar hasta que O2 termina de validarlo
// (la URL por defecto da COM-1005 y la firmada aún no existe). Prueba real del
// 19/11/2026: 3 GB, GET 53 s después de subir → 503 a los 45 s. Ahora la descarga
// espera a que termine la validación y la relectura, sin límite propio, y lee de la
// URL firmada a la primera.
func TestGet_RecienSubidoEsperaALaValidacion(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("WEBDAV_TMPDIR", t.TempDir())
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	// la espera de respaldo (45 s en real), a escala: más corta que la validación
	oldWait, oldPoll := reuploadSourceWait, reuploadSourcePoll
	reuploadSourceWait, reuploadSourcePoll = 300*time.Millisecond, 50*time.Millisecond
	defer func() { reuploadSourceWait, reuploadSourcePoll = oldWait, oldPoll }()

	payload := make([]byte, 2<<20) // por encima de la caché en memoria: se lee de O2
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	pendientes := 15 // consultas de validación que responden «V» (más que los 45 s de antes, a escala)
	var defaultHits, signedHits int
	var mock *httptest.Server
	mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_, _ = w.Write([]byte(`{"data":{"folders":[{"id":100,"name":"Root","parentid":0,"magic":true}]}}`))
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save-metadata":
			_, _ = w.Write([]byte(`{"success":"ok","id":"1597720466"}`))
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"success":"Media uploaded successfully","id":"1597720466","status":"V","etag":"x"}`))
		case r.URL.Path == "/sapi/media" && action == "get-validation-status":
			mu.Lock()
			st := "U"
			if pendientes > 0 {
				pendientes--
				st = "V"
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"ids": []map[string]interface{}{{"id": 1597720466, "status": st}}}})
		case r.URL.Path == "/sapi/media" && action == "get":
			mu.Lock()
			validado := pendientes == 0
			mu.Unlock()
			if !validado {
				_, _ = w.Write([]byte(`{"data":{"media":[{"id":"1597720466","name":"g.bin","size":2097152}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"media":[{"id":"1597720466","name":"g.bin","size":2097152,"url":"` + mock.URL + `/firmada"}]}}`))
		case r.URL.Path == "/firmada":
			mu.Lock()
			signedHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(payload)
		case strings.Contains(r.URL.Path, "/media/content"):
			mu.Lock()
			defaultHits++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"error":{"code":"COM-1005","message":"Unsupported operation"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	defer mock.Close()

	client, err := sapi.NewClient(config.AccountConfig{Provider: "o2", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"},
		sapi.WithValidationPolling(50*time.Millisecond, 100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})

	put := httptest.NewRequest("PUT", "/g.bin", bytes.NewReader(payload))
	put.ContentLength = int64(len(payload))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, put)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT = %d", rec.Code)
	}

	get := httptest.NewRecorder()
	srv.ServeHTTP(get, httptest.NewRequest("GET", "/g.bin", nil))
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), payload) {
		t.Fatalf("GET = %d, %d bytes (esperados %d)", get.Code, get.Body.Len(), len(payload))
	}
	mu.Lock()
	defer mu.Unlock()
	if defaultHits != 0 || signedHits != 1 {
		t.Errorf("URL por defecto %d veces (COM-1005), firmada %d; debía esperar a la validación y usar la firmada", defaultHits, signedHits)
	}
}

// Una copia en la nube (COPY) crea un archivo nuevo sin URL firmada: igual que tras
// una subida, el proxy lo sigue (validación y relectura) y una descarga inmediata
// espera y usa la URL firmada. Batería real del 19/11/2026: «copia.txt» bajaba
// primero por la URL por defecto y daba COM-1005.
func TestGet_CopiaRecienHechaUsaLaURLFirmada(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	oldWait, oldPoll := reuploadSourceWait, reuploadSourcePoll
	reuploadSourceWait, reuploadSourcePoll = 300*time.Millisecond, 50*time.Millisecond
	defer func() { reuploadSourceWait, reuploadSourcePoll = oldWait, oldPoll }()

	contenido := []byte("contenido de la copia")
	var mu sync.Mutex
	pendientes := 3
	copiado := false
	var defaultHits, signedHits int
	var mock *httptest.Server
	mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		mu.Lock()
		defer mu.Unlock()
		item := func(id, name string, folder int, url string) map[string]interface{} {
			m := map[string]interface{}{"id": id, "name": name, "size": len(contenido), "folderid": folder,
				"creationdate": "20260101T000000Z", "modificationdate": "20260101T000000Z"}
			if url != "" {
				m["url"] = url
			}
			return m
		}
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}}}})
		case r.URL.Path == "/sapi/media/folder" && action == "list":
			folders := []interface{}{}
			if r.URL.Query().Get("parentid") == "100" {
				folders = append(folders, map[string]interface{}{"id": 200, "name": "dst", "parentid": 100})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"folders": folders}})
		case r.URL.Path == "/sapi/media/file" && action == "copy":
			copiado = true
			_, _ = io.WriteString(w, `{"data":{"id":"999"}}`)
		case r.URL.Path == "/sapi/media" && action == "get-validation-status":
			st := "U"
			if pendientes > 0 {
				pendientes--
				st = "V"
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"ids": []map[string]interface{}{{"id": 999, "status": st}}}})
		case r.URL.Path == "/sapi/media" && action == "get":
			items := []interface{}{}
			switch {
			case r.URL.Query().Get("folderid") == "100":
				items = append(items, item("555", "a.bin", 100, mock.URL+"/original"))
			case r.URL.Query().Get("folderid") == "200":
				if copiado {
					items = append(items, item("999", "b.bin", 200, ""))
				}
			default: // relectura por ID (como la app)
				url := ""
				if pendientes == 0 {
					url = mock.URL + "/firmada"
				}
				items = append(items, item("999", "b.bin", 200, url))
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": items}})
		case r.URL.Path == "/firmada":
			signedHits++
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(contenido)
		case strings.Contains(r.URL.Path, "/media/content") || strings.Contains(r.URL.Path, "/download/"):
			defaultHits++
			_, _ = io.WriteString(w, `{"error":{"code":"COM-1005","message":"Unsupported operation"}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer mock.Close()

	client, err := sapi.NewClient(config.AccountConfig{Provider: "o2", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"},
		sapi.WithValidationPolling(50*time.Millisecond, 100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, copyReq("/dst/b.bin"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("COPY = %d", rec.Code)
	}
	get := httptest.NewRecorder()
	srv.ServeHTTP(get, httptest.NewRequest("GET", "/dst/b.bin", nil))
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), contenido) {
		t.Fatalf("GET = %d, %q", get.Code, get.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if defaultHits != 0 || signedHits != 1 {
		t.Errorf("URL por defecto %d veces (COM-1005), firmada %d; debía seguir la copia y usar la firmada", defaultHits, signedHits)
	}
}
