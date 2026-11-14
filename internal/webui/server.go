package webui

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/notifier"
	"movistar-webdav-proxy/internal/sapi"
	"movistar-webdav-proxy/internal/webdav"
)

//go:embed templates/index.html
var indexHTML []byte

//go:embed templates/login.html
var loginHTML []byte

var loginTmpl = template.Must(template.New("login").Parse(string(loginHTML)))

const (
	maxBodyBytes       = 1 << 20 // 1 MB
	loginStateTTL      = 10 * time.Minute
	maxPendingLogin    = 50
	sessionCookieName  = "mc_webui_session"
	defaultSessionTTL  = 24 * time.Hour
	rememberSessionTTL = 30 * 24 * time.Hour
	panelCSP           = "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'"
)

type webSession struct {
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type loginPageData struct {
	Version  string
	Error    string
	Username string
}

type Server struct {
	client       *sapi.Client
	webdavServer *webdav.Server
	notifier     *notifier.TelegramNotifier
	loginMu      sync.Mutex
	loginState   map[string]*pendingLogin
	authCfg      config.AuthConfig
	rateLimiter  *config.AuthRateLimiter
	sessionMu    sync.RWMutex
	sessions     map[string]*webSession
}

type pendingLogin struct {
	Provider string
	Verifier string
	State    string
	Time     time.Time
}

func NewServer(client *sapi.Client, webdavServer *webdav.Server) *Server {
	return NewServerWithAuth(client, webdavServer, config.LoadWebUIAuthConfig())
}

func NewServerWithAuth(client *sapi.Client, webdavServer *webdav.Server, authCfg config.AuthConfig) *Server {
	tg := notifier.NewTelegramNotifier("", "")
	s := &Server{
		client:       client,
		webdavServer: webdavServer,
		notifier:     tg,
		loginState:   make(map[string]*pendingLogin),
		authCfg:      authCfg,
		rateLimiter:  config.NewAuthRateLimiter(10, time.Minute),
		sessions:     make(map[string]*webSession),
	}

	if client != nil {
		client.OnSessionExpired = func(err error) {
			if s.notifier != nil && s.notifier.IsConfigured() {
				cfg := client.GetConfig()
				_ = s.notifier.SendSessionExpiredAlert(cfg.Provider, cfg.Phone, err.Error())
			}
		}
	}

	return s
}

func (s *Server) IsRunning() bool {
	if s.webdavServer != nil {
		return s.webdavServer.IsRunning()
	}
	return true
}

func (s *Server) SetRunning(r bool) {
	if s.webdavServer != nil {
		s.webdavServer.SetRunning(r)
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/version", s.handleVersion)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/verify_phone", s.handleVerifyPhone)
	mux.HandleFunc("/storage_info", s.handleStorageInfo)
	mux.HandleFunc("/start", s.handleStart)
	mux.HandleFunc("/stop", s.handleStop)
	mux.HandleFunc("/logs", s.handleLogs)
	mux.HandleFunc("/start_login", s.handleStartLogin)
	mux.HandleFunc("/complete_login", s.handleCompleteLogin)
	mux.HandleFunc("/unlink", s.handleUnlink)
	mux.HandleFunc("/test_telegram", s.handleTestTelegram)
	mux.HandleFunc("/save_telegram", s.handleSaveTelegram)
	mux.HandleFunc("/api/diagnostic/compare", s.handleDiagnosticCompare)
	mux.HandleFunc("/diagnostic/compare", s.handleDiagnosticCompare)

	return s.securityMiddleware(mux)
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", panelCSP)

		// /health siempre accesible sin autenticación para healthchecks de Docker
		if r.URL.Path == "/health" || r.URL.Path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}

		// Rutas públicas de autenticación web
		if r.URL.Path == "/login" || r.URL.Path == "/logout" {
			next.ServeHTTP(w, r)
			return
		}

		if !s.authCfg.IsEnabled() {
			next.ServeHTTP(w, r)
			return
		}

		if s.isAuthenticated(r) {
			next.ServeHTTP(w, r)
			return
		}

		// No autenticado
		clientIP := config.ExtractClientIP(r)

		// 1. Si el cliente envió cabecera de Basic Auth (e.g. curl o scripts automáticos con credenciales erróneas)
		rawUser, _, _, hasAuth := config.ParseBasicAuthHeader(r.Header.Get("Authorization"))
		if hasAuth {
			s.rateLimiter.RecordFailure(clientIP)
			logger.Log("[WebUI] 🔒 Intento de acceso Basic Auth fallido al panel desde %s (usuario: %s)", clientIP, logger.Mask(rawUser))
			w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// 2. Si es una petición de navegación web (GET a "/" o con Accept: text/html)
		accept := strings.ToLower(r.Header.Get("Accept"))
		if r.Method == http.MethodGet && (r.URL.Path == "/" || strings.Contains(accept, "text/html")) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// 3. Para peticiones AJAX / API sin cabecera de autenticación, devolver 401 Unauthorized sin WWW-Authenticate para evitar el popup del navegador
		jsonResponse(w, http.StatusUnauthorized, map[string]interface{}{
			"status":  "error",
			"message": "Autenticación requerida. Inicia sesión en /login",
		})
	})
}

