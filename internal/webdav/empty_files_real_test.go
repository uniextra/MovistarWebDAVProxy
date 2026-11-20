package webdav

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"movistar-webdav-proxy/internal/sapi"
)

// Con WEBDAV_REAL_EMPTY_FILES (por defecto), un archivo de 0 bytes se sube a la nube
// como cualquier otro: no queda emulado en SQLite y se lee vacío.
func TestRealEmptyFiles_PutUploadsToCloud(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()
	t.Setenv("WEBDAV_REAL_EMPTY_FILES", "")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("PUT", "/Backups/vacio.cinfo", bytes.NewReader(nil)))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("PUT de 0 bytes: %d %s", rec.Code, rec.Body.String())
	}
	if vf, _ := ms.GetVirtualEmptyFile(200, "vacio.cinfo"); vf != nil {
		t.Fatal("no debe quedar emulado en SQLite")
	}
	m, err := ms.GetMediaByFolderAndName(200, "vacio.cinfo")
	if err != nil || m == nil || m.Size != 0 {
		t.Fatalf("debe estar en la nube como archivo de 0 bytes: %+v, %v", m, err)
	}

	get := httptest.NewRecorder()
	srv.ServeHTTP(get, httptest.NewRequest("GET", "/Backups/vacio.cinfo", nil))
	if body, _ := io.ReadAll(get.Body); get.Code != http.StatusOK || len(body) != 0 {
		t.Fatalf("GET: %d, %d bytes", get.Code, len(body))
	}
}

// Los vacíos de bloqueo de Hyper Backup viven segundos: siguen emulados.
func TestRealEmptyFiles_LockFilesStayEmulated(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()
	t.Setenv("WEBDAV_REAL_EMPTY_FILES", "")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("PUT", "/Backups/lock_123", bytes.NewReader(nil)))
	if rec.Code >= 300 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if vf, _ := ms.GetVirtualEmptyFile(200, "lock_123"); vf == nil {
		t.Fatal("el lock debe seguir emulado")
	}
	if m, _ := ms.GetMediaByFolderAndName(200, "lock_123"); m != nil {
		t.Fatalf("el lock no debe subirse: %+v", m)
	}
}

// Los vacíos que seguían emulados (de antes de activarlo, o del manifiesto) se suben
// a la nube; si ya hay un archivo real con ese nombre, solo se quita el emulado. Los
// de bloqueo no se tocan.
func TestRealEmptyFiles_MigratesEmulated(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()
	t.Setenv("WEBDAV_REAL_EMPTY_FILES", "")

	const mod = "20260101T000000Z"
	for _, name := range []string{"v1.cformat", "ya-real.txt", "lock_9"} {
		if err := ms.UpsertVirtualEmptyFile(200, name, mod); err != nil {
			t.Fatal(err)
		}
	}
	// ya-real.txt ya está en la nube (subido en un arranque anterior); el emulado es
	// como si lo hubiera resucitado un manifiesto viejo.
	if err := ms.UpsertMedia(sapi.MediaItem{ID: "777", Name: "ya-real.txt", Size: 1, FolderID: 200}); err != nil {
		t.Fatal(err)
	}

	srv.fs.emptyFileManager.migrateToCloud()

	if vf, _ := ms.GetVirtualEmptyFile(200, "v1.cformat"); vf != nil {
		t.Fatal("v1.cformat debe dejar de estar emulado")
	}
	if m, _ := ms.GetMediaByFolderAndName(200, "v1.cformat"); m == nil {
		t.Fatal("v1.cformat debe estar en la nube")
	}
	if vf, _ := ms.GetVirtualEmptyFile(200, "ya-real.txt"); vf != nil {
		t.Fatal("ya-real.txt: debe quitarse el emulado")
	}
	if m, _ := ms.GetMediaByFolderAndName(200, "ya-real.txt"); m == nil || m.Size != 1 {
		t.Fatalf("ya-real.txt no debe sustituirse por uno vacío: %+v", m)
	}
	if vf, _ := ms.GetVirtualEmptyFile(200, "lock_9"); vf == nil {
		t.Fatal("el lock debe seguir emulado")
	}
}

// Con WEBDAV_REAL_EMPTY_FILES=false, la emulación de siempre.
func TestRealEmptyFiles_OptOut(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("PUT", "/Backups/vacio.txt", bytes.NewReader(nil)))
	if rec.Code >= 300 {
		t.Fatalf("PUT: %d", rec.Code)
	}
	if vf, _ := ms.GetVirtualEmptyFile(200, "vacio.txt"); vf == nil {
		t.Fatal("con la opción desactivada debe emularse")
	}
	if m, _ := ms.GetMediaByFolderAndName(200, "vacio.txt"); m != nil {
		t.Fatalf("no debe subirse: %+v", m)
	}
}

// Vaciar un archivo con contenido: O2 no admite sobrescribirlo con 0 bytes
// (MED-1049), así que se sube uno nuevo vacío y el anterior va a la papelera.
func TestRealEmptyFiles_EmptyingExistingUploadsNew(t *testing.T) {
	srv, ms, _, cleanup := setupTestWebDAVServerWithDB(t)
	defer cleanup()
	t.Setenv("WEBDAV_REAL_EMPTY_FILES", "")
	if err := ms.UpsertMedia(sapi.MediaItem{ID: "777", Name: "lleno.txt", Size: 4, FolderID: 200}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("PUT", "/Backups/lleno.txt", bytes.NewReader(nil)))
	if rec.Code >= 300 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if old, _ := ms.GetMediaByID("777"); old != nil {
		t.Fatalf("el anterior debe ir a la papelera: %+v", old)
	}
	m, _ := ms.GetMediaByFolderAndName(200, "lleno.txt")
	if m == nil || m.ID == "777" || m.Size != 0 {
		t.Fatalf("debe quedar uno nuevo de 0 bytes: %+v", m)
	}
}
