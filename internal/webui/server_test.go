package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	// 4b. Guardar en /save_telegram con phone vacío recurre a la cuenta activa (Issue #10)
	saveEmptyPhoneReq := httptest.NewRequest("POST", "/save_telegram", strings.NewReader(`{"phone":"","bot_token":"","chat_id":"-100888888"}`))
	saveEmptyPhoneReq.Header.Set("Content-Type", "application/json")
	saveEmptyPhoneRec := httptest.NewRecorder()
	handler.ServeHTTP(saveEmptyPhoneRec, saveEmptyPhoneReq)
	if saveEmptyPhoneRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /save_telegram with empty phone fallback, got %d: %s", saveEmptyPhoneRec.Code, saveEmptyPhoneRec.Body.String())
	}
	reloadedFallback := config.LoadAccountConfig("612345678")
	if reloadedFallback.TelegramChatID != "-100888888" {
		t.Errorf("expected chat_id updated via fallback, got: %s", reloadedFallback.TelegramChatID)
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

func TestWebUI_UnlinkActiveAndSecondary(t *testing.T) {
	tmpDir := t.TempDir()
	origTokensDir := config.TokensDir
	config.TokensDir = tmpDir
	t.Cleanup(func() { config.TokensDir = origTokensDir })

	// Create active account (612345678) and secondary account (699999999)
	activeCfg := config.AccountConfig{
		Provider: "movistar",
		Phone:    "612345678",
	}
	if err := config.SaveAccountConfig(activeCfg); err != nil {
		t.Fatalf("SaveAccountConfig active failed: %v", err)
	}
	secCfg := config.AccountConfig{
		Provider: "o2",
		Phone:    "699999999",
	}
	if err := config.SaveAccountConfig(secCfg); err != nil {
		t.Fatalf("SaveAccountConfig secondary failed: %v", err)
	}

	// Create dummy metadata DB files for both
	_ = os.WriteFile(filepath.Join(tmpDir, "metadata_612345678.db"), []byte("active-db"), 0600)
	_ = os.WriteFile(filepath.Join(tmpDir, "metadata_699999999.db"), []byte("sec-db"), 0600)

	client, err := sapi.NewClient(activeCfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	authCfg := config.AuthConfig{AuthDisabled: true}
	davSrv := webdav.NewServerWithAuth(client, authCfg)
	uiSrv := NewServerWithAuth(client, davSrv, authCfg)
	handler := uiSrv.Routes()

	// 1. Unlink secondary account (699999999)
	unlinkSecBody := strings.NewReader(`{"phone":"699999999"}`)
	unlinkSecReq := httptest.NewRequest("POST", "/unlink", unlinkSecBody)
	unlinkSecReq.Header.Set("Content-Type", "application/json")
	unlinkSecRec := httptest.NewRecorder()
	handler.ServeHTTP(unlinkSecRec, unlinkSecReq)
	if unlinkSecRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /unlink secondary, got %d: %s", unlinkSecRec.Code, unlinkSecRec.Body.String())
	}

	// Verify secondary config & DB files are removed
	if _, err := os.Stat(filepath.Join(tmpDir, "account_699999999.env")); !os.IsNotExist(err) {
		t.Errorf("expected secondary account config to be deleted")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "metadata_699999999.db")); !os.IsNotExist(err) {
		t.Errorf("expected secondary metadata DB to be deleted")
	}

	// Verify primary account is still intact and uiSrv client is still active
	if _, err := os.Stat(filepath.Join(tmpDir, "account_612345678.env")); err != nil {
		t.Errorf("expected primary account config to remain, got err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "metadata_612345678.db")); err != nil {
		t.Errorf("expected primary metadata DB to remain, got err: %v", err)
	}
	if uiSrv.client == nil || uiSrv.client.GetConfig().Phone != "612345678" {
		t.Errorf("expected active client to remain active on secondary unlink, got: %v", uiSrv.client)
	}

	// 2. Unlink active account (612345678)
	unlinkActBody := strings.NewReader(`{"phone":"612345678"}`)
	unlinkActReq := httptest.NewRequest("POST", "/unlink", unlinkActBody)
	unlinkActReq.Header.Set("Content-Type", "application/json")
	unlinkActRec := httptest.NewRecorder()
	handler.ServeHTTP(unlinkActRec, unlinkActReq)
	if unlinkActRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /unlink active, got %d: %s", unlinkActRec.Code, unlinkActRec.Body.String())
	}

	// Verify active client config is cleared and proxy is stopped
	if uiSrv.client != nil && uiSrv.client.GetConfig().Phone != "" {
		t.Errorf("expected active client config to be cleared after active unlink, got: %v", uiSrv.client.GetConfig())
	}
	if uiSrv.IsRunning() {
		t.Errorf("expected server to not be running after unlinking active account")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "account_612345678.env")); !os.IsNotExist(err) {
		t.Errorf("expected active account config to be deleted")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "metadata_612345678.db")); !os.IsNotExist(err) {
		t.Errorf("expected active metadata DB to be deleted")
	}
}