func isLoopbackHost(h string) bool {
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func validateSameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	fwdHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))

	if origin != "" {
		if strings.EqualFold(origin, "null") {
			logger.Log("[WebUI] 🚫 CSRF bloqueado: Origin='null' (contexto aislado o no confiable)")
			return false
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			logger.Log("[WebUI] 🚫 CSRF bloqueado: Origin='%s' no válido", origin)
			return false
		}
		if equalHost(u.Host, r.Host) || (fwdHost != "" && equalHost(u.Host, fwdHost)) {
			return true
		}
		logger.Log("[WebUI] 🚫 CSRF bloqueado: Origin='%s' no coincide con Host='%s'", origin, r.Host)
		return false
	}

	referer := strings.TrimSpace(r.Header.Get("Referer"))
	if referer != "" {
		u, err := url.Parse(referer)
		if err == nil && u.Host != "" {
			if equalHost(u.Host, r.Host) || (fwdHost != "" && equalHost(u.Host, fwdHost)) {
				return true
			}
			logger.Log("[WebUI] 🚫 CSRF bloqueado: Referer='%s' no coincide con Host='%s'", referer, r.Host)
			return false
		}
	}

	return true
}

func equalHost(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	ha, pa, errA := net.SplitHostPort(a)
	if errA != nil {
		ha = a
		pa = ""
	}
	hb, pb, errB := net.SplitHostPort(b)
	if errB != nil {
		hb = b
		pb = ""
	}

	// Si ambos son loopback (localhost, 127.0.0.1, ::1, 0.0.0.0), se consideran equivalentes
	if isLoopbackHost(ha) && isLoopbackHost(hb) {
		if pa != "" && pb != "" {
			return pa == pb
		}
		return true
	}

	if pa != "" && pb != "" && pa != pb {
		return false
	}

	return strings.EqualFold(ha, hb)
}

func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, map[string]interface{}{"status": "error", "message": "Método no permitido"})
		return false
	}
	if !validateSameOrigin(r) {
		jsonResponse(w, http.StatusForbidden, map[string]interface{}{"status": "error", "message": "Origen no permitido (CSRF)"})
		return false
	}
	return true
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	if !requirePost(w, r) {
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if !strings.HasPrefix(ct, "application/json") {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Se requiere Content-Type: application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Cuerpo JSON inválido o demasiado grande"})
		return false
	}
	return true
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	rendered := bytes.ReplaceAll(indexHTML, []byte("{{.Version}}"), []byte(config.Version))
	_, _ = w.Write(rendered)
}

func generateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) createSession(username string, remember bool) (string, time.Duration, error) {
	token, err := generateSessionToken()
	if err != nil {
		return "", 0, err
	}
	ttl := defaultSessionTTL
	if remember {
		ttl = rememberSessionTTL
	}
	now := time.Now()
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.cleanupExpiredSessionsLocked(now)
	s.sessions[token] = &webSession{
		Username:  username,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	return token, ttl, nil
}

func (s *Server) cleanupExpiredSessionsLocked(now time.Time) {
	for token, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, token)
		}
	}
}

func (s *Server) validateSession(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	s.sessionMu.RLock()
	sess, ok := s.sessions[token]
	s.sessionMu.RUnlock()
	if !ok || sess == nil {
		return false
	}
	if now.After(sess.ExpiresAt) {
		s.sessionMu.Lock()
		delete(s.sessions, token)
		s.sessionMu.Unlock()
		return false
	}
	return true
}

func (s *Server) destroySession(token string) {
	if token == "" {
		return
	}
	s.sessionMu.Lock()
	delete(s.sessions, token)
	s.sessionMu.Unlock()
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie == nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
		Expires:  time.Now().Add(ttl),
		Secure:   isHTTPS(r),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		Secure:   isHTTPS(r),
	})
}

