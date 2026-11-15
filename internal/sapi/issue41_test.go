package sapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"movistar-webdav-proxy/internal/config"
)

// TestNormalDownload_EmptyBodyRefreshesURL verifies Issue #41 in normal (non-web) mode:
// when a preexisting MediaItem.URL returns HTTP 200 with an empty body (or 403/etc.) for a
// non-empty file, DownloadFileStreamRange refreshes the URL via GetMediaByID and succeeds.
func TestNormalDownload_EmptyBodyRefreshesURL(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 2048)
	var srv *httptest.Server
	var getMediaCalls int

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sapi/media" && r.URL.Query().Get("action") == "get":
			getMediaCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"media": []map[string]interface{}{
						{
							"id":   "41001",
							"name": "preexisting.bin",
							"size": len(payload),
							"url":  srv.URL + "/fresh-content",
						},
					},
				},
			})
		case r.URL.Path == "/stale-empty":
			// Simulates upstream returning 200 OK with empty body (Content-Length: 0)
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/fresh-content":
			w.Header().Set("Content-Range", "bytes 512-2047/2048")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[512:])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:    "movistar",
		AccessToken: "test-token",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL+"/upload")

	rc, err := client.DownloadFileStreamRange(MediaItem{
		ID:   "41001",
		Name: "preexisting.bin",
		Size: int64(len(payload)),
		URL:  srv.URL + "/stale-empty",
	}, 512)
	if err != nil {
		t.Fatalf("DownloadFileStreamRange failed: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(got, payload[512:]) {
		t.Fatalf("got %d bytes, want %d", len(got), len(payload[512:]))
	}
	if getMediaCalls != 1 {
		t.Fatalf("expected 1 GetMediaByID refresh call, got %d", getMediaCalls)
	}
}
