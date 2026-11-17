package sapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/config"
)

func TestCI_CloudFrontWAF_RejectsLegacyFolDeviceIDAndBadUserAgent(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	t.Setenv("SAPI_USER_AGENT", "")
	t.Setenv("DESKTOP_USER_AGENT", "")

	var sawDeviceID, sawUserAgent, sawReferer, sawXHR, sawAcceptLang string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawDeviceID = r.Header.Get("X-deviceid")
		sawUserAgent = r.Header.Get("User-Agent")
		sawReferer = r.Header.Get("Referer")
		sawXHR = r.Header.Get("X-Requested-With")
		sawAcceptLang = r.Header.Get("Accept-Language")

		dropAsCloudFront401 := func() {
			w.Header().Set("Server", "CloudFront")
			w.Header().Set("X-Cache", "Error from cloudfront")
			w.WriteHeader(http.StatusUnauthorized) // 0-byte body
		}

		if sawDeviceID == "" || strings.HasPrefix(sawDeviceID, "fol-") || strings.ContainsAny(sawDeviceID, "+/=") {
			dropAsCloudFront401()
			return
		}
		if sawUserAgent == "DESKTOP" || strings.Contains(sawUserAgent, "Edg/157.0.0.0") || !strings.Contains(sawUserAgent, "Chrome/133.0.0.0") {
			dropAsCloudFront401()
			return
		}
		if sawReferer == "" || sawXHR != "XMLHttpRequest" || sawAcceptLang == "" {
			dropAsCloudFront401()
			return
		}

		w.Header().Set("Content-Type", "text/javascript;charset=UTF-8")
		switch {
		case strings.Contains(r.URL.Path, "/media"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"quota":       int64(9223372036854775807),
					"used":        int64(361411543375),
					"free":        int64(9223371675443232432),
					"softdeleted": int64(134492577931),
					"nolimit":     true,
				},
				"responsetime": 1794844952692,
			})
		case strings.Contains(r.URL.Path, "/profile/changes"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"requesttime":  int64(1794844952000),
				"responsetime": int64(1794844952692),
				"data":         map[string]interface{}{},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{}})
		}
	}))
	defer server.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "movistar",
		Phone:           "612345678",
		AccessToken:     "valid-token",
		RefreshToken:    "valid-refresh",
		DeviceID:        "fol-xShd2lUKwcsWLi7YPhSfnA==",
		DeviceName:      "PC Windows",
		LastRefreshDate: time.Now().UnixMilli(),
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(server.URL+"/sapi", server.URL+"/upload")

	space, err := client.GetStorageSpace()
	if err != nil {
		t.Fatalf("GetStorageSpace failed: %v (UA=%q, X-deviceid=%q)", err, sawUserAgent, sawDeviceID)
	}
	if space == nil || space.UsedBytes != 361411543375 || !space.NoLimit {
		t.Fatalf("unexpected storage space: %+v", space)
	}

	if _, err := client.GetProfileChanges("0"); err != nil {
		t.Fatalf("GetProfileChanges failed: %v", err)
	}

	if sawDeviceID != "web-xShd2lUKwcsWLi7YPhSfnA" {
		t.Errorf("X-deviceid = %q, want %q", sawDeviceID, "web-xShd2lUKwcsWLi7YPhSfnA")
	}
	if !strings.Contains(sawUserAgent, "Chrome/133.0.0.0") {
		t.Errorf("User-Agent = %q, want Chrome/133.0.0.0", sawUserAgent)
	}

	// Verify SAPILoginOAuth also sanitizes legacy fol- DeviceIDs
	if got := auth.SanitizeDeviceID("fol-xShd2lUKwcsWLi7YPhSfnA=="); got != "web-xShd2lUKwcsWLi7YPhSfnA" {
		t.Errorf("SanitizeDeviceID = %q, want web-xShd2lUKwcsWLi7YPhSfnA", got)
	}
}

