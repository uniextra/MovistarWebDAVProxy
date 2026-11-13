package sapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
)

// Clasificación comprobada con O2 Cloud (ver mediaKindForName).
func TestMediaKindForName(t *testing.T) {
	cases := map[string]string{
		"01 Canción.m4a": "audio", "pista.MP3": "audio", "toma.wav": "audio",
		"cap.mp4":  "video",
		"foto.JPG": "picture", "foto.jpeg": "picture", "foto.png": "picture", "anim.gif": "picture",
		"foto.webp": "picture", "foto.bmp": "picture", "scan.tif": "picture", "scan.tiff": "picture",
		// la app no los trata como multimedia: genéricos
		"pista.flac": "file", "pista.aac": "file", "pista.ogg": "file", "pista.opus": "file",
		"pista.wma": "file", "pista.aiff": "file", "cap.mkv": "file", "cap.mov": "file",
		"cap.m4v": "file", "cap.3gp": "file", "cap.avi": "file", "cap.webm": "file",
		"cap.wmv": "file", "cap.mpg": "file", "cap.ts": "file", "cap.flv": "file",
		"foto.heic": "file", "foto.heif": "file", "foto.avif": "file", "logo.svg": "file",
		"letra.lrc": "file", "notas.txt": "file", "pack": "file",
	}
	for name, want := range cases {
		if got := mediaKindForName(name); got != want {
			t.Errorf("mediaKindForName(%q) = %q, esperado %q", name, got, want)
		}
	}
}

// Cambiar la extensión de un audio: SAPI responde MED-1050 y no tiene sentido probar otra operación.
func TestUpdateMedia_ExtensionChangeRefusedIsFinal(t *testing.T) {
	c, calls := fakeSaveMetadataCode(t, "MED-1050")
	if err := c.UpdateMedia("1", "a.m4a", "a.m4a.otra", 200); err == nil || !strings.Contains(err.Error(), "MED-1050") {
		t.Fatalf("esperado error MED-1050, obtenido %v", err)
	}
	assertCalls(t, calls(), "audio")
}

// fakeSaveMetadataCode responde siempre con el error indicado.
func fakeSaveMetadataCode(t *testing.T, code string) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, strings.TrimPrefix(r.URL.Path, "/sapi/upload/"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"code": code, "message": "The file extension or the format is not supported."}})
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(config.AccountConfig{Provider: "movistar", Phone: "612345678", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.SetBaseURLs(srv.URL+"/sapi", srv.URL)
	return c, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
}

// fakeSaveMetadata simula SAPI: las operaciones de okKinds confirman; las demás
// responden HTTP 200 con un error en el cuerpo (MED-1005), como el servidor real.
// Las primeras validating respuestas de cualquier operación son MED-1017.
func fakeSaveMetadata(t *testing.T, validating int, okKinds ...string) (*Client, func() []string) {
	t.Helper()
	ok := map[string]bool{}
	for _, k := range okKinds {
		ok[k] = true
	}
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := strings.TrimPrefix(r.URL.Path, "/sapi/upload/")
		mu.Lock()
		calls = append(calls, kind)
		n := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case n <= validating:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"code": "MED-1017", "message": "Operation forbidden on a media not yet validated"}})
		case ok[kind]:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"code": "MED-1005", "message": "Unable to retrieve media with given id"}})
		}
	}))
	t.Cleanup(srv.Close)

	old := mediaMoveRetryDelay
	mediaMoveRetryDelay = time.Millisecond
	t.Cleanup(func() { mediaMoveRetryDelay = old })

	c, err := NewClient(config.AccountConfig{Provider: "movistar", Phone: "612345678", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.SetBaseURLs(srv.URL+"/sapi", srv.URL)
	return c, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
}

func assertCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("llamadas = %v, esperado %v", got, want)
	}
}

// m4a/mp3 (audio), mp4 (vídeo) y jpg/png (imagen): su propia operación, a la primera.
func TestUpdateMedia_TypedMediaUsesItsOperation(t *testing.T) {
	c, calls := fakeSaveMetadata(t, 0, "audio")
	if err := c.UpdateMedia("1", "01 Canción.m4a", "01 Canción.m4a", 200); err != nil {
		t.Fatalf("esperado éxito: %v", err)
	}
	assertCalls(t, calls(), "audio")
}

// Si la operación del tipo no corresponde (p. ej. la estimación es errónea), se usa la genérica.
func TestUpdateMedia_FallsBackToGenericOperation(t *testing.T) {
	c, calls := fakeSaveMetadata(t, 0, "file")
	if err := c.UpdateMedia("1", "foto.jpg", "foto.jpg", 200); err != nil {
		t.Fatalf("esperado éxito con la genérica: %v", err)
	}
	assertCalls(t, calls(), "picture", "file")
}

// El tipo sale del nombre actual: renombrar "a.m4a" a "a.m4a.bak" usa primero upload/audio.
func TestUpdateMedia_KindFromCurrentName(t *testing.T) {
	c, calls := fakeSaveMetadata(t, 0, "audio")
	if err := c.UpdateMedia("1", "a.m4a", "a.m4a.bak", 200); err != nil {
		t.Fatalf("esperado éxito: %v", err)
	}
	assertCalls(t, calls(), "audio")
}

// FLAC y MKV son genéricos para O2: una sola llamada.
func TestUpdateMedia_GenericFileOnlyOneCall(t *testing.T) {
	c, calls := fakeSaveMetadata(t, 0, "file")
	if err := c.UpdateMedia("1", "pista.flac", "pista.flac", 200); err != nil {
		t.Fatalf("esperado éxito: %v", err)
	}
	assertCalls(t, calls(), "file")
}

// Recién subido y aún en validación (MED-1017): se reintenta la misma operación.
func TestUpdateMedia_RetriesWhileValidating(t *testing.T) {
	c, calls := fakeSaveMetadata(t, 2, "file")
	if err := c.UpdateMedia("1", "letra.lrc", "letra.lrc", 200); err != nil {
		t.Fatalf("esperado éxito tras la validación: %v", err)
	}
	assertCalls(t, calls(), "file", "file", "file")
}

// Si ninguna operación lo confirma, se devuelve error (antes se daba por bueno).
func TestUpdateMedia_ErrorInBodyIsReported(t *testing.T) {
	c, _ := fakeSaveMetadata(t, 0)
	if err := c.UpdateMedia("1", "01 Canción.m4a", "01 Canción.m4a", 200); err == nil {
		t.Fatal("esperado error cuando SAPI responde con error en el cuerpo; se dio por bueno")
	}
}

// La espera ante MED-1017 crece con cada intento y se limita a mediaMoveMaxDelay.
func TestMediaMoveDelay_GrowsAndCaps(t *testing.T) {
	var total time.Duration
	for i := 0; i < mediaMoveRetries; i++ {
		d := mediaMoveDelay(i)
		if i > 0 && d < mediaMoveDelay(i-1) {
			t.Errorf("intento %d: %v menor que el anterior", i, d)
		}
		if d > mediaMoveMaxDelay {
			t.Errorf("intento %d: %v supera el máximo %v", i, d, mediaMoveMaxDelay)
		}
		total += d
	}
	if total < 60*time.Second {
		t.Errorf("espera total %v: debería cubrir al menos 60 s de validación", total)
	}
}