func (s *Server) isAuthenticated(r *http.Request) bool {
	if !s.authCfg.IsEnabled() {
		return true
	}
	token := s.getSessionToken(r)
	if token != "" && s.validateSession(token) {
		return true
	}
	_, cleanUser, pass, hasAuth := config.ParseBasicAuthHeader(r.Header.Get("Authorization"))
	if hasAuth && s.authCfg.Validate(cleanUser, pass) {
		return true
	}
	return false
}

func isJSONRequest(r *http.Request) bool {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "application/json") {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

func (s *Server) renderLoginPage(w http.ResponseWriter, r *http.Request, errMsg, username string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	data := loginPageData{
		Version:  config.Version,
		Error:    errMsg,
		Username: username,
	}
	if err := loginTmpl.Execute(w, data); err != nil {
		logger.Log("[WebUI] ❌ Error renderizando plantilla de login: %v", err)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authCfg.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if s.isAuthenticated(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		clientIP := config.ExtractClientIP(r)
		var errMsg string
		if s.rateLimiter.IsBlocked(clientIP) {
			w.WriteHeader(http.StatusTooManyRequests)
			errMsg = "Demasiados intentos fallidos. Por seguridad, tu IP ha sido bloqueada temporalmente. Espera unos minutos."
		}
		s.renderLoginPage(w, r, errMsg, "")

	case http.MethodPost:
		if !validateSameOrigin(r) {
			if isJSONRequest(r) {
				jsonResponse(w, http.StatusForbidden, map[string]interface{}{"status": "error", "message": "Origen no permitido (CSRF)"})
			} else {
				w.WriteHeader(http.StatusForbidden)
				s.renderLoginPage(w, r, "Origen de solicitud no permitido (CSRF)", "")
			}
			return
		}

		clientIP := config.ExtractClientIP(r)
		if s.rateLimiter.IsBlocked(clientIP) {
			logger.Log("[WebUI] 🚫 IP bloqueada temporalmente por fallos de autenticación: %s", clientIP)
			w.Header().Set("Retry-After", "60")
			if isJSONRequest(r) {
				jsonResponse(w, http.StatusTooManyRequests, map[string]interface{}{
					"status":  "error",
					"message": "Demasiados intentos de acceso fallidos. Por seguridad, tu IP ha sido bloqueada temporalmente.",
				})
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			s.renderLoginPage(w, r, "Demasiados intentos fallidos. Por seguridad, tu IP ha sido bloqueada temporalmente. Espera unos minutos.", "")
			return
		}

		var username, password string
		var remember bool

		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if strings.HasPrefix(ct, "application/json") {
			var body struct {
				Username string `json:"username"`
				Password string `json:"password"`
				Remember bool   `json:"remember"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil {
				jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
					"status":  "error",
					"message": "Datos de formulario inválidos",
				})
				return
			}
			username = body.Username
			password = body.Password
			remember = body.Remember
		} else {
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				s.renderLoginPage(w, r, "Error al procesar el formulario", "")
				return
			}
			username = r.FormValue("username")
			password = r.FormValue("password")
			rememberVal := strings.ToLower(strings.TrimSpace(r.FormValue("remember")))
			remember = rememberVal == "1" || rememberVal == "true" || rememberVal == "on" || rememberVal == "yes"
		}

		cleanUser := config.NormalizeUsername(username)
		if cleanUser == "" || password == "" || !s.authCfg.Validate(cleanUser, password) {
			s.rateLimiter.RecordFailure(clientIP)
			logger.Log("[WebUI] 🔒 Intento de login web fallido desde %s (usuario: %s)", clientIP, logger.Mask(username))
			if isJSONRequest(r) {
				jsonResponse(w, http.StatusUnauthorized, map[string]interface{}{
					"status":  "error",
					"message": "Usuario o contraseña incorrectos",
				})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			s.renderLoginPage(w, r, "Usuario o contraseña incorrectos", username)
			return
		}

		s.rateLimiter.Reset(clientIP)
		token, ttl, err := s.createSession(cleanUser, remember)
		if err != nil {
			logger.Log("[WebUI] ❌ Error generando token de sesión: %v", err)
			if isJSONRequest(r) {
				jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
					"status":  "error",
					"message": "Error interno al iniciar sesión",
				})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			s.renderLoginPage(w, r, "Error interno del servidor", username)
			return
		}

		s.setSessionCookie(w, r, token, ttl)
		logger.Log("[WebUI] 🔓 Sesión web iniciada para %s desde %s", logger.Mask(cleanUser), clientIP)

		if isJSONRequest(r) {
			jsonResponse(w, http.StatusOK, map[string]interface{}{
				"status":   "ok",
				"redirect": "/",
			})
			return
		}

		http.Redirect(w, r, "/", http.StatusSeeOther)

	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := s.getSessionToken(r)
	if token != "" {
		s.destroySession(token)
		s.clearSessionCookie(w, r)
		logger.Log("[WebUI] 🔒 Sesión cerrada correctamente")
	}
	if isJSONRequest(r) {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"status":   "ok",
			"redirect": "/login",
		})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok", "version": config.Version})
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><rect width="100" height="100" rx="20" fill="#2563eb"/><path d="M25 58h50a15 15 0 0 0 0-30 22 22 0 0 0-42-5 18 18 0 0 0-8 35z" fill="white"/></svg>`))
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{"version": config.Version})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.client.GetConfig()
	status := "stopped"
	if s.IsRunning() {
		status = "running"
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":           status,
		"phone":            logger.Mask(cfg.Phone),
		"auth_enabled":     s.authCfg.IsEnabled(),
		"webdav_host_port": webDAVHostPort(),
	})
}

func webDAVHostPort() string {
	if port := strings.TrimSpace(os.Getenv("WEBDAV_HOST_PORT")); port != "" {
		return port
	}
	if port := strings.TrimSpace(os.Getenv("WEBDAV_PORT")); port != "" {
		return port
	}
	return "5050"
}

func (s *Server) handleVerifyPhone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Teléfono inválido"})
		return
	}

	cfg := config.LoadAccountConfig(phone)
	hasCredentials := cfg.RefreshToken != "" || (cfg.JSessionID != "" && cfg.ValidationKey != "")
	hasDeviceToken := cfg.RefreshToken != ""

	if cfg.TelegramBotToken != "" && cfg.TelegramChatID != "" {
		s.notifier.UpdateCredentials(cfg.TelegramBotToken, cfg.TelegramChatID)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":                "ok",
		"phone":                 phone,
		"provider":              cfg.Provider,
		"has_credentials":       hasCredentials,
		"has_device_token":      hasDeviceToken,
		"device_id":             logger.Mask(cfg.DeviceID),
		"telegram_configured":   cfg.TelegramBotToken != "" && cfg.TelegramChatID != "",
		"telegram_token_masked": logger.Mask(cfg.TelegramBotToken),
		"telegram_chat_id":      cfg.TelegramChatID,
	})
}