func TestCI_RefreshSession_WhenSAPILoginReturnsHTMLShell(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	t.Setenv("SAPI_USER_AGENT", "")
	t.Setenv("DESKTOP_USER_AGENT", "")

	var refreshCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "fresh-access-token",
			"refresh_token": "fresh-refresh-token",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()
	t.Setenv("OAUTH_TOKEN_URL", tokenServer.URL)

	var loginCalls atomic.Int32
	var mediaCalls atomic.Int32
	var seenRetryQuery string

	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/login/oauth") {
			call := loginCalls.Add(1)
			if call == 1 {
				// Primary attempt: returns 200 text/html SPA shell with no validationkey
				w.Header().Set("Content-Type", "text/html;charset=UTF-8")
				_, _ = w.Write([]byte(`<html><body>SPA shell</body></html>`))
				return
			}
			// Secondary attempt after RefreshOAuthToken: still returns HTML/401
			w.Header().Set("Content-Type", "text/html;charset=UTF-8")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if strings.Contains(r.URL.Path, "/media") {
			call := mediaCalls.Add(1)
			if call == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			seenRetryQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"quota":   int64(1000),
					"used":    int64(100),
					"free":    int64(900),
					"nolimit": false,
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer sapiServer.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "movistar",
		Phone:           "612345678",
		AccessToken:     "expired-access-token",
		RefreshToken:    "valid-refresh-token",
		ValidationKey:   "old-vk",
		DeviceID:        "fol-xShd2lUKwcsWLi7YPhSfnA==",
		DeviceName:      "PC Windows",
		LastRefreshDate: time.Now().Add(-2 * time.Minute).UnixMilli(),
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	space, err := client.GetStorageSpace()
	if err != nil {
		t.Fatalf("GetStorageSpace after RefreshSession failed: %v", err)
	}
	if space == nil || space.UsedBytes != 100 {
		t.Fatalf("unexpected storage space after retry: %+v", space)
	}
	if refreshCalls.Load() != 1 {
		t.Errorf("RefreshOAuthToken calls = %d, want 1", refreshCalls.Load())
	}
	if client.IsSessionExpired() {
		t.Errorf("IsSessionExpired() = true, want false")
	}
	if cfg := client.GetConfig(); cfg.AccessToken != "fresh-access-token" || cfg.RefreshToken != "fresh-refresh-token" {
		t.Errorf("unexpected rotated config: %+v", cfg)
	}
	if strings.Contains(seenRetryQuery, "validationkey=old-vk&validationkey=") {
		t.Errorf("duplicate validationkey in retried query: %q", seenRetryQuery)
	}
}

func TestCI_ProcessSAPIResponse_Ignores401AnonymousJSessionIDCookie(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	t.Setenv("SAPI_USER_AGENT", "")
	t.Setenv("DESKTOP_USER_AGENT", "")

	var refreshCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "fresh-access-after-401-cookie",
			"refresh_token": "fresh-refresh-after-401-cookie",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()
	t.Setenv("OAUTH_TOKEN_URL", tokenServer.URL)

	var mediaCalls atomic.Int32
	var retrySawCookie string
	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/login/oauth") {
			http.SetCookie(w, &http.Cookie{
				Name:  "JSESSIONID",
				Value: "fresh-auth-jsess",
				Path:  "/",
			})
			w.Header().Set("Content-Type", "text/html;charset=UTF-8")
			_, _ = w.Write([]byte(`<html><body>Movistar Cloud</body></html>`))
			return
		}
		if strings.Contains(r.URL.Path, "/media") {
			call := mediaCalls.Add(1)
			if call == 1 {
				http.SetCookie(w, &http.Cookie{
					Name:  "JSESSIONID",
					Value: "unauth-401-cookie",
					Path:  "/",
				})
				w.Header().Set("Content-Type", "text/javascript;charset=UTF-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`<html><title>Movistar Cloud</title></html>`))
				return
			}
			retrySawCookie = r.Header.Get("Cookie")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"quota":   int64(2000),
					"used":    int64(500),
					"free":    int64(1500),
					"nolimit": false,
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer sapiServer.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "movistar",
		Phone:           "612345678",
		AccessToken:     "expired-access",
		RefreshToken:    "valid-refresh",
		JSessionID:      "orig-jsess",
		DeviceID:        "web-clean-device",
		DeviceName:      "PC Windows",
		LastRefreshDate: time.Now().Add(-2 * time.Minute).UnixMilli(),
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	space, err := client.GetStorageSpace()
	if err != nil {
		t.Fatalf("GetStorageSpace failed: %v", err)
	}
	if space == nil || space.UsedBytes != 500 {
		t.Fatalf("unexpected storage space: %+v", space)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("RefreshOAuthToken calls = %d, want 1 (401 cookie must NOT skip RefreshSession)", refreshCalls.Load())
	}
	if strings.Contains(retrySawCookie, "unauth-401-cookie") {
		t.Errorf("retry sent anonymous 401 cookie: %q", retrySawCookie)
	}
	if !strings.Contains(retrySawCookie, "JSESSIONID=fresh-auth-jsess") {
		t.Errorf("retry missing authenticated JSESSIONID: %q", retrySawCookie)
	}
	if got := client.GetConfig().JSessionID; got == "unauth-401-cookie" {
		t.Errorf("client.cfg.JSessionID overwritten with 401 cookie")
	}
}

func TestCI_SAPILoginOAuth_RetainsPrimedJSessionIDOn200HTMLWithoutSetCookie(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	var loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ui/html/clientoauth.html":
			http.SetCookie(w, &http.Cookie{
				Name:  "JSESSIONID",
				Value: "primed-123",
				Path:  "/",
			})
			_, _ = w.Write([]byte(`<html></html>`))
		case "/sapi/login/oauth":
			loginCalls.Add(1)
			// Real Movistar/O2 SAPI behavior: 200 OK text/html, 0 Set-Cookie headers
			w.Header().Set("Content-Type", "text/html;charset=UTF-8")
			_, _ = w.Write([]byte(`<html><title>Movistar Cloud</title></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Jar: jar}

	resp, err := httpClient.Get(server.URL + "/ui/html/clientoauth.html")
	if err != nil {
		t.Fatalf("prime GET failed: %v", err)
	}
	resp.Body.Close()

	parsedBase, _ := url.Parse(server.URL)
	if len(jar.Cookies(parsedBase)) == 0 {
		t.Fatalf("expected primed cookie in jar")
	}

	res := auth.SAPILoginOAuth(httpClient, server.URL+"/sapi", "acc", "ref", "web-dev", "PC Windows", time.Now().UnixMilli(), "3600")
	if res.StatusCode != http.StatusOK || res.Error != nil {
		t.Fatalf("SAPILoginOAuth failed: status=%d err=%v", res.StatusCode, res.Error)
	}
	if res.JSessionID != "primed-123" {
		t.Fatalf("res.JSessionID = %q, want %q", res.JSessionID, "primed-123")
	}
	if loginCalls.Load() != 1 {
		t.Errorf("loginCalls = %d, want 1", loginCalls.Load())
	}
}

func TestCI_UploadFileToFolder_RetriesOn401AndSucceeds(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	oldTokensDir := config.TokensDir
	config.TokensDir = t.TempDir()
	t.Cleanup(func() { config.TokensDir = oldTokensDir })

	var refreshCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "fresh-access-upload",
			"refresh_token": "fresh-refresh-upload",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()
	t.Setenv("OAUTH_TOKEN_URL", tokenServer.URL)

	const payload = "hello-hyperbackup-index-payload"
	var uploadCalls atomic.Int32
	var retriedPayloadOK atomic.Bool
	var retriedTokenOK atomic.Bool

	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/login/oauth"):
			rotated := auth.ParseOAuthResponseHeader(r.Header.Get("Authorization"))
			if rotated == nil || rotated.AccessToken != "fresh-access-upload" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"validationkey": "fresh-vk-upload",
					"responsetime":  time.Now().UnixMilli(),
				},
			})
		case strings.Contains(r.URL.Path, "/upload"):
			call := uploadCalls.Add(1)
			bodyBytes, _ := io.ReadAll(r.Body)
			if call == 1 {
				w.Header().Set("Content-Type", "text/html; charset=UTF-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`<!--?xml version="1.0" encoding="UTF-8"?--><!DOCTYPE html><html><head><title>Movistar Cloud</title></head><body>` + strings.Repeat("x", 95000) + `</body></html>`))
				return
			}
			if rotated := auth.ParseOAuthResponseHeader(r.Header.Get("Authorization")); rotated != nil && rotated.AccessToken == "fresh-access-upload" {
				retriedTokenOK.Store(true)
			}
			if strings.Contains(string(bodyBytes), payload) {
				retriedPayloadOK.Store(true)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"id":   int64(7289446999),
					"name": "387.index.2",
					"size": int64(len(payload)),
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer sapiServer.Close()

	client, err := NewClient(config.AccountConfig{
		Provider:        "movistar",
		Phone:           "612345678",
		AccessToken:     "expired-access-upload",
		RefreshToken:    "valid-refresh-upload",
		DeviceID:        "web-upload-test",
		DeviceName:      "PC Windows",
		LastRefreshDate: time.Now().Add(-2 * time.Minute).UnixMilli(),
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	item, err := client.UploadFileToFolder("387.index.2", bytes.NewReader([]byte(payload)), int64(len(payload)), 100, "application/octet-stream", "", time.Time{})
	if err != nil {
		t.Fatalf("UploadFileToFolder failed after 401 retry: %v", err)
	}
	if item == nil || item.ID != "7289446999" {
		t.Fatalf("unexpected uploaded item: %+v", item)
	}
	if uploadCalls.Load() != 2 {
		t.Errorf("uploadCalls = %d, want 2", uploadCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Errorf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
	if !retriedTokenOK.Load() {
		t.Errorf("retry did not use refreshed access token")
	}
	if !retriedPayloadOK.Load() {
		t.Errorf("retry did not rewind multipart file payload")
	}
}

func TestCI_RefreshSession_StaleLastRefreshDateRenewsInStep1(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // comportamiento del perfil "browser"
	oldTokensDir := config.TokensDir
	config.TokensDir = t.TempDir()
	t.Cleanup(func() { config.TokensDir = oldTokensDir })

	var refreshCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		http.Error(w, "should not call apiseg when Step 1 succeeds", http.StatusUnauthorized)
	}))
	defer tokenServer.Close()
	t.Setenv("OAUTH_TOKEN_URL", tokenServer.URL)

	var sawFreshLRD atomic.Bool
	sapiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/login/oauth") {
			if parsed := auth.ParseOAuthResponseHeader(r.Header.Get("Authorization")); parsed != nil {
				if time.Since(time.UnixMilli(parsed.LastRefreshDate)) < time.Minute {
					sawFreshLRD.Store(true)
				}
			}
			http.SetCookie(w, &http.Cookie{
				Name:  "JSESSIONID",
				Value: "renewed-jsess-after-1h",
				Path:  "/",
			})
			w.Header().Set("Content-Type", "text/html;charset=UTF-8")
			_, _ = w.Write([]byte(`<html><title>Movistar Cloud</title></html>`))
			return
		}
		http.NotFound(w, r)
	}))
	defer sapiServer.Close()

	staleLRD := time.Now().Add(-65 * time.Minute).UnixMilli()
	client, err := NewClient(config.AccountConfig{
		Provider:        "movistar",
		Phone:           "612345678",
		AccessToken:     "still-valid-access",
		RefreshToken:    "valid-refresh",
		JSessionID:      "old-jsess",
		ValidationKey:   "old-vk",
		DeviceID:        "web-stale-lrd",
		DeviceName:      "PC Windows",
		LastRefreshDate: staleLRD,
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURLs(sapiServer.URL+"/sapi", sapiServer.URL+"/upload")

	if ok := client.RefreshSession(); !ok {
		t.Fatalf("RefreshSession() = false, want true")
	}
	if refreshCalls.Load() != 0 {
		t.Errorf("RefreshOAuthToken calls = %d, want 0 (Step 1 should renew without burning refresh_token)", refreshCalls.Load())
	}
	if !sawFreshLRD.Load() {
		t.Errorf("Step 1 sent stale LastRefreshDate instead of fresh timestamp")
	}
	cfg := client.GetConfig()
	if cfg.JSessionID != "renewed-jsess-after-1h" {
		t.Errorf("JSessionID = %q, want renewed-jsess-after-1h", cfg.JSessionID)
	}
	if time.Since(time.UnixMilli(cfg.LastRefreshDate)) > time.Minute {
		t.Errorf("LastRefreshDate was not updated: %d", cfg.LastRefreshDate)
	}
}

func TestCI_SummarizeHTTPErrorBody_TruncatesHTML(t *testing.T) {
	htmlBody := []byte(`<!--?xml version="1.0" encoding="UTF-8"?--><!DOCTYPE html><html><head><title>Movistar Cloud</title></head><body>` + strings.Repeat("<div>SPA</div>", 5000) + `</body></html>`)
	got := summarizeHTTPErrorBody(htmlBody)
	if !strings.Contains(got, "HTML error page") || !strings.Contains(got, `title="Movistar Cloud"`) {
		t.Fatalf("unexpected HTML summary: %q", got)
	}
	if len(got) >= 200 {
		t.Fatalf("HTML summary too long (%d bytes): %q", len(got), got)
	}

	longText := []byte(strings.Repeat("a", 500))
	gotLong := summarizeHTTPErrorBody(longText)
	if len(gotLong) > 250 || !strings.HasSuffix(gotLong, "…") {
		t.Fatalf("unexpected long text truncation: len=%d val=%q", len(gotLong), gotLong)
	}

	shortJSON := []byte(`{"error":{"code":"MED-1017"}}`)
	if gotShort := summarizeHTTPErrorBody(shortJSON); gotShort != string(shortJSON) {
		t.Fatalf("short JSON modified: %q", gotShort)
	}
}
