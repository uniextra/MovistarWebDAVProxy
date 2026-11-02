package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// AuthConfig almacena la configuración de autenticación Basic Auth para un servicio.
type AuthConfig struct {
	Username     string
	Password     string
	PasswordHash string // Hash SHA-256 en hex opcional (con o sin prefijo "sha256:")
	AuthDisabled bool
	RequireAuth  bool
}

// LoadWebDAVAuthConfig carga la configuración de autenticación para el servidor WebDAV.
func LoadWebDAVAuthConfig() AuthConfig {
	user := strings.TrimSpace(os.Getenv("WEBDAV_USER"))
	pass := os.Getenv("WEBDAV_PASSWORD")
	hash := strings.TrimSpace(os.Getenv("WEBDAV_PASSWORD_HASH"))
	disabled := strings.EqualFold(strings.TrimSpace(os.Getenv("AUTH_DISABLED")), "true")
	requireAuth := strings.EqualFold(strings.TrimSpace(os.Getenv("REQUIRE_AUTH")), "true")

	return AuthConfig{
		Username:     user,
		Password:     pass,
		PasswordHash: strings.TrimPrefix(strings.ToLower(hash), "sha256:"),
		AuthDisabled: disabled,
		RequireAuth:  requireAuth,
	}
}

// LoadWebUIAuthConfig carga la configuración de autenticación para el panel WebUI.
// Si no se especifican WEBUI_USER / WEBUI_PASSWORD, reutiliza las de WebDAV.
func LoadWebUIAuthConfig() AuthConfig {
	davCfg := LoadWebDAVAuthConfig()
	user := strings.TrimSpace(os.Getenv("WEBUI_USER"))
	pass := os.Getenv("WEBUI_PASSWORD")
	hash := strings.TrimSpace(os.Getenv("WEBUI_PASSWORD_HASH"))

	if user == "" && pass == "" && hash == "" {
		return davCfg
	}

	return AuthConfig{
		Username:     user,
		Password:     pass,
		PasswordHash: strings.TrimPrefix(strings.ToLower(hash), "sha256:"),
		AuthDisabled: davCfg.AuthDisabled,
		RequireAuth:  davCfg.RequireAuth,
	}
}

// HasCredentials indica si se han configurado credenciales válidas.
func (a AuthConfig) HasCredentials() bool {
	return a.Username != "" && (a.Password != "" || a.PasswordHash != "")
}

// IsEnabled indica si se debe exigir autenticación HTTP Basic en cada petición.
func (a AuthConfig) IsEnabled() bool {
	if a.AuthDisabled {
		return false
	}
	return a.HasCredentials()
}

// Validate comprueba usuario y contraseña en tiempo constante usando SHA-256.
// Soporta prefijos de dominio de Windows WebClient (ej. "PC\usuario" o ".\usuario").
func (a AuthConfig) Validate(user, pass string) bool {
	if !a.IsEnabled() {
		return true
	}

	cleanUser := user
	if !strings.Contains(a.Username, `\`) {
		if idx := strings.LastIndex(cleanUser, `\`); idx != -1 {
			cleanUser = cleanUser[idx+1:]
		}
	}
	if !strings.Contains(a.Username, "@") {
		if idx := strings.Index(cleanUser, "@"); idx != -1 {
			cleanUser = cleanUser[:idx]
		}
	}

	userHash := sha256.Sum256([]byte(cleanUser))
	expectedUserHash := sha256.Sum256([]byte(a.Username))
	userMatch := subtle.ConstantTimeCompare(userHash[:], expectedUserHash[:]) == 1

	var passMatch bool
	if a.PasswordHash != "" {
		expectedHashBytes, err := hex.DecodeString(a.PasswordHash)
		if err == nil && len(expectedHashBytes) == sha256.Size {
			givenPassHash := sha256.Sum256([]byte(pass))
			passMatch = subtle.ConstantTimeCompare(givenPassHash[:], expectedHashBytes) == 1
		}
	} else {
		passHash := sha256.Sum256([]byte(pass))
		expectedPassHash := sha256.Sum256([]byte(a.Password))
		passMatch = subtle.ConstantTimeCompare(passHash[:], expectedPassHash[:]) == 1
	}

	return userMatch && passMatch
}

// ExtractClientIP obtiene la IP limpia del cliente (compatible con IPv4 e IPv6).
func ExtractClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

type authFailEntry struct {
	failures  int
	windowEnd time.Time
}

// AuthRateLimiter limita intentos fallidos de autenticación por IP en una ventana de tiempo.
type AuthRateLimiter struct {
	mu          sync.Mutex
	entries     map[string]*authFailEntry
	maxFailures int
	window      time.Duration
	maxEntries  int
}

// NewAuthRateLimiter crea un limitador de fallos por IP (por defecto 10 fallos/min, máx 1024 IPs).
func NewAuthRateLimiter(maxFailures int, window time.Duration) *AuthRateLimiter {
	if maxFailures <= 0 {
		maxFailures = 10
	}
	if window <= 0 {
		window = time.Minute
	}
	return &AuthRateLimiter{
		entries:     make(map[string]*authFailEntry),
		maxFailures: maxFailures,
		window:      window,
		maxEntries:  1024,
	}
}

// IsBlocked comprueba si una IP ha superado el límite de fallos permitidos en la ventana actual.
func ( rl *AuthRateLimiter) IsBlocked(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	entry, ok := rl.entries[ip]
	if !ok {
		return false
	}
	if now.After(entry.windowEnd) {
		delete(rl.entries, ip)
		return false
	}
	return entry.failures >= rl.maxFailures
}

// RecordFailure registra un fallo de autenticación para una IP y devuelve true si queda bloqueada.
func (rl *AuthRateLimiter) RecordFailure(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	if len(rl.entries) >= rl.maxEntries {
		for k, v := range rl.entries {
			if now.After(v.windowEnd) {
				delete(rl.entries, k)
			}
		}
		if len(rl.entries) >= rl.maxEntries {
			// Evicción simple de una entrada para mantener el mapa acotado
			for k := range rl.entries {
				delete(rl.entries, k)
				break
			}
		}
	}

	entry, ok := rl.entries[ip]
	if !ok || now.After(entry.windowEnd) {
		entry = &authFailEntry{
			failures:  1,
			windowEnd: now.Add(rl.window),
		}
		rl.entries[ip] = entry
		return entry.failures >= rl.maxFailures
	}

	entry.failures++
	return entry.failures >= rl.maxFailures
}

// Reset limpia el contador de fallos de una IP tras un login exitoso.
func (rl *AuthRateLimiter) Reset(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.entries, ip)
}