func (s *Server) handleStorageInfo(w http.ResponseWriter, r *http.Request) {
	if !s.client.HasCredentials() {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"status":    "unconfigured",
			"message":   "Dispositivo no vinculado",
			"used_str":  "-",
			"total_str": "-",
		})
		return
	}
	if s.client.IsSessionExpired() {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
			"status":  "error",
			"message": "Sesión expirada (HTTP 401): revincula tu cuenta en el panel web",
		})
		return
	}

	var space *sapi.StorageSpace
	if !s.IsRunning() {
		if s.webdavServer != nil {
			space = s.webdavServer.GetCachedStorageSpace()
		}
		if space == nil || (space.TotalBytes == 0 && space.UsedBytes == 0 && space.Raw == nil) {
			jsonResponse(w, http.StatusOK, map[string]interface{}{
				"status":    "paused",
				"message":   "Proxy pausado por el usuario",
				"used_str":  "-",
				"total_str": "-",
			})
			return
		}
	} else {
		if s.webdavServer != nil {
			space = s.webdavServer.GetStorageSpace()
		}
		if space == nil || (space.TotalBytes == 0 && space.UsedBytes == 0 && space.Raw == nil) {
			var err error
			space, err = s.client.GetStorageSpace()
			if err != nil {
				logger.Log("[WebUI] ⚠️ Error consultando espacio en almacenamiento: %v", err)
				jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"status": "error", "message": "Error al consultar el espacio de almacenamiento en Movistar/O2 Cloud"})
				return
			}
		}
	}

	formatBytes := func(b int64) string {
		const unit = 1024
		if b < unit {
			return fmt.Sprintf("%d B", b)
		}
		div, exp := int64(unit), 0
		for n := b / unit; n >= unit; n /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
	}

	usedStr := formatBytes(space.UsedBytes)
	totalStr := "Unlimited"
	if !space.NoLimit && space.TotalBytes > 0 {
		totalStr = formatBytes(space.TotalBytes)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":             "ok",
		"used_bytes":         space.UsedBytes,
		"total_bytes":        space.TotalBytes,
		"used_str":           usedStr,
		"total_str":          totalStr,
		"nolimit":            space.NoLimit,
		"max_upload_size_mb": webdav.GetMaxUploadMB(),
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	s.SetRunning(true)
	logger.Log("[WebDAV] ▶ Proxy reanudado por el usuario.")
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	s.SetRunning(false)
	logger.Log("[WebDAV] ■ Proxy pausado por el usuario.")
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"logs":       logger.GetLogs(),
		"is_running": s.IsRunning(),
	})
}

