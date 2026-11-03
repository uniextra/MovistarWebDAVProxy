package config

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeUsername(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Standard username", "admin", "admin"},
		{"DOMAIN\\username", `CORP\admin`, "admin"},
		{"\\username", `\admin`, "admin"},
		{"Multiple backslashes", `DOMAIN\SUB\admin`, "admin"},
		{"Whitespace around username", "  admin  ", "admin"},
		{"Whitespace around DOMAIN\\username", "  CORP\\  admin   ", "admin"},
		{"Empty username", "", ""},
		{"Whitespace only", "    ", ""},
		{"Case preserved", `PC\AliceAdmin`, "AliceAdmin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeUsername(tt.input)
			if got != tt.expected {
				t.Errorf("NormalizeUsername(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestParseBasicAuthHeader(t *testing.T) {
	tests := []struct {
		name          string
		authHeader    string
		wantRawUser   string
		wantCleanUser string
		wantPassword  string
		wantOK        bool
	}{
		{
			name:          "Standard username and password",
			authHeader:    "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret123")),
			wantRawUser:   "alice",
			wantCleanUser: "alice",
			wantPassword:  "secret123",
			wantOK:        true,
		},
		{
			name:          "DOMAIN\\username normalization",
			authHeader:    "Basic " + base64.StdEncoding.EncodeToString([]byte(`CORP\alice:secret123`)),
			wantRawUser:   `CORP\alice`,
			wantCleanUser: "alice",
			wantPassword:  "secret123",
			wantOK:        true,
		},
		{
			name:          "\\username normalization",
			authHeader:    "Basic " + base64.StdEncoding.EncodeToString([]byte(`\alice:secret123`)),
			wantRawUser:   `\alice`,
			wantCleanUser: "alice",
			wantPassword:  "secret123",
			wantOK:        true,
		},
		{
			name:          "Username with leading and trailing whitespace",
			authHeader:    "Basic " + base64.StdEncoding.EncodeToString([]byte("  alice   :secret123")),
			wantRawUser:   "  alice   ",
			wantCleanUser: "alice",
			wantPassword:  "secret123",
			wantOK:        true,
		},
		{
			name:          "Password containing multiple colons preserved exactly",
			authHeader:    "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret:with:extra:colons::")),
			wantRawUser:   "alice",
			wantCleanUser: "alice",
			wantPassword:  "secret:with:extra:colons::",
			wantOK:        true,
		},
		{
			name:          "Password with leading/trailing spaces preserved untouched",
			authHeader:    "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:   spaced_pass   ")),
			wantRawUser:   "alice",
			wantCleanUser: "alice",
			wantPassword:  "   spaced_pass   ",
			wantOK:        true,
		},
		{
			name:          "Empty username allowed by RFC",
			authHeader:    "Basic " + base64.StdEncoding.EncodeToString([]byte(":secret_only")),
			wantRawUser:   "",
			wantCleanUser: "",
			wantPassword:  "secret_only",
			wantOK:        true,
		},
		{
			name:       "Invalid Base64 payload",
			authHeader: "Basic !@#invalid_base64_payload$$",
			wantOK:     false,
		},
		{
			name:       "Missing header",
			authHeader: "",
			wantOK:     false,
		},
		{
			name:       "Non-Basic authorization scheme",
			authHeader: "Bearer some_jwt_or_bearer_token",
			wantOK:     false,
		},
		{
			name:       "Base64 without colon separator",
			authHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("username_without_colon")),
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawUser, cleanUser, pass, ok := ParseBasicAuthHeader(tt.authHeader)
			if ok != tt.wantOK {
				t.Fatalf("ParseBasicAuthHeader() ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if rawUser != tt.wantRawUser {
				t.Errorf("rawUser = %q, want %q", rawUser, tt.wantRawUser)
			}
			if cleanUser != tt.wantCleanUser {
				t.Errorf("cleanUser = %q, want %q", cleanUser, tt.wantCleanUser)
			}
			if pass != tt.wantPassword {
				t.Errorf("password = %q, want %q", pass, tt.wantPassword)
			}
		})
	}
}

func TestAuthConfigValidate_NormalizationAndColons(t *testing.T) {
	cfg := AuthConfig{
		Username: "davuser",
		Password: "complex:password:with:colons",
	}

	tests := []struct {
		name     string
		user     string
		pass     string
		expected bool
	}{
		{"Standard user and pass", "davuser", "complex:password:with:colons", true},
		{"DOMAIN\\user", `DESKTOP-ABC\davuser`, "complex:password:with:colons", true},
		{"\\user", `\davuser`, "complex:password:with:colons", true},
		{"User with spaces", "   davuser   ", "complex:password:with:colons", true},
		{"UPN format user@domain", "davuser@domain.local", "complex:password:with:colons", true},
		{"Wrong user", "otheruser", "complex:password:with:colons", false},
		{"Wrong password", "davuser", "wrongpassword", false},
		{"Truncated password missing colon parts", "davuser", "complex:password", false},
		{"Empty user", "", "complex:password:with:colons", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.Validate(tt.user, tt.pass); got != tt.expected {
				t.Errorf("cfg.Validate(%q, %q) = %v, want %v", tt.user, tt.pass, got, tt.expected)
			}
		})
	}
}

