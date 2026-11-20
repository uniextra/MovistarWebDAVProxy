package webdav

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// Si la sincronización del arranque falla, los vacíos no se cargan (no hay metadatos).
// La primera sincronización periódica buena los carga, y las siguientes no vuelven a
// descargar el manifiesto.
func TestBootstrapEmptyFilesIfPending(t *testing.T) {
	t.Setenv("WEBDAV_REAL_EMPTY_FILES", "false")
	manifestJSON := `{"version":1,"files":[{"path":"/Backups/Control/@writer/v1.cformat","mod_time":"20261116T200000Z"}]}`

	var descargas atomic.Int32
	var mockSAPI *httptest.Server
	mockSAPI = newShimServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/download/manifest") {
			descargas.Add(1)
			_, _ = w.Write([]byte(manifestJSON))
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer mockSAPI.Close()

	client, err := sapi.NewClient(config.AccountConfig{Provider: "movistar", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mockSAPI.URL+"/sapi", mockSAPI.URL)
	ms, err := cache.NewMetadataStore(filepath.Join(t.TempDir(), "pendiente.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()

	srv := NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true})
	srv.SetMetadataStore(ms)
	fs := srv.FS()

	// Base sin metadatos (la sincronización del arranque falló): no se puede cargar.
	fs.BootstrapEmptyFilesIfPending()
	if fs.emptyFileManager.bootstrapped.Load() {
		t.Fatal("sin metadatos no debería darse por cargado")
	}

	// Primera sincronización periódica buena: ya hay raíz, carpeta interna y manifiesto.
	_ = ms.SetInitialSync("cursor-1")
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 100, Name: "/", Magic: true})
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 150, Name: ProxyInternalDir, ParentID: 100})
	_ = ms.UpsertMedia(sapi.MediaItem{ID: "manifest-1", Name: ManifestFileName, FolderID: 150, Size: int64(len(manifestJSON)), URL: mockSAPI.URL + "/download/manifest"})
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 200, Name: "Backups", ParentID: 100})
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 300, Name: "Control", ParentID: 200})
	_ = ms.UpsertFolder(sapi.FolderItem{ID: 302, Name: "@writer", ParentID: 300})

	fs.BootstrapEmptyFilesIfPending()
	if vf, err := ms.GetVirtualEmptyFile(302, "v1.cformat"); err != nil || vf == nil {
		t.Fatalf("esperaba v1.cformat cargado, vf=%v err=%v", vf, err)
	}
	if n := descargas.Load(); n != 1 {
		t.Fatalf("esperaba 1 descarga del manifiesto, hubo %d", n)
	}

	// Las siguientes sincronizaciones no lo vuelven a cargar.
	fs.BootstrapEmptyFilesIfPending()
	fs.BootstrapEmptyFilesIfPending()
	if n := descargas.Load(); n != 1 {
		t.Fatalf("el manifiesto no debería volver a descargarse, hubo %d descargas", n)
	}
}
