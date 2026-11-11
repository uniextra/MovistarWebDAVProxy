package config

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizePhone(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"+34 612 34 56 78", "612345678"},
		{"34612345678", "612345678"},
		{"612-345-678", "612345678"},
		{"34123", "34123"}, // Menos de 9 dígitos no recorta "34"
		{"abc", ""},
	}
	for _, tc := range tests {
		if got := SanitizePhone(tc.input); got != tc.want {
			t.Errorf("SanitizePhone(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestValidateTelegramCredentials(t *testing.T) {
	if !ValidateBotToken("123456789:ABCdefGHI_jkl-MNO") {
		t.Error("expected valid bot token to pass")
	}
	if ValidateBotToken("invalid-token") {
		t.Error("expected invalid bot token to fail")
	}
	if ValidateBotToken("123456:ABC\"DEF\nINJECT") {
		t.Error("expected bot token with quotes/newline to fail")
	}

	if !ValidateChatID("-100123456789") || !ValidateChatID("12345678") || !ValidateChatID("@my_channel_name") {
		t.Error("expected valid chat IDs to pass")
	}
	if ValidateChatID("bad chat id") || ValidateChatID("@ab") {
		t.Error("expected invalid chat IDs to fail")
	}
}

func TestSaveAndLoadAccountConfig_RoundTripAndInjection(t *testing.T) {
	tmpDir := t.TempDir()
	origTokensDir := TokensDir
	TokensDir = tmpDir
	defer func() { TokensDir = origTokensDir }()

	// 1. Crear un .env legacy para verificar que SaveAccountConfig lo limpia al guardar account_<phone>.env
	legacyEnv := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(legacyEnv, []byte(`MOVISTAR_PHONE="600000000"`+"\n"), 0600); err != nil {
		t.Fatalf("failed writing legacy .env: %v", err)
	}

	cfg := AccountConfig{
		Provider:         "o2",
		Phone:            "+34 611 22 33 44",
		JSessionID:       "jsess-123",
		ValidationKey:    "valkey-456",
		DeviceID:         "fol-abc",
		DeviceName:       `Mi "PC" \ Especial`,
		RefreshToken:     "ref-token-xyz",
		AccessToken:      "acc-token-uvw",
		LastRefreshDate:  1700000000000,
		ExpiresIn:        "7200",
		TelegramBotToken: "987654321:XYZ_abc-123",
		TelegramChatID:   "-100998877",
	}

	if err := SaveAccountConfig(cfg); err != nil {
		t.Fatalf("SaveAccountConfig failed: %v", err)
	}

	// El .env legacy debe haberse eliminado para evitar duplicidad / desincronización RTR
	if _, err := os.Stat(legacyEnv); !os.IsNotExist(err) {
		t.Errorf("expected legacy .env to be removed after saving account_<phone>.env")
	}

	loaded := LoadAccountConfig("611223344")
	if loaded.Provider != "o2" {
		t.Errorf("Provider = %q, want o2", loaded.Provider)
	}
	if loaded.Phone != "611223344" {
		t.Errorf("Phone = %q, want 611223344", loaded.Phone)
	}
	if loaded.DeviceName != `Mi "PC" \ Especial` {
		t.Errorf("DeviceName = %q, want %q", loaded.DeviceName, `Mi "PC" \ Especial`)
	}
	if loaded.RefreshToken != "ref-token-xyz" || loaded.AccessToken != "acc-token-uvw" {
		t.Errorf("tokens mismatch: %+v", loaded)
	}
	if loaded.TelegramBotToken != "987654321:XYZ_abc-123" || loaded.TelegramChatID != "-100998877" {
		t.Errorf("telegram mismatch: %+v", loaded)
	}

	// Carga sin teléfono (como en el arranque de main.go) debe encontrar account_611223344.env
	loadedDefault := LoadAccountConfig("")
	if loadedDefault.Phone != "611223344" || loadedDefault.RefreshToken != "ref-token-xyz" {
		t.Errorf("LoadAccountConfig(\"\") failed to load saved account: %+v", loadedDefault)
	}

	// 2. Intentar inyección de salto de línea en un campo libre
	badCfg := cfg
	badCfg.DeviceName = "PC\nMOVISTAR_REFRESH_TOKEN=\"hacked\""
	if err := SaveAccountConfig(badCfg); err == nil {
		t.Errorf("expected SaveAccountConfig to reject newline injection")
	}

	// 3. Intentar token de Telegram con comillas o salto de línea
	badTg := cfg
	badTg.TelegramBotToken = "12345:abc\"\nINJECTED=\"1"
	if err := SaveAccountConfig(badTg); err == nil {
		t.Errorf("expected SaveAccountConfig to reject invalid telegram bot token")
	}

	// Verificar que el fichero en disco no contiene la cadena inyectada
	rawBytes, err := os.ReadFile(filepath.Join(tmpDir, "account_611223344.env"))
	if err != nil {
		t.Fatalf("failed reading account file: %v", err)
	}
	if strings.Contains(string(rawBytes), "INJECTED") || strings.Contains(string(rawBytes), "hacked") {
		t.Errorf("injected content found in env file: %s", string(rawBytes))
	}
}

func TestAuthConfigAndRateLimiter(t *testing.T) {
	pass := "s3cr3t-P@ss"
	sum := sha256.Sum256([]byte(pass))
	hashHex := hex.EncodeToString(sum[:])

	cfgPlain := AuthConfig{
		Username: "admin",
		Password: pass,
	}
	if !cfgPlain.IsEnabled() {
		t.Fatal("expected cfgPlain to be enabled")
	}
	if !cfgPlain.Validate("admin", pass) {
		t.Error("expected valid plain credentials to succeed")
	}
	if cfgPlain.Validate("admin", "wrong") || cfgPlain.Validate("other", pass) {
		t.Error("expected wrong credentials to fail")
	}

	cfgHash := AuthConfig{
		Username:     "admin",
		PasswordHash: hashHex,
	}
	if !cfgHash.Validate("admin", pass) {
		t.Error("expected valid sha256 password hash to succeed")
	}
	if cfgHash.Validate("admin", "wrong") {
		t.Error("expected wrong password against hash to fail")
	}

	rl := NewAuthRateLimiter(3, 50*time.Millisecond)
	ip := "192.0.2.10"
	if rl.IsBlocked(ip) {
		t.Error("IP should not be blocked initially")
	}
	rl.RecordFailure(ip)
	rl.RecordFailure(ip)
	if rl.IsBlocked(ip) {
		t.Error("IP should not be blocked after 2 failures (limit 3)")
	}
	if !rl.RecordFailure(ip) || !rl.IsBlocked(ip) {
		t.Error("IP should be blocked on 3rd failure")
	}
	time.Sleep(60 * time.Millisecond)
	if rl.IsBlocked(ip) {
		t.Error("IP should unblock after window expiration")
	}
}

func TestExtractClientIP(t *testing.T) {
	r1 := httptest.NewRequest("GET", "/", nil)
	r1.RemoteAddr = "192.168.1.50:54321"
	if got := ExtractClientIP(r1); got != "192.168.1.50" {
		t.Errorf("ExtractClientIP IPv4 = %q, want 192.168.1.50", got)
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "[2001:db8::1]:8080"
	if got := ExtractClientIP(r2); got != "2001:db8::1" {
		t.Errorf("ExtractClientIP IPv6 = %q, want 2001:db8::1", got)
	}
}

func TestTokensEncryptionKey_AESGCM(t *testing.T) {
	tmpDir := t.TempDir()
	origTokensDir := TokensDir
	TokensDir = tmpDir
	defer func() { TokensDir = origTokensDir }()

	t.Setenv("TOKENS_ENCRYPTION_KEY", "mi-clave-maestra-super-secreta-2026")

	cfg := AccountConfig{
		Provider:         "movistar",
		Phone:            "699887766",
		JSessionID:       "plain-jsession-secret",
		ValidationKey:    "plain-valkey-secret",
		RefreshToken:     "plain-refresh-token-secret",
		AccessToken:      "plain-access-token-secret",
		TelegramBotToken: "11223344:AABBCCDDEEFF_secret",
		TelegramChatID:   "12345678",
	}

	if err := SaveAccountConfig(cfg); err != nil {
		t.Fatalf("SaveAccountConfig with encryption failed: %v", err)
	}

	envFile := filepath.Join(tmpDir, "account_699887766.env")
	rawBytes, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("failed reading encrypted env file: %v", err)
	}
	rawStr := string(rawBytes)

	// Verificar que ningún secreto aparece en texto plano en el disco y que usan prefijo enc:v1:
	for _, secret := range []string{"plain-jsession-secret", "plain-valkey-secret", "plain-refresh-token-secret", "plain-access-token-secret", "11223344:AABBCCDDEEFF_secret"} {
		if strings.Contains(rawStr, secret) {
			t.Errorf("plaintext secret %q leaked on disk when TOKENS_ENCRYPTION_KEY is set:\n%s", secret, rawStr)
		}
	}
	if !strings.Contains(rawStr, `MOVISTAR_REFRESH_TOKEN="enc:v1:`) {
		t.Errorf("expected enc:v1: prefix in saved file, got:\n%s", rawStr)
	}

	// Verificar que LoadAccountConfig descifra correctamente con la clave correcta
	loaded := LoadAccountConfig("699887766")
	if loaded.RefreshToken != cfg.RefreshToken || loaded.AccessToken != cfg.AccessToken ||
		loaded.ValidationKey != cfg.ValidationKey || loaded.JSessionID != cfg.JSessionID ||
		loaded.TelegramBotToken != cfg.TelegramBotToken {
		t.Errorf("decrypted config mismatch: got %+v", loaded)
	}

	// Verificar que con una clave incorrecta NO se descifran los secretos
	t.Setenv("TOKENS_ENCRYPTION_KEY", "clave-incorrecta")
	wrongLoaded := LoadAccountConfig("699887766")
	if wrongLoaded.RefreshToken != "" || wrongLoaded.AccessToken != "" {
		t.Errorf("expected empty tokens when decryption key is wrong, got: %+v", wrongLoaded)
	}
}

func TestGetSyncInterval(t *testing.T) {
	t.Setenv("SYNC_INTERVAL_SEC", "")
	if got := GetSyncInterval(); got != 60*time.Second {
		t.Errorf("expected default 60s, got %v", got)
	}

	t.Setenv("SYNC_INTERVAL_SEC", "30")
	if got := GetSyncInterval(); got != 30*time.Second {
		t.Errorf("expected 30s, got %v", got)
	}

	t.Setenv("SYNC_INTERVAL_SEC", "5") // below min 15s
	if got := GetSyncInterval(); got != 15*time.Second {
		t.Errorf("expected clamped min 15s, got %v", got)
	}

	t.Setenv("SYNC_INTERVAL_SEC", "invalid")
	if got := GetSyncInterval(); got != 60*time.Second {
		t.Errorf("expected default 60s on invalid, got %v", got)
	}
}
