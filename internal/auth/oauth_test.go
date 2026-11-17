package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge := GeneratePKCE()
	if len(verifier) == 0 || len(verifier) > 64 {
		t.Fatalf("unexpected verifier length: %d", len(verifier))
	}
	expectedSum := sha256.Sum256([]byte(verifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(expectedSum[:])
	if challenge != expectedChallenge {
		t.Errorf("PKCE S256 challenge mismatch: got %q, want %q", challenge, expectedChallenge)
	}
}

func TestExtractAuthCodeAndState(t *testing.T) {
	fullURL := "https://micloud.movistar.es/ui/html/clientoauth.html?code=740b4648-abc_123.xyz~9&state=st_987-xyz"
	if got := ExtractAuthCode(fullURL); got != "740b4648-abc_123.xyz~9" {
		t.Errorf("ExtractAuthCode(fullURL) = %q", got)
	}
	state, hasState := ExtractState(fullURL)
	if !hasState || state != "st_987-xyz" {
		t.Errorf("ExtractState(fullURL) = (%q, %v), want (st_987-xyz, true)", state, hasState)
	}

	rawCode := "740b4648-abc_123"
	if got := ExtractAuthCode(rawCode); got != rawCode {
		t.Errorf("ExtractAuthCode(rawCode) = %q, want %q", got, rawCode)
	}
	if _, hasStateRaw := ExtractState(rawCode); hasStateRaw {
		t.Errorf("expected raw code without URL/state= to return hasState=false")
	}

	// Entradas maliciosas o inválidas deben ser rechazadas
	badInputs := []string{
		"",
		"code with spaces",
		"abc\"def\ninjected",
		"<script>alert(1)</script>",
		strings.Repeat("a", 600),
	}
	for _, in := range badInputs {
		if got := ExtractAuthCode(in); got != "" {
			t.Errorf("ExtractAuthCode(%q) = %q, expected empty string", in, got)
		}
	}
}

func TestBuildAndParseOAuthHeader(t *testing.T) {
	hdr := BuildOAuthHeader("acc-123", "ref-456", 1700000000, "3600")
	if !strings.HasPrefix(hdr, "oauth ") {
		t.Fatalf("expected oauth prefix, got %q", hdr)
	}

	parsed := ParseOAuthResponseHeader(hdr)
	if parsed == nil {
		t.Fatal("ParseOAuthResponseHeader returned nil")
	}
	if parsed.AccessToken != "acc-123" || parsed.RefreshToken != "ref-456" {
		t.Errorf("parsed tokens mismatch: %+v", parsed)
	}
	// 1700000000 seconds should be normalized to milliseconds (1700000000000)
	if parsed.LastRefreshDate != 1700000000000 {
		t.Errorf("LastRefreshDate = %d, want 1700000000000", parsed.LastRefreshDate)
	}
	if parsed.ExpiresIn != "3600" {
		t.Errorf("ExpiresIn = %q, want 3600", parsed.ExpiresIn)
	}

	if ParseOAuthResponseHeader("Bearer invalid") != nil {
		t.Error("expected nil for non-oauth header")
	}
}

func TestBuildOAuthHeaderMatchesOfficialClientFieldOrder(t *testing.T) {
	header := BuildOAuthHeader("acc-123", "ref-456", 1700000000, "3600")
	encoded := strings.TrimPrefix(header, "oauth ")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode OAuth header: %v", err)
	}

	want := `{"data":{"accesstoken":"acc-123","lastrefreshdate":1700000000000,"expiresin":"3600","refreshtoken":"ref-456","platform":"windows"}}`
	if string(decoded) != want {
		t.Fatalf("OAuth header payload = %s, want %s", decoded, want)
	}

	var parsed map[string]any
	if err := json.Unmarshal(decoded, &parsed); err != nil {
		t.Fatalf("OAuth header is not valid JSON: %v", err)
	}
}

func TestDesktopUserAgentDefault(t *testing.T) {
	t.Setenv("DESKTOP_USER_AGENT", "")
	// User-Agent del cliente oficial de Windows de O2 (capturado, noviembre de 2026).
	const want = "omh windows client 4.0.1"
	if got := GetDesktopUserAgent(); got != want {
		t.Fatalf("GetDesktopUserAgent() = %q, want %q", got, want)
	}
}

