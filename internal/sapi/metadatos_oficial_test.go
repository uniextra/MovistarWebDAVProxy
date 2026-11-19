package sapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type capturedCall struct {
	Method, Path, Query, ContentType string
	Body                             map[string]interface{}
	RawBody                          string
}

func captureCalls(t *testing.T, reply func(c capturedCall) string) (*Client, func() []capturedCall) {
	t.Helper()
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	var mu sync.Mutex
	var calls []capturedCall
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c := capturedCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, ContentType: r.Header.Get("Content-Type"), RawBody: string(b)}
		var body struct {
			Data map[string]interface{} `json:"data"`
		}
		if json.Unmarshal(b, &body) == nil {
			c.Body = body.Data
		}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		_, _ = w.Write([]byte(reply(c)))
	})
	// lo que se compara es lo que hace la app de O2 (con Movistar no se usa por defecto)
	client.mu.Lock()
	client.cfg.Provider = "o2"
	client.mu.Unlock()
	return client, func() []capturedCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedCall(nil), calls...)
	}
}

func assertKeys(t *testing.T, what string, body map[string]interface{}, keys ...string) {
	t.Helper()
	if len(body) != len(keys) {
		t.Errorf("%s: campos %v, la app manda exactamente %v", what, body, keys)
	}
	for _, k := range keys {
		if _, ok := body[k]; !ok {
			t.Errorf("%s: falta %q en %v", what, k, body)
		}
	}
}

func TestOfficialRenameMoveFile_SingleCallLikeTheApp(t *testing.T) {
	client, calls := captureCalls(t, func(capturedCall) string { return `{"success":"Media metadata saved successfully"}` })
	item := MediaItem{ID: "1597720466", Name: "IMG_4028.HEIC", FolderID: 1, ModificationDate: "20261118T071500Z"}
	if err := client.UpdateMediaItem(item, "IMG_4028-nuevonombre.HEIC", 37224270); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("llamadas = %d; mover y renombrar es una sola llamada", len(got))
	}
	c := got[0]
	if c.Path != "/sapi/upload/picture" || !strings.Contains(c.Query, "action=save-metadata") {
		t.Fatalf("petición = %s?%s", c.Path, c.Query)
	}
	assertKeys(t, "save-metadata de renombrar/mover", c.Body, "contenttype", "folderid", "id", "modificationdate", "name")
	if c.Body["modificationdate"] != "20261118T071500Z" {
		t.Errorf("renombrar no cambia la fecha del archivo: %v", c.Body["modificationdate"])
	}
	if ct, _ := c.Body["contenttype"].(string); ct != "image/heic" {
		t.Errorf("contenttype = %v", c.Body["contenttype"])
	}
}

func TestOfficialCreateFolder_PayloadLikeTheApp(t *testing.T) {
	client, calls := captureCalls(t, func(capturedCall) string { return `{"id":37244653}` })
	f, err := client.CreateFolder("NAS_HA-Copias.hbk", 36513996)
	if err != nil || f.ID != 37244653 {
		t.Fatalf("CreateFolder = %v, %v", f, err)
	}
	c := calls()[0]
	if c.Path != "/sapi/media/folder" || !strings.Contains(c.Query, "action=save") || c.ContentType != "application/json" {
		t.Fatalf("petición = %s?%s (%s)", c.Path, c.Query, c.ContentType)
	}
	assertKeys(t, "crear carpeta", c.Body, "creationdate", "magic", "modificationdate", "name", "parentid")
	if c.Body["magic"] != false {
		t.Errorf("magic = %v", c.Body["magic"])
	}
}

func TestOfficialRenameFolder_PayloadLikeTheApp(t *testing.T) {
	client, calls := captureCalls(t, func(capturedCall) string { return `{"success":"ok"}` })
	folder := FolderItem{ID: 37244654, Name: "Control", ParentID: 37244653, CreationDate: "20261118T153010Z"}
	if err := client.UpdateFolderItem(folder, "Control2", 37244658); err != nil {
		t.Fatal(err)
	}
	c := calls()[0]
	assertKeys(t, "renombrar/mover carpeta", c.Body, "creationdate", "id", "magic", "modificationdate", "name", "parentid")
	if c.Body["creationdate"] != "20261118T153010Z" {
		t.Errorf("la fecha de creación de la carpeta se conserva: %v", c.Body["creationdate"])
	}
}

func TestOfficialDeleteFile_LikeTheApp(t *testing.T) {
	client, calls := captureCalls(t, func(capturedCall) string { return `"Item successfully moved to trash"` })
	if err := client.DeleteFile("1597720466", "corte.bin"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("llamadas = %+v", got)
	}
	c := got[0]
	if c.Path != "/sapi/media" || !strings.Contains(c.Query, "action=softdelete") || !strings.Contains(c.Query, "id=1597720466") {
		t.Fatalf("petición = %s?%s", c.Path, c.Query)
	}
	if c.RawBody != "" || c.ContentType != "application/x-www-form-urlencoded; charset=UTF-8" {
		t.Fatalf("cuerpo %q, content-type %q", c.RawBody, c.ContentType)
	}
}

func TestOfficialDeleteFile_FallsBackWhenRejected(t *testing.T) {
	client, calls := captureCalls(t, func(c capturedCall) string {
		if c.Path == "/sapi/media" {
			return `{"error":{"code":"COM-1011","message":"Missing required parameter"}}`
		}
		return `{"success":"ok"}`
	})
	if err := client.DeleteFile("1597720466", "corte.bin"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || got[1].Path != "/sapi/media/file" {
		t.Fatalf("se esperaba la llamada de la app y luego la alternativa: %+v", got)
	}
}

func TestOfficialDeleteFolder_LikeTheApp(t *testing.T) {
	client, calls := captureCalls(t, func(capturedCall) string { return `{"success":"Folders have been softdeleted"}` })
	if err := client.DeleteFolder(37244653, false); err != nil {
		t.Fatal(err)
	}
	c := calls()[0]
	if c.Path != "/sapi/media/folder" || !strings.Contains(c.Query, "action=softdelete") || !strings.Contains(c.Query, "id=37244653") || c.RawBody != "" {
		t.Fatalf("petición = %s?%s cuerpo %q", c.Path, c.Query, c.RawBody)
	}
}
