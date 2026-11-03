package webdav

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func TestParseOCMtime(t *testing.T) {
	cases := []struct {
		raw  string
		ok   bool
		want int64
	}{
		{"1700000000", true, 1700000000},
		{" 1700000000 ", true, 1700000000},
		{"1700000000.75", true, 1700000000},
		{"", false, 0},
		{"abc", false, 0},
		{"0", false, 0},
		{"-5", false, 0},
		{"1700000000000", false, 0}, // milisegundos por error: fuera de rango
	}
	for _, c := range cases {
		got, ok := parseOCMtime(c.raw)
		if ok != c.ok {
			t.Errorf("parseOCMtime(%q) ok=%v, want %v", c.raw, ok, c.ok)
			continue
		}
		if ok && got.Unix() != c.want {
			t.Errorf("parseOCMtime(%q) = %d, want %d", c.raw, got.Unix(), c.want)
		}
	}
}

// newMtimeMockSAPI simula lo mínimo de SAPI y guarda el campo "data" de cada subida.
func newMtimeMockSAPI(t *testing.T) (*httptest.Server, func() []map[string]interface{}) {
	t.Helper()
	var mu sync.Mutex
	var uploads []map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		switch {
		case r.URL.Path == "/sapi/media/folder/root" && action == "get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"folders": []map[string]interface{}{{"id": 100, "name": "Root", "parentid": 0, "magic": true}},
				},
			})
		case r.URL.Path == "/sapi/upload" && action == "save":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart inválido: %v", err)
			}
			var meta struct {
				Data map[string]interface{} `json:"data"`
			}
			_ = json.Unmarshal([]byte(r.FormValue("data")), &meta)
			mu.Lock()
			uploads = append(uploads, meta.Data)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"id": "777", "name": meta.Data["name"], "url": "/sapi/media/content?id=777"},
			})
		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))

	get := func() []map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]interface{}(nil), uploads...)
	}
	return srv, get
}

func setupMtimeServer(t *testing.T) (*Server, func() []map[string]interface{}) {
	t.Helper()
	mock, uploads := newMtimeMockSAPI(t)
	t.Cleanup(mock.Close)
	client, err := sapi.NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "test-vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	return NewServerWithAuth(client, config.AuthConfig{AuthDisabled: true}), uploads
}

func TestWebDAV_PutWithOCMtimePreservesModificationDate(t *testing.T) {
	srv, uploads := setupMtimeServer(t)

	// 1. PUT con X-OC-Mtime -> se acepta y se envía como modificationdate
	req := httptest.NewRequest("PUT", "/foto.jpg", bytes.NewReader([]byte("contenido")))
	req.Header.Set("X-OC-Mtime", "1700000000") // 2023-11-14 22:13:20 UTC
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("PUT esperado 201/204, obtenido %d", rec.Code)
	}
	if got := rec.Header().Get("X-OC-Mtime"); got != "accepted" {
		t.Errorf("cabecera de respuesta X-OC-Mtime = %q, esperado \"accepted\"", got)
	}
	up := uploads()
	if len(up) != 1 {
		t.Fatalf("esperada 1 subida a SAPI, hubo %d", len(up))
	}
	if got := up[0]["modificationdate"]; got != "20231114T221320Z" {
		t.Errorf("modificationdate enviado = %v, esperado 20231114T221320Z", got)
	}

	// 2. PROPFIND inmediato (desde la caché de subidas) devuelve la fecha original
	pf := httptest.NewRequest("PROPFIND", "/foto.jpg", strings.NewReader(
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getlastmodified/></d:prop></d:propfind>`))
	pf.Header.Set("Depth", "0")
	pfRec := httptest.NewRecorder()
	srv.ServeHTTP(pfRec, pf)
	want := time.Unix(1700000000, 0).UTC().Format(http.TimeFormat)
	if !strings.Contains(pfRec.Body.String(), want) {
		t.Errorf("PROPFIND no devuelve la fecha original %q:\n%s", want, pfRec.Body.String())
	}
}

func TestWebDAV_PutWithoutOrInvalidOCMtimeKeepsCurrentBehaviour(t *testing.T) {
	srv, uploads := setupMtimeServer(t)

	for i, hdr := range []string{"", "no-es-un-numero"} {
		req := httptest.NewRequest("PUT", "/archivo.bin", bytes.NewReader([]byte("x")))
		if hdr != "" {
			req.Header.Set("X-OC-Mtime", hdr)
		}
		before := time.Now().UTC().Add(-2 * time.Second)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Header().Get("X-OC-Mtime") != "" {
			t.Errorf("caso %d: no debería anunciar X-OC-Mtime", i)
		}
		up := uploads()
		if len(up) != i+1 {
			t.Fatalf("caso %d: esperadas %d subidas, hubo %d", i, i+1, len(up))
		}
		got, err := time.Parse("20060102T150405Z", up[i]["modificationdate"].(string))
		if err != nil || got.Before(before) {
			t.Errorf("caso %d: modificationdate debería ser la hora actual, es %v", i, up[i]["modificationdate"])
		}
	}
}