func TestIPMatcher_IndividualAndCIDR(t *testing.T) {
	matcher := ParseIPMatcher("127.0.0.1,::1,172.17.0.0/16,172.18.0.0/16,2001:db8::/32")

	tests := []struct {
		ip       string
		expected bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"172.17.0.1", true},
		{"172.17.42.99", true},
		{"172.18.0.1", true},
		{"172.18.254.10", true},
		{"::ffff:172.18.0.1", true}, // IPv4-mapped IPv6
		{"2001:db8::1", true},
		{"2001:db8:ffff::1", true},
		{"172.19.0.1", false},
		{"192.168.1.1", false},
		{"203.0.113.195", false},
		{"2001:db9::1", false},
		{"invalid-ip", false},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			if got := matcher.Matches(tt.ip); got != tt.expected {
				t.Errorf("matcher.Matches(%q) = %v, want %v", tt.ip, got, tt.expected)
			}
		})
	}
}

func TestExtractClientIPWithTrusted_SpoofProtection(t *testing.T) {
	trusted := ParseIPMatcher("127.0.0.1,172.18.0.2")

	// 1. Direct connection from external client attempting to spoof X-Forwarded-For
	r1 := httptest.NewRequest("GET", "/", nil)
	r1.RemoteAddr = "203.0.113.50:12345"
	r1.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	r1.Header.Set("X-Real-IP", "9.10.11.12")

	if got := ExtractClientIPWithTrusted(r1, trusted); got != "203.0.113.50" {
		t.Errorf("Direct connection spoof attempt: got %q, want %q", got, "203.0.113.50")
	}

	// 2. Connection from trusted reverse proxy with X-Forwarded-For
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "172.18.0.2:54321"
	r2.Header.Set("X-Forwarded-For", "198.51.100.25")

	if got := ExtractClientIPWithTrusted(r2, trusted); got != "198.51.100.25" {
		t.Errorf("Trusted proxy X-Forwarded-For: got %q, want %q", got, "198.51.100.25")
	}

	// 3. Connection from trusted reverse proxy with multi-hop X-Forwarded-For
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.RemoteAddr = "172.18.0.2:54321"
	r3.Header.Set("X-Forwarded-For", "198.51.100.25, 172.18.0.2")

	if got := ExtractClientIPWithTrusted(r3, trusted); got != "198.51.100.25" {
		t.Errorf("Trusted proxy multi-hop X-Forwarded-For: got %q, want %q", got, "198.51.100.25")
	}

	// 4. Connection from trusted reverse proxy with X-Real-IP
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.RemoteAddr = "127.0.0.1:45678"
	r4.Header.Set("X-Real-IP", "198.51.100.88")

	if got := ExtractClientIPWithTrusted(r4, trusted); got != "198.51.100.88" {
		t.Errorf("Trusted proxy X-Real-IP: got %q, want %q", got, "198.51.100.88")
	}

	// 5. Normalization of IPv4-mapped IPv6 address
	r5 := httptest.NewRequest("GET", "/", nil)
	r5.RemoteAddr = "[::ffff:192.168.1.55]:9999"
	if got := ExtractClientIPWithTrusted(r5, trusted); got != "192.168.1.55" {
		t.Errorf("IPv4-mapped IPv6 normalization: got %q, want %q", got, "192.168.1.55")
	}
}

func TestAuthRateLimiter_DockerGatewayAndLoopbackExclusions(t *testing.T) {
	// Excluir Docker subnets y localhost de bloqueos por fuerza bruta
	exclusions := ParseIPMatcher("127.0.0.1,::1,172.18.0.0/16")
	rl := NewAuthRateLimiterWithExclusions(3, 50*time.Millisecond, exclusions)

	dockerGateway := "172.18.0.1"
	externalAttacker := "203.0.113.195"

	// Docker gateway: simular 10 intentos fallidos de múltiples clientes distintos
	for i := 0; i < 10; i++ {
		if blocked := rl.RecordFailure(dockerGateway); blocked {
			t.Errorf("Docker gateway %s must NOT be blocked (attempt %d)", dockerGateway, i+1)
		}
	}
	if rl.IsBlocked(dockerGateway) {
		t.Errorf("Docker gateway %s must NOT be marked as blocked", dockerGateway)
	}

	// External client: debe bloquearse al 3er intento
	if rl.RecordFailure(externalAttacker) {
		t.Error("1st failure should not block")
	}
	if rl.RecordFailure(externalAttacker) {
		t.Error("2nd failure should not block")
	}
	if !rl.RecordFailure(externalAttacker) {
		t.Error("3rd failure MUST block external attacker")
	}
	if !rl.IsBlocked(externalAttacker) {
		t.Error("external attacker MUST be blocked")
	}

	// Loopback exclusion
	loopback := "127.0.0.1"
	for i := 0; i < 10; i++ {
		if blocked := rl.RecordFailure(loopback); blocked {
			t.Errorf("Loopback %s must NOT be blocked", loopback)
		}
	}
	if rl.IsBlocked(loopback) {
		t.Errorf("Loopback %s must NOT be marked as blocked", loopback)
	}
}
