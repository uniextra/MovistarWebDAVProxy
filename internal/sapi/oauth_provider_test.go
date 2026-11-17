package sapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
)

// TestRefreshSession_O2_SAPILoginSuccess verifies that for O2 Cloud, when
// tokens are successfully renewed at apiseg.telefonica.es, RefreshSession
// initializes SAPI via SAPILoginOAuth, captures validationkey and jsessionid,
// and returns true with an active session.
func TestRefreshSession_O2_SAPILoginSuccess(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	// 1. Mock token server (apiseg.telefonica.es)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			http.Error(w, "invalid grant_type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "new-o2-access-token",
			"refresh_token": "new-o2-refresh-token",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	t.Setenv("OAUTH_TOKEN_URL", tokenServer.URL)

	// 2. Mock SAPI server for O2
	var loginOAuthCalled atomic.Bool
	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/login/oauth") {
			loginOAuthCalled.Store(true)
			// Rechazar token viejo para forzar canje de refresh_token en apiseg
			rawAuth := r.Header.Get("Authorization")
			if strings.HasPrefix(rawAuth, "oauth ") {
				if b, err := base64.StdEncoding.DecodeString(rawAuth[6:]); err == nil && strings.Contains(string(b), "old-expired-token") {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
					return
				}
			}
			http.SetCookie(w, &http.Cookie{
				Name:  "JSESSIONID",
				Value: "new-o2-jsessionid",
				Path:  "/",
			})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"validationkey": "new-o2-valkey",
					"jsessionid":    "new-o2-jsessionid",
					"roles":         []map[string]string{{"name": "sync_user"}},
				},
				"responsetime": 1794834400000,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"status": "ok"}})
	}))
	defer sapiServer.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "o2",
		Phone:           "612345678",
		AccessToken:     "old-expired-token",
		RefreshToken:    "valid-refresh-token",
		DeviceID:        "fol-test-device",
		DeviceName:      "PC Windows",
		LastRefreshDate: 1000,
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	// 3. Execute RefreshSession
	ok := client.RefreshSession()
	if !ok {
		t.Fatalf("RefreshSession() = false, want true for O2 when token refresh succeeds")
	}

	if client.IsSessionExpired() {
		t.Errorf("IsSessionExpired() = true, want false")
	}

	cfg := client.GetConfig()
	if cfg.AccessToken != "new-o2-access-token" {
		t.Errorf("AccessToken = %q, want %q", cfg.AccessToken, "new-o2-access-token")
	}
	if cfg.RefreshToken != "new-o2-refresh-token" {
		t.Errorf("RefreshToken = %q, want %q", cfg.RefreshToken, "new-o2-refresh-token")
	}
	if !loginOAuthCalled.Load() {
		t.Errorf("loginOAuthCalled = false, want true")
	}
	if cfg.ValidationKey != "new-o2-valkey" {
		t.Errorf("ValidationKey = %q, want new-o2-valkey", cfg.ValidationKey)
	}
	if cfg.JSessionID != "new-o2-jsessionid" {
		t.Errorf("JSessionID = %q, want new-o2-jsessionid", cfg.JSessionID)
	}
}

// TestRefreshSession_O2_RecoversValidationKeyWithoutTokenRefresh verifies that if
// an O2 account has an active access token but lacks validationkey, RefreshSession
// initializes SAPI directly via SAPILoginOAuth without touching the refresh token.
func TestRefreshSession_O2_RecoversValidationKeyWithoutTokenRefresh(t *testing.T) {
	var tokenServerCalled atomic.Bool
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenServerCalled.Store(true)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer tokenServer.Close()
	t.Setenv("OAUTH_TOKEN_URL", tokenServer.URL)

	var loginOAuthCalled atomic.Bool
	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/login/oauth") {
			loginOAuthCalled.Store(true)
			http.SetCookie(w, &http.Cookie{
				Name:  "JSESSIONID",
				Value: "recovered-jsessionid",
				Path:  "/",
			})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"validationkey": "recovered-valkey",
					"jsessionid":    "recovered-jsessionid",
				},
				"responsetime": 1794834400000,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"status": "ok"}})
	}))
	defer sapiServer.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "o2",
		Phone:           "612345678",
		AccessToken:     "valid-active-token",
		RefreshToken:    "unburned-refresh-token",
		ValidationKey:   "", // missing validation key
		DeviceID:        "web-test-device",
		DeviceName:      "PC Windows",
		LastRefreshDate: 1000,
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	ok := client.RefreshSession()
	if !ok {
		t.Fatalf("RefreshSession() = false, want true")
	}

	if tokenServerCalled.Load() {
		t.Errorf("tokenServer was called, but should have been avoided to protect refresh_token")
	}
	if !loginOAuthCalled.Load() {
		t.Errorf("loginOAuthCalled = false, want true")
	}

	cfg := client.GetConfig()
	if cfg.ValidationKey != "recovered-valkey" {
		t.Errorf("ValidationKey = %q, want recovered-valkey", cfg.ValidationKey)
	}
	if cfg.JSessionID != "recovered-jsessionid" {
		t.Errorf("JSessionID = %q, want recovered-jsessionid", cfg.JSessionID)
	}
}

