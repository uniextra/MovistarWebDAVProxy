package cache

import (
	"testing"

	"movistar-webdav-proxy/internal/sapi"
)

func TestListGalleryMedia(t *testing.T) {
	store, _ := createTempStore(t)

	items := []sapi.MediaItem{
		{ID: "200", Name: "foto.jpg", Size: 200, FolderID: 0, CreationDate: "20261120T100000Z"},
		{ID: "100", Name: "foto.jpg", Size: 100, FolderID: 0, CreationDate: "20261120T100000Z"},
		{ID: "300", Name: "video.mp4", Size: 300, FolderID: 0, CreationDate: "20261121T100000Z"},
		{ID: "400", Name: "en_carpeta.jpg", Size: 400, FolderID: 42, CreationDate: "20261121T100000Z"},
	}
	for _, it := range items {
		if err := store.UpsertMedia(it); err != nil {
			t.Fatalf("UpsertMedia(%s): %v", it.ID, err)
		}
	}

	got, err := store.ListGalleryMedia()
	if err != nil {
		t.Fatalf("ListGalleryMedia: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("esperados 3 elementos de galería (con duplicados y sin folder_id>0), recibidos %d: %+v", len(got), got)
	}
	if got[0].ID != "100" || got[1].ID != "200" || got[2].ID != "300" {
		t.Fatalf("orden determinista inesperado: [%s, %s, %s]", got[0].ID, got[1].ID, got[2].ID)
	}
}