func (s *Server) cleanupExpiredLoginsLocked(now time.Time) {
	for k, v := range s.loginState {
		if now.Sub(v.Time) > loginStateTTL {
			delete(s.loginState, k)
		}
	}
	if len(s.loginState) >= maxPendingLogin {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.loginState {
			if oldestKey == "" || v.Time.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.Time
			}
		}
		if oldestKey != "" {
			delete(s.loginState, oldestKey)
		}
	}
}

func (s *Server) handleStartLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		Provider string `json:"provider"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Introduce un teléfono válido"})
		return
	}

	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	if provider != "o2" {
		provider = "movistar"
	}

	verifier, state, authURL := auth.BuildOAuthLoginURL(provider)

	now := time.Now()
	s.loginMu.Lock()
	s.cleanupExpiredLoginsLocked(now)
	s.loginState[phone] = &pendingLogin{
		Provider: provider,
		Verifier: verifier,
		State:    state,
		Time:     now,
	}
	s.loginMu.Unlock()

	logger.Log("[OAuth] 🔑 URL de autorización generada para %s (Proveedor: %s)", logger.Mask(phone), provider)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"auth_url": authURL,
		"provider": provider,
	})
}

func (s *Server) handleCompleteLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone     string `json:"phone"`
		CodeOrURL string `json:"code_or_url"`
		Provider  string `json:"provider"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	phone := config.SanitizePhone(body.Phone)
	rawCode := body.CodeOrURL

	if phone == "" || rawCode == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Datos incompletos"})
		return
	}

	now := time.Now()
	s.loginMu.Lock()
	s.cleanupExpiredLoginsLocked(now)
	pending, ok := s.loginState[phone]
	s.loginMu.Unlock()

	if !ok || pending == nil || now.Sub(pending.Time) > loginStateTTL {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "La sesión de vinculación ha expirado o no se ha iniciado. Abre de nuevo el enlace de login."})
		return
	}

	// Si el usuario pegó la URL completa o incluye state=, validar state en tiempo constante
	if returnedState, hasStateInInput := auth.ExtractState(rawCode); hasStateInInput && returnedState != "" {
		if subtle.ConstantTimeCompare([]byte(returnedState), []byte(pending.State)) != 1 {
			jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "El parámetro state de seguridad no coincide. Inicia de nuevo la vinculación."})
			return
		}
	}

	verifier := pending.Verifier
	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	if provider == "" {
		provider = pending.Provider
	}
	if provider != "o2" {
		provider = "movistar"
	}

	code := auth.ExtractAuthCode(rawCode)
	if code == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "No se encontró un código de autorización válido"})
		return
	}

	callbackURL := strings.TrimSpace(rawCode)
	if !strings.HasPrefix(strings.ToLower(callbackURL), "http") {
		callback, parseErr := url.Parse(auth.GetOAuthRedirectURI(provider))
		if parseErr != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"status": "error", "message": "No se pudo preparar la URL de retorno OAuth"})
			return
		}
		query := callback.Query()
		query.Set("code", code)
		query.Set("state", pending.State)
		callback.RawQuery = query.Encode()
		callbackURL = callback.String()
	}

	_, primedCookies, sessionErr := auth.NewSAPILoginSessionAt(provider, callbackURL)
	if sessionErr != nil {
		logger.Log("[OAuth] ⚠️ No se pudo preparar la sesión web previa a SAPI: %v", sessionErr)
	} else {
		logger.Log("[OAuth] 🍪 Sesión web preparada para SAPI (cookies: %v).", primedCookies.Names)
	}

	logger.Log("[OAuth] 🔄 Canjeando código para %s en apiseg.telefonica.es (%s)...", logger.Mask(phone), provider)
	tokens, err := auth.ExchangeCodeForTokens(code, verifier, provider)
	if err != nil {
		logger.Log("[OAuth] ❌ Error en canje para %s: %v", logger.Mask(phone), err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "No se pudo canjear el código de autorización. Comprueba que no haya caducado."})
		return
	}

	existingCfg := config.LoadAccountConfig(phone)
	devID := existingCfg.DeviceID
	if devID == "" {
		devID = auth.GenerateDeviceID()
	}
	logger.Log("[OAuth] ✔ Tokens obtenidos. Usando autorización OAuth directa para el dispositivo %s (%s)...", logger.Mask(devID), provider)

	nowMS := time.Now().UnixMilli()

	cfg := config.AccountConfig{
		Provider:         provider,
		Phone:            phone,
		DeviceID:         devID,
		DeviceName:       "PC Windows",
		RefreshToken:     tokens.RefreshToken,
		AccessToken:      tokens.AccessToken,
		LastRefreshDate:  nowMS,
		ExpiresIn:        strconv.Itoa(tokens.ExpiresIn),
		TelegramBotToken: existingCfg.TelegramBotToken,
		TelegramChatID:   existingCfg.TelegramChatID,
	}

	if primedCookies.JSessionID != "" {
		cfg.JSessionID = primedCookies.JSessionID
	}
	if primedCookies.ValidationKey != "" {
		cfg.ValidationKey = primedCookies.ValidationKey
	}

	if err := config.SaveAccountConfig(cfg); err != nil {
		logger.Log("[OAuth] ⚠️ Error guardando credenciales: %v", err)
	}

	s.loginMu.Lock()
	delete(s.loginState, phone)
	s.loginMu.Unlock()

	s.client.SetConfig(cfg)
	if s.webdavServer != nil {
		s.webdavServer.InvalidateCache()
		dbPath := filepath.Join(config.TokensDir, fmt.Sprintf("metadata_%s.db", phone))
		newStore, err := cache.NewMetadataStore(dbPath)
		if err != nil {
			logger.Log("[Metadata] ⚠️ Error inicializando MetadataStore tras login: %v", err)
		} else {
			logger.Log("[Metadata] 🗄️ SQLite MetadataStore inicializado para %s en %s", logger.Mask(phone), maskDBPath(dbPath))
			if oldStore := s.webdavServer.MetadataStore(); oldStore != nil && oldStore != newStore {
				_ = oldStore.Close()
			}
			s.webdavServer.SetMetadataStore(newStore)
			go func() {
				logger.Log("[Metadata] 🔄 Sincronizando metadatos para nueva cuenta vinculada...")
				if _, syncErr := newStore.SyncChanges(s.client, true); syncErr != nil {
					logger.Log("[Metadata] ⚠️ Error en sincronización inicial: %v", syncErr)
				} else {
					logger.Log("[Metadata] ✔ Sincronización inicial completada para %s", logger.Mask(phone))
				}
			}()
		}
	}
	if cfg.TelegramBotToken != "" && cfg.TelegramChatID != "" {
		s.notifier.UpdateCredentials(cfg.TelegramBotToken, cfg.TelegramChatID)
	}
	s.SetRunning(true)

	logger.Log("[OAuth] ✔ ¡Dispositivo vinculado y guardado con éxito para %s (%s)! Proxy WebDAV activo.", logger.Mask(phone), provider)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok", "provider": provider})
}

