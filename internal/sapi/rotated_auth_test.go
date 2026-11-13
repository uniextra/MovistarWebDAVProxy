package sapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/config"
)

func TestDoRequestRetriesWithServerRotatedAuthHeader(t *testing.T) {
	newHeader := auth.BuildOAuthHeader("new-access", "new-refresh", 1700000000000, "3600")
	var calls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Authorization", newHeader)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	defer server.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "movistar",
		AccessToken:     "old-access",
		LastRefreshDate: 1700000000000,
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.DoRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	cfg := client.GetConfig()
	if cfg.AccessToken != "new-access" || cfg.RefreshToken != "new-refresh" {
		t.Fatalf("rotated credentials not retained: %+v", cfg)
	}
}
