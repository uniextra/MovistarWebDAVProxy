package cache

import (
	"path/filepath"
	"testing"

	"movistar-webdav-proxy/internal/sapi"
)

// El contenido diminuto se sirve solo para la misma versión del archivo: una subida
// (sin etag) o un etag distinto lo invalidan.
func TestTinyContent(t *testing.T) {
	s, err := NewMetadataStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	meta := []byte(`{"ver":1,"size":123,"nchunks":2}`)
	_ = s.UpsertMedia(sapi.MediaItem{ID: "1", Name: "x", Size: int64(len(meta)), ETag: "e1", FolderID: 5})
	s.PutTinyContent("1", "e1", meta)
	if b, ok := s.GetTinyContent("1", "e1", int64(len(meta))); !ok || string(b) != string(meta) {
		t.Fatal("debía servirse")
	}
	if _, ok := s.GetTinyContent("1", "e2", int64(len(meta))); ok {
		t.Fatal("otro etag no debe servirse")
	}
	// Relistado con el mismo etag: se conserva.
	_ = s.UpsertMedia(sapi.MediaItem{ID: "1", Name: "x", Size: int64(len(meta)), ETag: "e1", FolderID: 5})
	if _, ok := s.GetTinyContent("1", "e1", int64(len(meta))); !ok {
		t.Fatal("el mismo etag debe conservarlo")
	}
	// Subida (sin etag): se invalida aunque el etag guardado siga en la tabla media.
	_ = s.UpsertMedia(sapi.MediaItem{ID: "1", Name: "x", Size: int64(len(meta)), FolderID: 5})
	if _, ok := s.GetTinyContent("1", "e1", int64(len(meta))); ok {
		t.Fatal("tras una subida no debe servirse")
	}
	s.PutTinyContent("1", "e1", meta)
	_ = s.DeleteMedia("1")
	if _, ok := s.GetTinyContent("1", "e1", int64(len(meta))); ok {
		t.Fatal("tras borrar no debe servirse")
	}
}
