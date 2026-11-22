package webdav

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// folderCloud simula SAPI con creación de carpetas lenta (300 ms), como cuando
// varias transferencias crean a la vez una carpeta nueva y su subcarpeta.
func folderCloud(t *testing.T) *MovistarFS {
	t.Helper()
	var mu sync.Mutex
	nextID := int64(200)
	children := map[string][]map[string]interface{}{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}}}})
		case r.URL.Path == "/sapi/media/folder" && action == "list":
			mu.Lock()
			list := append([]map[string]interface{}{}, children[r.URL.Query().Get("parentid")]...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"folders": list}})
		case r.URL.Path == "/sapi/media/folder" && action == "save":
			var body struct {
				Data struct {
					Name     string `json:"name"`
					ParentID int64  `json:"parentid"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			time.Sleep(300 * time.Millisecond)
			mu.Lock()
			nextID++
			f := map[string]interface{}{"id": nextID, "name": body.Data.Name, "parentid": body.Data.ParentID}
			key := strconv.FormatInt(body.Data.ParentID, 10)
			children[key] = append(children[key], f)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"folders": []interface{}{f}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"media": []interface{}{}}})
		}
	}))
	t.Cleanup(mock.Close)
	client, err := sapi.NewClient(config.AccountConfig{Provider: "movistar", Phone: "612345678", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	return NewMovistarFS(client)
}

// Un MKCOL de la subcarpeta que llega mientras se crea la carpeta madre espera a
// que termine, en vez de responder que no existe.
func TestMkdir_WaitsForParentBeingCreated(t *testing.T) {
	mfs := folderCloud(t)
	ctx := context.Background()
	parentDone := make(chan error, 1)
	go func() { parentDone <- mfs.Mkdir(ctx, "/serie", 0o755) }()
	time.Sleep(50 * time.Millisecond) // la madre ya está creándose
	if err := mfs.Mkdir(ctx, "/serie/temporada 1", 0o755); err != nil {
		t.Fatalf("la subcarpeta no esperó a la madre: %v", err)
	}
	if err := <-parentDone; err != nil {
		t.Fatalf("madre: %v", err)
	}
}

// Sin nadie creándola, una madre inexistente sigue siendo «no existe» (409), que
// el cliente resuelve creándola, y no se registra como error.
func TestMkdir_MissingParentStillNotExist(t *testing.T) {
	mfs := folderCloud(t)
	err := mfs.Mkdir(context.Background(), "/no/existe", 0o755)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("esperado ErrNotExist, obtenido %v", err)
	}
	if !isExpectedNotFound("MKCOL", err) {
		t.Error("un MKCOL sin madre no debe registrarse como error")
	}
}
