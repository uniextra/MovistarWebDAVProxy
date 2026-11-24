package sapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsGalleryMediaName(t *testing.T) {
	valid := []string{
		"foto.jpg", "FOTO.JPEG", "img.png", "anim.gif", "pic.webp",
		"raw.bmp", "scan.tif", "scan.tiff", "iphone.heic",
		"clip.mp4", "CLIP.MOV", "vid.m4v", "old.3gp", "cam.avi", "movie.mkv",
	}
	for _, name := range valid {
		if !IsGalleryMediaName(name) {
			t.Errorf("IsGalleryMediaName(%q) = false, esperado true", name)
		}
	}

	invalid := []string{
		"notas.txt", "doc.pdf", "backup.zip", "song.mp3", "apple.heif", "sin_extension", "",
	}
	for _, name := range invalid {
		if IsGalleryMediaName(name) {
			t.Errorf("IsGalleryMediaName(%q) = true, esperado false", name)
		}
	}
}

func TestGalleryUpload_OfficialOmitFolderIDAndPreserveCreationDate(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)

	mod := time.Date(2025, 8, 15, 12, 30, 45, 0, time.UTC)
	jpgData := []byte("fake-jpeg-bytes")
	item, err := c.UploadFileToFolderCtx(context.Background(), "vacaciones.JPG", bytes.NewReader(jpgData), int64(len(jpgData)), 0, "", "", mod)
	if err != nil {
		t.Fatalf("subida foto galería: %v", err)
	}
	if item.FolderID != 0 {
		t.Errorf("FolderID = %d, esperado 0", item.FolderID)
	}
	if got := item.CreationDate; got != "20250815T123045Z" {
		t.Errorf("CreationDate = %q, esperado 20250815T123045Z", got)
	}
	if len(f.metaBodies) != 1 || len(f.postKinds) != 1 {
		t.Fatalf("metaBodies=%d postKinds=%d", len(f.metaBodies), len(f.postKinds))
	}
	if f.postKinds[0] != "picture" {
		t.Errorf("kind[0] = %q, esperado picture", f.postKinds[0])
	}
	if _, hasFolder := f.metaBodies[0]["folderid"]; hasFolder {
		t.Errorf("save-metadata de galería no debe enviar folderid: %v", f.metaBodies[0])
	}
	if f.metaBodies[0]["creationdate"] != "20250815T123045Z" {
		t.Errorf("creationdate en save-metadata = %v", f.metaBodies[0]["creationdate"])
	}

	// Vídeo .mov: en carpetas va como "file", en galería debe ir como "video" con MIME video/quicktime.
	movData := []byte("fake-mov-bytes")
	_, err = c.UploadFileToFolderCtx(context.Background(), "video.MOV", bytes.NewReader(movData), int64(len(movData)), 0, "", "", mod)
	if err != nil {
		t.Fatalf("subida vídeo galería: %v", err)
	}
	if f.postKinds[1] != "video" {
		t.Errorf("kind[1] = %q, esperado video", f.postKinds[1])
	}
	if _, hasFolder := f.metaBodies[1]["folderid"]; hasFolder {
		t.Errorf("save-metadata vídeo galería no debe enviar folderid: %v", f.metaBodies[1])
	}
	if f.metaBodies[1]["contenttype"] != "video/quicktime" {
		t.Errorf("contenttype de .MOV = %v, esperado video/quicktime", f.metaBodies[1]["contenttype"])
	}
}

func TestGalleryUpload_RejectsNonMediaAndNoGenericFallback(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	t.Setenv("SAPI_UNSUPPORTED_AS_FILE", "true")

	// 1. Archivo no multimedia rechazado localmente sin llamar a SAPI
	_, err := c.UploadFileToFolderCtx(context.Background(), "notas.txt", bytes.NewReader([]byte("hola")), 4, 0, "", "", time.Now())
	if !errors.Is(err, ErrMediaUnsupported) {
		t.Fatalf("esperado ErrMediaUnsupported para .txt, recibido %v", err)
	}
	if len(f.metaBodies) != 0 {
		t.Fatalf("no debía llamar a save-metadata para .txt: %v", f.metaBodies)
	}

	// 2. Foto rechazada por O2 con MED-1050 en galería NO debe reintentarse como upload/file
	f.reject["picture"] = "MED-1050"
	_, err = c.UploadFileToFolderCtx(context.Background(), "rota.jpg", bytes.NewReader([]byte("bad")), 3, 0, "", "", time.Now())
	if !errors.Is(err, ErrMediaUnsupported) {
		t.Fatalf("esperado ErrMediaUnsupported ante MED-1050, recibido %v", err)
	}
	for _, k := range f.postKinds {
		if k == "file" {
			t.Fatalf("la galería nunca debe hacer fallback a upload/file: postKinds=%v", f.postKinds)
		}
	}
}

func TestGalleryUpload_NonOfficialOmitsFolderIDOnlyForZero(t *testing.T) {
	var captured []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("Content-Type inválido: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("NextPart: %v", err)
			}
			b, _ := io.ReadAll(part)
			if part.FormName() == "data" {
				var body struct {
					Data map[string]interface{} `json:"data"`
				}
				if err := json.Unmarshal(b, &body); err != nil {
					t.Fatalf("JSON meta inválido: %v", err)
				}
				captured = append(captured, body.Data)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":"Media uploaded successfully","id":"9999"}`))
	}))
	defer srv.Close()

	c := newOfficialTestClient(t, srv)
	t.Setenv("SAPI_OFFICIAL_CLIENT", "false")

	_, err := c.UploadFileToFolder("gal.jpg", bytes.NewReader([]byte("123")), 3, 0, "image/jpeg", "", time.Now())
	if err != nil {
		t.Fatalf("UploadFileToFolder(0): %v", err)
	}
	_, err = c.UploadFileToFolder("doc.jpg", bytes.NewReader([]byte("123")), 3, 42, "image/jpeg", "", time.Now())
	if err != nil {
		t.Fatalf("UploadFileToFolder(42): %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("capturadas %d subidas, esperadas 2", len(captured))
	}
	if _, has := captured[0]["folderid"]; has {
		t.Errorf("folderID=0 no debía enviar folderid: %v", captured[0])
	}
	if got, _ := captured[1]["folderid"].(float64); int64(got) != 42 {
		t.Errorf("folderID=42 debía enviar folderid=42, recibido %v", captured[1])
	}
}