func TestSAPIUserAgentDefault(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser")
	t.Setenv("SAPI_USER_AGENT", "")
	const want = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
	if got := GetSAPIUserAgent(); got != want {
		t.Fatalf("GetSAPIUserAgent() = %q, want %q", got, want)
	}
}

func TestGetOAuthUserAgent_DefaultMatchesSAPIUserAgent(t *testing.T) {
	t.Setenv("DESKTOP_USER_AGENT", "")
	t.Setenv("SAPI_USER_AGENT", "")
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	if got, want := getOAuthUserAgent(), GetDesktopUserAgent(); got != want {
		t.Fatalf("getOAuthUserAgent() in desktop profile = %q, want %q", got, want)
	}

	t.Setenv("SAPI_CLIENT_PROFILE", "browser")
	if got, want := getOAuthUserAgent(), SAPIUserAgent; got != want {
		t.Fatalf("getOAuthUserAgent() in browser profile = %q, want %q", got, want)
	}

	t.Setenv("DESKTOP_USER_AGENT", "CustomDesktopUA/1.0")
	if got := getOAuthUserAgent(); got != "CustomDesktopUA/1.0" {
		t.Fatalf("getOAuthUserAgent() with custom DESKTOP_USER_AGENT = %q, want %q", got, "CustomDesktopUA/1.0")
	}
}

func TestSAPILoginOAuth_WebDeviceKeepsWindowsPlatform(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser")
	var sawDeviceID, sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawDeviceID = r.Header.Get("X-deviceid")
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"validationkey":"vk-123"}}`))
	}))
	defer srv.Close()

	res := SAPILoginOAuth(srv.Client(), srv.URL, "acc-token", "ref-token", "web-e833", "PC Windows", 1700000000000, "3600")
	if res.StatusCode != http.StatusOK || res.Error != nil {
		t.Fatalf("SAPILoginOAuth failed: status=%d err=%v", res.StatusCode, res.Error)
	}
	if sawDeviceID != "web-e833" {
		t.Fatalf("X-deviceid = %q, want %q", sawDeviceID, "web-e833")
	}
	if !strings.HasPrefix(sawAuth, "oauth ") {
		t.Fatalf("Authorization = %q, want oauth prefix", sawAuth)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sawAuth, "oauth "))
	if err != nil {
		t.Fatalf("decode Authorization failed: %v", err)
	}
	var payload OAuthCredentialHeader
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("unmarshal OAuth payload failed: %v", err)
	}
	if payload.Data.Platform != ClientPlatform {
		t.Fatalf("OAuth payload platform = %q, want %q", payload.Data.Platform, ClientPlatform)
	}
}

func TestRefreshOAuthToken_BasicAuthFirstAndErrorSummary(t *testing.T) {
	var calls int
	var firstHadBodyClientID, secondHadBodyClientID bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		hasCreds := r.Form.Get("client_id") != "" && r.Form.Get("client_secret") != ""
		if calls == 1 {
			firstHadBodyClientID = hasCreds
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"Invalid Token"}`))
			return
		}
		secondHadBodyClientID = hasCreds
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"Invalid Token"}`))
	}))
	defer srv.Close()

	t.Setenv("OAUTH_TOKEN_URL", srv.URL)

	_, err := RefreshOAuthToken("secret-refresh-token", "movistar")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (Basic-only first, then fallback with body creds)", calls)
	}
	if firstHadBodyClientID {
		t.Errorf("first attempt sent client_id/client_secret in body alongside Basic auth")
	}
	if !secondHadBodyClientID {
		t.Errorf("fallback attempt did not send client_id/client_secret in body")
	}
	if !strings.Contains(err.Error(), "Invalid Token") {
		t.Errorf("error missing server description: %v", err)
	}
	if strings.Contains(err.Error(), "secret-refresh-token") {
		t.Errorf("error leaked refresh token: %v", err)
	}
}
