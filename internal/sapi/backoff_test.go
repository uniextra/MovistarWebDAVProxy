package sapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"movistar-webdav-proxy/internal/config"
)

func TestNewClient_MaxIdleConnsPerHost(t *testing.T) {
	client, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "test-vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.httpClient.Transport)
	}
	if transport.MaxIdleConnsPerHost != 20 {
		t.Errorf("expected MaxIdleConnsPerHost = 20, got %d", transport.MaxIdleConnsPerHost)
	}
	if cap(client.sapiSem) != 1 {
		t.Errorf("expected sapiSem capacity = 1, got %d", cap(client.sapiSem))
	}
}

func TestDoRequest_429Retry(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := attempts.Add(1)
		if att < 3 {
			// Las 2 primeras peticiones devuelven 429 Too Many Requests
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		// La 3ª petición tiene éxito
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"status":"ok"}}`))
	}))
	defer server.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "test-vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(server.URL, server.URL)

	req, err := http.NewRequest("POST", server.URL+"/test", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	resp, err := client.DoRequest(req)
	if err != nil {
		t.Fatalf("DoRequest failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected HTTP 200 after retries, got %d", resp.StatusCode)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", attempts.Load())
	}
}

func TestCreateFolder_FOL1035_LimitError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"data": {
				"folders": []
			},
			"error": {
				"code": "FOL-1035",
				"message": "Maximum folder count reached for user"
			}
		}`))
	}))
	defer server.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "test-vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(server.URL, server.URL)

	item, err := client.CreateFolder("nueva_carpeta", 0)
	if err == nil {
		t.Fatalf("expected error from CreateFolder on FOL-1035, got item: %+v", item)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("FOL-1035")) {
		t.Errorf("expected error message to contain 'FOL-1035', got: %v", err)
	}
}

func TestCreateFolder_ZeroID_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"folders":[]}}`))
	}))
	defer server.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test-access",
		RefreshToken:  "test-refresh",
		ValidationKey: "test-vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(server.URL, server.URL)

	item, err := client.CreateFolder("carpeta_sin_id", 0)
	if err == nil {
		t.Fatalf("expected error when SAPI returns no ID, got item: %+v", item)
	}
	if item != nil {
		t.Errorf("expected nil item, got %+v", item)
	}
}