// TestRefreshSession_CooldownPreventsForceOAuthEscalationLoop verifies that a
// 401 within the <30s cooldown window does not escalate doRequestLocked to
// forceOAuth=true and burn refresh_token in a tight loop.
func TestRefreshSession_CooldownPreventsForceOAuthEscalationLoop(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser")
	oldTokensDir := config.TokensDir
	config.TokensDir = t.TempDir()
	t.Cleanup(func() { config.TokensDir = oldTokensDir })

	var tokenCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		http.Error(w, "should not be called during cooldown", http.StatusInternalServerError)
	}))
	defer tokenServer.Close()
	t.Setenv("OAUTH_TOKEN_URL", tokenServer.URL)

	var loginCalls atomic.Int32
	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/login/oauth") {
			loginCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer sapiServer.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "o2",
		Phone:           "612345678",
		AccessToken:     "fresh-access",
		RefreshToken:    "fresh-refresh",
		JSessionID:      "existing-jsess",
		DeviceID:        "web-test-cooldown",
		DeviceName:      "PC Windows",
		LastRefreshDate: time.Now().UnixMilli(),
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	req, err := http.NewRequest(http.MethodGet, sapiServer.URL+"/sapi/profile/changes?action=get&from=0", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	resp, err := client.DoRequest(req)
	if err != nil {
		t.Fatalf("DoRequest returned unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if tokenCalls.Load() != 0 {
		t.Errorf("tokenServer called %d times during <30s cooldown, want 0", tokenCalls.Load())
	}
	if loginCalls.Load() == 0 {
		t.Errorf("expected Step 1 SAPILoginOAuth attempt before cooldown check")
	}
}

// TestDoRequest_O2_SendsValidationKeyAndCookie verifies that SAPI requests for O2
// include validationkey and Cookie: JSESSIONID headers as required by Synchronoss.
func TestDoRequest_O2_SendsValidationKeyAndCookie(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	var receivedCookie string
	var receivedAuth string
	var receivedURL string

	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCookie = r.Header.Get("Cookie")
		receivedAuth = r.Header.Get("Authorization")
		receivedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"profile":{"id":"123"}}}`))
	}))
	defer sapiServer.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "o2",
		Phone:           "612345678",
		AccessToken:     "valid-access-token",
		RefreshToken:    "valid-refresh-token",
		ValidationKey:   "o2-vk-123",
		JSessionID:      "o2-jsess-456",
		DeviceID:        "fol-test-device",
		DeviceName:      "PC Windows",
		LastRefreshDate: 1700000000000,
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	req, err := http.NewRequest(http.MethodGet, sapiServer.URL+"/sapi/profile/changes?action=get&from=0", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	resp, err := client.DoRequest(req)
	if err != nil {
		t.Fatalf("DoRequest failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	if !strings.Contains(receivedCookie, "JSESSIONID=o2-jsess-456") {
		t.Errorf("Cookie = %q, want JSESSIONID=o2-jsess-456", receivedCookie)
	}
	if !strings.Contains(receivedCookie, "validationkey=o2-vk-123") {
		t.Errorf("Cookie = %q, want validationkey=o2-vk-123", receivedCookie)
	}
	if !strings.Contains(receivedURL, "validationkey=o2-vk-123") {
		t.Errorf("URL = %q, want validationkey parameter", receivedURL)
	}
	if !strings.HasPrefix(receivedAuth, "oauth ") {
		t.Errorf("Authorization = %q, want 'oauth ...'", receivedAuth)
	}
}

// TestRefreshSession_Movistar_SAPILoginSuccess verifies the existing Movistar
// flow completes SAPILoginOAuth after refreshing tokens.
func TestRefreshSession_Movistar_SAPILoginSuccess(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "new-movistar-access-token",
			"refresh_token": "new-movistar-refresh-token",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	t.Setenv("OAUTH_TOKEN_URL", tokenServer.URL)

	var loginCalled atomic.Bool
	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/login/oauth") {
			loginCalled.Store(true)
			http.SetCookie(w, &http.Cookie{
				Name:  "JSESSIONID",
				Value: "new-session-id",
				Path:  "/",
			})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"validationkey": "new-validation-key",
					"responsetime":  1700000000000,
				},
				"responsetime": 1700000000000,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"status": "ok"}})
	}))
	defer sapiServer.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "movistar",
		Phone:           "612345678",
		AccessToken:     "expired-movistar-token",
		RefreshToken:    "valid-movistar-refresh",
		DeviceID:        "fol-test-device",
		DeviceName:      "PC Windows",
		LastRefreshDate: 1000,
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	ok := client.RefreshSession()
	if !ok {
		t.Fatalf("RefreshSession() = false, want true for Movistar")
	}

	if !loginCalled.Load() {
		t.Errorf("SAPILoginOAuth was not called for Movistar")
	}

	cfg := client.GetConfig()
	if cfg.ValidationKey != "new-validation-key" {
		t.Errorf("ValidationKey = %q, want 'new-validation-key'", cfg.ValidationKey)
	}
	if cfg.JSessionID != "new-session-id" {
		t.Errorf("JSessionID = %q, want 'new-session-id'", cfg.JSessionID)
	}
}
