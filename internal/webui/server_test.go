package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
	"movistar-webdav-proxy/internal/webdav"
)

func setupTestWebUI(t *testing.T, authCfg config.AuthConfig) (http.Handler, string) {
	t.Helper()
	tmpDir := t.TempDir()
	origTokensDir := config.TokensDir
	config.TokensDir = tmpDir
	t.Cleanup(func() { config.TokensDir = origTokensDir })

	cfg := config.AccountConfig{
		Provider:         "movistar",
		Phone:            "612345678",
		DeviceID:         "fol-secretdevice1234",
		RefreshToken:     "refresh-secret-token",
		AccessToken:      "access-secret-token",
		TelegramBotToken: "123456789:SecretBotTokenValueXYZ",
		TelegramChatID:   "-100123456",
	}
	if err := config.SaveAccountConfig(cfg); err != nil {
		t.Fatalf("SaveAccountConfig failed: %v", err)
	}

	client, err := sapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	davSrv := webdav.NewServerWithAuth(client, authCfg)
	uiSrv := NewServerWithAuth(client, davSrv, authCfg)
	return uiSrv.Routes(), tmpDir
}

func TestWebUI_AuthAndHealthExemption(t *testing.T) {
	handler, _ := setupTestWebUI(t, config.AuthConfig{
		Username: "admin",
		Password: "secretpassword",
	})

	// 1. /health no requiere autenticación
	healthReq := httptest.NewRequest("GET", "/health", nil)
	healthRec := httptest.NewRecorder()
	handler.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("expected /health 200 without auth, got %d", healthRec.Code)
	}

	// 2. /status sin credenciales -> 401
	statusReq := httptest.NewRequest("GET", "/status", nil)
	statusRec := httptest.NewRecorder()
	handler.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected /status 401 without auth, got %d", statusRec.Code)
	}

	// 3. /status con credenciales -> 200 y teléfono enmascarado
	authReq := httptest.NewRequest("GET", "/status", nil)
	authReq.SetBasicAuth("admin", "secretpassword")
	authRec := httptest.NewRecorder()
	handler.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusOK {
		t.Fatalf("expected /status 200 with auth, got %d", authRec.Code)
	}
	if strings.Contains(authRec.Body.String(), "612345678") {
		t.Errorf("expected phone to be masked in /status, got: %s", authRec.Body.String())
	}

	// 4. Ruta inexistente -> 404 (no devuelve index.html)
	notFoundReq := httptest.NewRequest("GET", "/nonexistent", nil)
	notFoundReq.SetBasicAuth("admin", "secretpassword")
	notFoundRec := httptest.NewRecorder()
	handler.ServeHTTP(notFoundRec, notFoundReq)
	if notFoundRec.Code != http.StatusNotFound {
		t.Errorf("expected 404 on unknown route, got %d", notFoundRec.Code)
	}
	if notFoundRec.Header().Get("X-Frame-Options") != "DENY" || notFoundRec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing security headers: %+v", notFoundRec.Header())
	}
}

func TestWebUI_MethodsCSRFAndSecretsMasking(t *testing.T) {
	handler, _ := setupTestWebUI(t, config.AuthConfig{AuthDisabled: true})

	// 1. Métodos GET en endpoints mutadores -> 405 Method Not Allowed
	postOnlyEndpoints := []string{"/start", "/stop", "/verify_phone", "/start_login", "/complete_login", "/save_telegram", "/test_telegram", "/unlink"}
	for _, ep := range postOnlyEndpoints {
		req := httptest.NewRequest("GET", ep, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: expected 405, got %d", ep, rec.Code)
		}
	}

	// 2. CSRF: Origin distinto a Host -> 403 Forbidden
	csrfReq := httptest.NewRequest("POST", "/start", strings.NewReader("{}"))
	csrfReq.Host = "localhost:5000"
	csrfReq.Header.Set("Origin", "http://evil.example.com")
	csrfReq.Header.Set("Content-Type", "application/json")
	csrfRec := httptest.NewRecorder()
	handler.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on cross-origin POST, got %d", csrfRec.Code)
	}

	// 3. POST /verify_phone NO debe devolver telegram_bot_token en claro
	vpReq := httptest.NewRequest("POST", "/verify_phone", strings.NewReader(`{"phone":"612345678"}`))
	vpReq.Host = "localhost:5000"
	vpReq.Header.Set("Origin", "http://localhost:5000")
	vpReq.Header.Set("Content-Type", "application/json")
	vpRec := httptest.NewRecorder()
	handler.ServeHTTP(vpRec, vpReq)
	if vpRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /verify_phone, got %d: %s", vpRec.Code, vpRec.Body.String())
	}
	rawBody := vpRec.Body.String()
	if strings.Contains(rawBody, "SecretBotTokenValueXYZ") || strings.Contains(rawBody, `"telegram_bot_token"`) {
		t.Errorf("/verify_phone leaked plaintext telegram_bot_token: %s", rawBody)
	}

	var vpData map[string]interface{}
	_ = json.Unmarshal(vpRec.Body.Bytes(), &vpData)
	if vpData["telegram_configured"] != true {
		t.Errorf("expected telegram_configured=true, got %v", vpData["telegram_configured"])
	}

	// 4. Guardar en /save_telegram dejando bot_token vacío conserva el token existente sin sobrescribirlo
	saveReq := httptest.NewRequest("POST", "/save_telegram", strings.NewReader(`{"phone":"612345678","bot_token":"","chat_id":"-100999999"}`))
	saveReq.Header.Set("Content-Type", "application/json")
	saveRec := httptest.NewRecorder()
	handler.ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /save_telegram, got %d: %s", saveRec.Code, saveRec.Body.String())
	}
	reloaded := config.LoadAccountConfig("612345678")
	if reloaded.TelegramBotToken != "123456789:SecretBotTokenValueXYZ" || reloaded.TelegramChatID != "-100999999" {
		t.Errorf("expected existing bot token to be preserved and chat_id updated, got: %+v", reloaded)
	}

	// 5. Intentar guardar bot_token con comillas o salto de línea -> 400 Bad Request
	badSaveReq := httptest.NewRequest("POST", "/save_telegram", strings.NewReader(`{"phone":"612345678","bot_token":"123:abc\"def","chat_id":"-100999999"}`))
	badSaveReq.Header.Set("Content-Type", "application/json")
	badSaveRec := httptest.NewRecorder()
	handler.ServeHTTP(badSaveRec, badSaveReq)
	if badSaveRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on invalid bot_token with quotes, got %d", badSaveRec.Code)
	}

	// 6. Límite de body (> 1 MB) -> 400 Bad Request
	hugeJSON := `{"phone":"` + strings.Repeat("6", (1<<20)+100) + `"}`
	hugeReq := httptest.NewRequest("POST", "/verify_phone", bytes.NewReader([]byte(hugeJSON)))
	hugeReq.Header.Set("Content-Type", "application/json")
	hugeRec := httptest.NewRecorder()
	handler.ServeHTTP(hugeRec, hugeReq)
	if hugeRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on >1MB JSON body, got %d", hugeRec.Code)
	}
}