func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}

	botToken := strings.TrimSpace(body.BotToken)
	chatID := strings.TrimSpace(body.ChatID)

	phone := config.SanitizePhone(body.Phone)
	cfg := config.LoadAccountConfig(phone)
	if botToken == "" || strings.Contains(botToken, "…") || strings.Contains(botToken, "****") {
		botToken = cfg.TelegramBotToken
	}
	if chatID == "" {
		chatID = cfg.TelegramChatID
	}

	if botToken == "" || chatID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Token de bot o Chat ID no configurados",
		})
		return
	}
	if !config.ValidateBotToken(botToken) || !config.ValidateChatID(chatID) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Formato de Token de bot o Chat ID inválido",
		})
		return
	}

	s.notifier.UpdateCredentials(botToken, chatID)
	if err := s.notifier.SendTestAlert(cfg.Provider); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleSaveTelegram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}

	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		if activeCfg := s.client.GetConfig(); activeCfg.Phone != "" {
			phone = activeCfg.Phone
		} else {
			loaded := config.LoadAccountConfig("")
			phone = loaded.Phone
		}
	}
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Introduce un teléfono válido o vincula una cuenta primero",
		})
		return
	}

	cfg := config.LoadAccountConfig(phone)
	newToken := strings.TrimSpace(body.BotToken)
	newChatID := strings.TrimSpace(body.ChatID)

	// Si el usuario deja el token vacío o envía el valor enmascarado mientras mantiene un chat_id, conservar el token actual
	if (newToken == "" || strings.Contains(newToken, "…") || strings.Contains(newToken, "****")) && newChatID != "" && cfg.TelegramBotToken != "" {
		newToken = cfg.TelegramBotToken
	}

	if !config.ValidateBotToken(newToken) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Formato de Token de Telegram inválido (esperado: 123456:ABC-DEF...)",
		})
		return
	}
	if !config.ValidateChatID(newChatID) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Formato de Chat ID inválido (esperado: ID numérico o @canal)",
		})
		return
	}

	cfg.TelegramBotToken = newToken
	cfg.TelegramChatID = newChatID

	if err := config.SaveAccountConfig(cfg); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Error guardando configuración de Telegram",
		})
		return
	}

	s.client.SetConfig(cfg)
	if s.webdavServer != nil {
		s.webdavServer.InvalidateCache()
	}
	s.notifier.UpdateCredentials(cfg.TelegramBotToken, cfg.TelegramChatID)

	logger.Log("[Telegram] ⚙️ Credenciales de Telegram actualizadas para %s.", logger.Mask(phone))
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	phone := config.SanitizePhone(body.Phone)
	activePhone := config.SanitizePhone(s.client.GetConfig().Phone)
	if phone == "" {
		phone = activePhone
	}

	isActive := (phone == "" || phone == activePhone)

	if isActive {
		if s.webdavServer != nil {
			s.webdavServer.InvalidateCache()
			if ms := s.webdavServer.MetadataStore(); ms != nil {
				dbPath := ms.Path()
				_ = ms.Close()
				s.webdavServer.SetMetadataStore(nil)
				if dbPath != "" {
					if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
						logger.Log("[WebUI] ⚠️ Error eliminando base de datos SQLite %s: %v", maskDBPath(dbPath), err)
					}
					_ = os.Remove(dbPath + "-wal")
					_ = os.Remove(dbPath + "-shm")
				}
			}
		}
		if phone != "" {
			dbPath := filepath.Join(config.TokensDir, fmt.Sprintf("metadata_%s.db", phone))
			if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
				logger.Log("[WebUI] ⚠️ Error eliminando base de datos SQLite %s: %v", maskDBPath(dbPath), err)
			}
			_ = os.Remove(dbPath + "-wal")
			_ = os.Remove(dbPath + "-shm")
		}

		_ = config.DeleteAccountConfig(phone)
		s.client.SetConfig(config.AccountConfig{})
		s.SetRunning(false)

		logger.Log("[OAuth] 🗑️ Dispositivo desvinculado y credenciales eliminadas por el usuario.")
	} else {
		// Secondary account unlink: only delete its config and its metadata db
		dbPath := filepath.Join(config.TokensDir, fmt.Sprintf("metadata_%s.db", phone))
		if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
			logger.Log("[WebUI] ⚠️ Error eliminando base de datos SQLite secundaria %s: %v", maskDBPath(dbPath), err)
		}
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
		_ = config.DeleteAccountConfig(phone)
		logger.Log("[OAuth] 🗑️ Cuenta secundaria %s desvinculada y credenciales eliminadas.", logger.Mask(phone))
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func maskDBPath(p string) string {
	base := filepath.Base(p)
	dir := filepath.Dir(p)
	if strings.HasPrefix(base, "metadata_") && strings.HasSuffix(base, ".db") {
		phone := strings.TrimSuffix(strings.TrimPrefix(base, "metadata_"), ".db")
		return filepath.Join(dir, fmt.Sprintf("metadata_%s.db", logger.Mask(phone)))
	}
	return p
}

