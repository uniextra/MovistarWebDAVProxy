package auth

import (
	"crypto/sha256"
	"encoding/base64"
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
