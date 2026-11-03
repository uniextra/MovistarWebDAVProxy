package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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

// NormalizeUsername normaliza el nombre de usuario eliminando prefijos de dominio estilo Windows (ej. "DOMAIN\user", "\user")
// y espacios en blanco al inicio o al final:
//   - DOMAIN\username -> username
//   - \username       -> username
//   - username        -> username
func NormalizeUsername(rawUser string) string {
	parts := strings.Split(rawUser, `\`)
	clean := parts[len(parts)-1]
	return strings.TrimSpace(clean)
}

// ParseBasicAuthHeader extrae y normaliza credenciales desde la cabecera HTTP "Authorization: Basic".
// Preserva la contraseña intacta sin alterar espacios ni caracteres especiales (incluyendo dos puntos ":").
// Proporciona:
//   - rawUser: nombre de usuario original sin normalizar (para auditoría y logs enmascarados).
//   - cleanUser: nombre de usuario normalizado (para validación y lookup de cuenta).
//   - password: contraseña original exacta (nunca recortada ni normalizada).
//   - ok: true si la cabecera es válida y pudo decodificarse en Base64 con formato user:password.
func ParseBasicAuthHeader(authHeader string) (rawUser, cleanUser, password string, ok bool) {
	if authHeader == "" {
		return "", "", "", false
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Basic") {
		return "", "", "", false
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		return "", "", "", false
	}
	decoded := string(payload)
	colonIdx := strings.IndexByte(decoded, ':')
	if colonIdx == -1 {
		return "", "", "", false
	}
	rawUser = decoded[:colonIdx]
	password = decoded[colonIdx+1:]
	cleanUser = NormalizeUsername(rawUser)
	return rawUser, cleanUser, password, true
}

// Validate comprueba usuario y contraseña en tiempo constante usando SHA-256.
// Soporta prefijos de dominio de Windows WebClient (ej. "DOMAIN\usuario" o "\usuario")
// mediante NormalizeUsername, y compara en tiempo constante contra a.Username.
func (a AuthConfig) Validate(user, pass string) bool {
	if !a.IsEnabled() {
		return true
	}

	cleanUser := NormalizeUsername(user)
	expectedUser := NormalizeUsername(a.Username)
	if expectedUser == "" {
		return false
	}

	// Soporte de formato UPN (usuario@dominio) si el usuario configurado no contiene @
	if !strings.Contains(expectedUser, "@") {
		if idx := strings.Index(cleanUser, "@"); idx != -1 {
			cleanUser = cleanUser[:idx]
		}
	}

	userHash := sha256.Sum256([]byte(cleanUser))
	expectedUserHash := sha256.Sum256([]byte(expectedUser))
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

// IPMatcher comprueba si una dirección IP coincide con una lista de IPs individuales o bloques CIDR.
type IPMatcher struct {
	ips   []net.IP
	cidrs []*net.IPNet
}

// ParseIPMatcher analiza una lista separada por comas de IPs (IPv4/IPv6) y bloques CIDR.
func ParseIPMatcher(list string) *IPMatcher {
	m := &IPMatcher{}
	for _, raw := range strings.Split(list, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			_, ipnet, err := net.ParseCIDR(item)
			if err == nil && ipnet != nil {
				m.cidrs = append(m.cidrs, ipnet)
			}
		} else {
			ip := net.ParseIP(item)
			if ip != nil {
				if v4 := ip.To4(); v4 != nil {
					ip = v4
				}
				m.ips = append(m.ips, ip)
			}
		}
	}
	return m
}

// Matches indica si la dirección IP dada (en string) coincide con alguna IP o rango CIDR del matcher.
func (m *IPMatcher) Matches(ipStr string) bool {
	if m == nil {
		return false
	}
	clean := NormalizeIP(ipStr)
	parsed := net.ParseIP(clean)
	if parsed == nil {
		return false
	}
	if v4 := parsed.To4(); v4 != nil {
		parsed = v4
	}
	for _, ip := range m.ips {
		if ip.Equal(parsed) {
			return true
		}
	}
	for _, cidr := range m.cidrs {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

// NormalizeIP normaliza una dirección IP IPv4 o IPv6:
// - Desenvuelve direcciones IPv6 mapeadas a IPv4 (ej. "::ffff:192.168.1.1" -> "192.168.1.1").
// - Remueve corchetes de direcciones IPv6 (ej. "[2001:db8::1]" -> "2001:db8::1").
// - Recorta espacios en blanco.
func NormalizeIP(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// ExtractPeerIP extrae la dirección IP del socket TCP (RemoteAddr) limpia de puerto y normalizada.
func ExtractPeerIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return NormalizeIP(host)
	}
	return NormalizeIP(remoteAddr)
}

// LoadTrustedProxies carga la lista de proxies de confianza desde la variable de entorno TRUSTED_PROXIES.
func LoadTrustedProxies() *IPMatcher {
	raw := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES"))
	if raw == "" {
		return nil
	}
	return ParseIPMatcher(raw)
}

// ExtractClientIP obtiene la IP del cliente respetando proxies de confianza:
// 1. Usa la IP del socket remoto (RemoteAddr) como fuente de confianza por defecto.
// 2. Solo si la IP remota coincide con un proxy de confianza (TRUSTED_PROXIES), evalúa cabeceras X-Forwarded-For o X-Real-IP.
// 3. Normaliza direcciones IPv4 e IPv6 (incluyendo IPv4-mapped IPv6).
func ExtractClientIP(r *http.Request) string {
	return ExtractClientIPWithTrusted(r, LoadTrustedProxies())
}

// ExtractClientIPWithTrusted implementa la resolución de IP permitiendo inyectar el matcher de proxies de confianza.
func ExtractClientIPWithTrusted(r *http.Request, trusted *IPMatcher) string {
	peerIP := ExtractPeerIP(r.RemoteAddr)
	if trusted == nil || !trusted.Matches(peerIP) {
		// Origen directo o proxy no confiable: NUNCA se confía en X-Forwarded-For ni X-Real-IP
		return peerIP
	}

	// El origen inmediato es un proxy de confianza: podemos evaluar cabeceras de reenvío
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// Buscar de izquierda a derecha la primera IP que NO sea un proxy de confianza
		for _, p := range parts {
			ipStr := NormalizeIP(p)
			if ip := net.ParseIP(ipStr); ip != nil {
				if !trusted.Matches(ipStr) {
					return ipStr
				}
			}
		}
		// Si todas son proxies de confianza, tomar la primera válida
		for _, p := range parts {
			ipStr := NormalizeIP(p)
			if ip := net.ParseIP(ipStr); ip != nil {
				return ipStr
			}
		}
	}

	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		ipStr := NormalizeIP(xri)
		if ip := net.ParseIP(ipStr); ip != nil {
			return ipStr
		}
	}

	return peerIP
}

// DefaultBruteForceExcludedSubnets subredes puente estándar de Docker excluidas por defecto para evitar bloqueos globales.
const DefaultBruteForceExcludedSubnets = "127.0.0.1,::1,172.17.0.0/16,172.18.0.0/16"

// LoadBruteForceExclusions carga las IPs y subredes excluidas de bloqueo por fuerza bruta.
// Permite que peticiones originadas desde el gateway de Docker o localhost no queden bloqueadas con 429,
// sin eludir la autenticación Basic Auth.
func LoadBruteForceExclusions() *IPMatcher {
	raw := strings.TrimSpace(os.Getenv("BRUTE_FORCE_EXCLUDED_IPS"))
	if raw == "" {
		raw = DefaultBruteForceExcludedSubnets
	} else {
		// Asegurar que loopback siempre esté incluido incluso si se definen subredes personalizadas
		if !strings.Contains(raw, "127.0.0.1") {
			raw = "127.0.0.1,::1," + raw
		}
	}
	return ParseIPMatcher(raw)
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
	exclusions  *IPMatcher
}

// NewAuthRateLimiter crea un limitador de fallos por IP con las exclusiones por defecto de Docker/localhost.
func NewAuthRateLimiter(maxFailures int, window time.Duration) *AuthRateLimiter {
	return NewAuthRateLimiterWithExclusions(maxFailures, window, LoadBruteForceExclusions())
}

// NewAuthRateLimiterWithExclusions crea un limitador de fallos con un matcher de exclusión explícito.
func NewAuthRateLimiterWithExclusions(maxFailures int, window time.Duration, exclusions *IPMatcher) *AuthRateLimiter {
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
		exclusions:  exclusions,
	}
}

// IsExcluded comprueba si una IP está exenta del bloqueo por fuerza bruta (ej. Docker gateway o localhost).
func (rl *AuthRateLimiter) IsExcluded(ipStr string) bool {
	cleanIP := NormalizeIP(ipStr)
	parsed := net.ParseIP(cleanIP)
	if parsed != nil && parsed.IsLoopback() {
		return true
	}
	if rl.exclusions != nil && rl.exclusions.Matches(cleanIP) {
		return true
	}
	return false
}

// IsBlocked comprueba si una IP ha superado el límite de fallos permitidos en la ventana actual.
func (rl *AuthRateLimiter) IsBlocked(ip string) bool {
	if rl.IsExcluded(ip) {
		return false
	}

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
	if rl.IsExcluded(ip) {
		return false
	}

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
	if rl.IsExcluded(ip) {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.entries, ip)
}
