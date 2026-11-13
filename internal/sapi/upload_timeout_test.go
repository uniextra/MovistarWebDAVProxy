package sapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
)

func TestUploadFileToFolder_Retries408WithFreshConnection(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/sapi/upload" || r.URL.Query().Get("action") != "save" {
			http.NotFound(w, r)
			return
		}
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			_, _ = w.Write([]byte("Server timeout waiting for the HTTP request from the client"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"id":   "123",
				"name": "x.bin",
				"url":  "/sapi/download/file?id=123",
			},
		})
	}))
	defer srv.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		AccessToken:   "a",
		RefreshToken:  "r",
		ValidationKey: "v",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.SetBaseURLs(srv.URL+"/sapi", srv.URL)

	item, err := client.UploadFileToFolder("x.bin", bytes.NewReader(make([]byte, 64)), 64, 100, "application/octet-stream", "", time.Time{})
	if err != nil {
		t.Fatalf("expected 408 retry to succeed: %v", err)
	}
	if item == nil || item.ID != "123" {
		t.Fatalf("unexpected item: %+v", item)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 upload attempts, got %d", got)
	}
}

func TestUploadTransportDisablesKeepAlives(t *testing.T) {
	client, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		AccessToken:   "a",
		RefreshToken:  "r",
		ValidationKey: "v",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	provider, ok := client.httpClient.Transport.(interface {
		HTTP1Transport() *http.Transport
	})
	if !ok {
		t.Fatalf("transport does not expose HTTP1Transport: %T", client.httpClient.Transport)
	}
	if !provider.HTTP1Transport().DisableKeepAlives {
		t.Fatal("upload HTTP/1.1 transport must disable keep-alives to avoid stale-connection 408")
	}
}

func TestIsTerminalRefreshError(t *testing.T) {
	if !isTerminalRefreshError(errors.New("OAuth token refresh failed (HTTP 401)")) {
		t.Fatal("401 refresh error must be terminal")
	}
	if !isTerminalRefreshError(errors.New("invalid_grant")) {
		t.Fatal("invalid_grant must be terminal")
	}
	if isTerminalRefreshError(errors.New("connection reset by peer")) {
		t.Fatal("network error must be transient")
	}
}