func (s *Server) handleDiagnosticCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.webdavServer == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status":  "error",
			"message": "Servidor WebDAV no inicializado",
		})
		return
	}

	ms := s.webdavServer.MetadataStore()
	if ms == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status":  "error",
			"message": "MetadataStore SQLite no inicializado",
		})
		return
	}

	reqPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if reqPath == "" {
		reqPath = "/"
	}
	cleanPath := path.Clean("/" + reqPath)

	folderInfo, _, isFolder, err := s.webdavServer.ResolvePath(cleanPath)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Ruta no encontrada: %v", err),
			"path":    cleanPath,
		})
		return
	}
	if !isFolder || folderInfo == nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "La ruta indicada no es una carpeta",
			"path":    cleanPath,
		})
		return
	}

	folderID := folderInfo.ID

	// 1. SAPI calls direct
	sapiFolders, errF := s.client.ListAllSubfolders(folderID)
	if errF != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Error consultando subcarpetas en SAPI: %v", errF),
		})
		return
	}
	sapiMedia, errM := s.client.ListAllMediaInFolder(folderID)
	if errM != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Error consultando archivos en SAPI: %v", errM),
		})
		return
	}

	// 2. SQLite queries
	sqliteFolders, err := ms.ListSubfolders(folderID)
	if err != nil {
		sqliteFolders = nil
	}
	sqliteMedia, err := ms.ListMediaInFolder(folderID)
	if err != nil {
		sqliteMedia = nil
	}
	virtualFiles, err := ms.ListVirtualEmptyFilesInFolder(folderID)
	if err != nil {
		virtualFiles = nil
	}

	type sizeDiff struct {
		Name       string `json:"name"`
		SQLiteSize int64  `json:"sqlite_size"`
		SAPISize   int64  `json:"sapi_size"`
	}

	sapiFolderNameMap := make(map[string]sapi.FolderItem)
	sapiFolderDups := make([]string, 0)
	for _, f := range sapiFolders {
		k := strings.ToLower(f.Name)
		if _, exists := sapiFolderNameMap[k]; exists {
			sapiFolderDups = append(sapiFolderDups, f.Name)
		} else {
			sapiFolderNameMap[k] = f
		}
	}

	sapiMediaNameMap := make(map[string]sapi.MediaItem)
	sapiMediaDups := make([]string, 0)
	for _, m := range sapiMedia {
		k := strings.ToLower(m.Name)
		if _, exists := sapiMediaNameMap[k]; exists {
			sapiMediaDups = append(sapiMediaDups, m.Name)
		} else {
			sapiMediaNameMap[k] = m
		}
	}

	sqliteFolderNameMap := make(map[string]sapi.FolderItem)
	sqliteFolderDups := make([]string, 0)
	for _, f := range sqliteFolders {
		k := strings.ToLower(f.Name)
		if _, exists := sqliteFolderNameMap[k]; exists {
			sqliteFolderDups = append(sqliteFolderDups, f.Name)
		} else {
			sqliteFolderNameMap[k] = f
		}
	}

	sqliteMediaNameMap := make(map[string]sapi.MediaItem)
	sqliteMediaDups := make([]string, 0)
	for _, m := range sqliteMedia {
		k := strings.ToLower(m.Name)
		if _, exists := sqliteMediaNameMap[k]; exists {
			sqliteMediaDups = append(sqliteMediaDups, m.Name)
		} else {
			sqliteMediaNameMap[k] = m
		}
	}

	virtualNames := make(map[string]bool)
	virtualList := make([]string, 0, len(virtualFiles))
	for _, vf := range virtualFiles {
		virtualNames[strings.ToLower(vf.Name)] = true
		virtualList = append(virtualList, vf.Name)
	}

	// Missing in SQLite (present in SAPI but missing in SQLite)
	missingInSQLite := make([]string, 0)
	for k, sf := range sapiFolderNameMap {
		if _, ok := sqliteFolderNameMap[k]; !ok {
			missingInSQLite = append(missingInSQLite, "[Carpeta] "+sf.Name)
		}
	}
	for k, sm := range sapiMediaNameMap {
		if _, ok := sqliteMediaNameMap[k]; !ok {
			missingInSQLite = append(missingInSQLite, "[Archivo] "+sm.Name)
		}
	}

	// Extra in SQLite (present in SQLite but missing in SAPI, excluding virtual empty files)
	extraInSQLite := make([]string, 0)
	for k, qf := range sqliteFolderNameMap {
		if _, ok := sapiFolderNameMap[k]; !ok {
			extraInSQLite = append(extraInSQLite, "[Carpeta] "+qf.Name)
		}
	}
	for k, qm := range sqliteMediaNameMap {
		if _, ok := sapiMediaNameMap[k]; !ok && !virtualNames[k] {
			extraInSQLite = append(extraInSQLite, "[Archivo] "+qm.Name)
		}
	}

	// Size mismatches
	sizeMismatches := make([]sizeDiff, 0)
	for k, sm := range sapiMediaNameMap {
		if qm, ok := sqliteMediaNameMap[k]; ok {
			if qm.Size != sm.Size {
				sizeMismatches = append(sizeMismatches, sizeDiff{
					Name:       sm.Name,
					SQLiteSize: qm.Size,
					SAPISize:   sm.Size,
				})
			}
		}
	}

	allDupsSAPI := append(sapiFolderDups, sapiMediaDups...)
	allDupsSQLite := append(sqliteFolderDups, sqliteMediaDups...)

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":                  "ok",
		"path":                    cleanPath,
		"folder_id":               folderID,
		"missing_in_sqlite":       missingInSQLite,
		"extra_in_sqlite":         extraInSQLite,
		"duplicates_sapi":         allDupsSAPI,
		"duplicates_sqlite":       allDupsSQLite,
		"size_mismatches":         sizeMismatches,
		"virtual_empty_files":     virtualList,
		"sapi_subfolders_count":   len(sapiFolders),
		"sapi_media_count":        len(sapiMedia),
		"sqlite_subfolders_count": len(sqliteFolders),
		"sqlite_media_count":      len(sqliteMedia),
		"virtual_empty_count":     len(virtualFiles),
	})
}
