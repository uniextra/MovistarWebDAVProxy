package webui

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/notifier"
	"movistar-webdav-proxy/internal/sapi"
	"movistar-webdav-proxy/internal/webdav"
)

//go:embed templates/index.html
var indexHTML []byte

const (
	maxBodyBytes    = 1 << 20 // 1 MB
	loginStateTTL   = 10 * time.Minute
	maxPendingLogin = 50
	panelCSP        = "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'"
)

type Server struct {
	client       *sapi.Client
	webdavServer *webdav.Server
	notifier     *notifier.TelegramNotifier
	loginMu      sync.Mutex
	loginState   map[string]*pendingLogin
	authCfg      config.AuthConfig
	rateLimiter  *config.AuthRateLimiter
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
	mux.HandleFunc("/health", s.handleHealth)
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

	return s.securityMiddleware(mux)
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", panelCSP)

		// /health siempre accesible sin autenticación para healthchecks de Docker
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		if s.authCfg.IsEnabled() {
			clientIP := config.ExtractClientIP(r)
			if s.rateLimiter.IsBlocked(clientIP) {
				logger.Log("[WebUI] 🚫 IP bloqueada temporalmente por fallos de autenticación: %s", clientIP)
				http.Error(w, "Too Many Authentication Failures", http.StatusTooManyRequests)
				return
			}
			rawUser, cleanUser, pass, hasAuth := config.ParseBasicAuthHeader(r.Header.Get("Authorization"))
			if !hasAuth || !s.authCfg.Validate(cleanUser, pass) {
				if hasAuth && (rawUser != "" || pass != "") {
					s.rateLimiter.RecordFailure(clientIP)
					logger.Log("[WebUI] 🔒 Intento de acceso fallido al panel desde %s (usuario: %s)", clientIP, logger.Mask(rawUser))
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			s.rateLimiter.Reset(clientIP)
		}

		next.ServeHTTP(w, r)
	})
}

func validateSameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return equalHost(u.Host, r.Host)
}

func equalHost(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	ha, pa, errA := net.SplitHostPort(a)
	hb, pb, errB := net.SplitHostPort(b)
	if errA == nil && errB == nil {
		return strings.EqualFold(ha, hb) && pa == pb
	}
	return false
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
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok", "version": config.Version})
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
		"status": status,
		"phone":  logger.Mask(cfg.Phone),
	})
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

	space, err := s.client.GetStorageSpace()
	if err != nil {
		logger.Log("[WebUI] ⚠️ Error consultando espacio en almacenamiento: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"status": "error", "message": "Error al consultar el espacio de almacenamiento en Movistar/O2 Cloud"})
		return
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
	logger.Log("[OAuth] ✔ Tokens obtenidos. Inicializando sesión SAPI para dispositivo %s (%s)...", logger.Mask(devID), provider)

	sapiBase := auth.GetSAPIBaseURL(provider)
	nowMS := time.Now().UnixMilli()
	sapiRes := auth.SAPILoginOAuth(nil, sapiBase, tokens.AccessToken, tokens.RefreshToken, devID, "PC Windows", nowMS, strconv.Itoa(tokens.ExpiresIn))

	if sapiRes.StatusCode != http.StatusOK || (sapiRes.ValidationKey == "" && sapiRes.JSessionID == "") {
		logger.Log("[OAuth] ❌ Error inicializando sesión SAPI (HTTP %d): %v", sapiRes.StatusCode, sapiRes.Error)
		provLabel := "Movistar Cloud"
		if provider == "o2" {
			provLabel = "O2 Cloud"
		}
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Error iniciando sesión en %s (HTTP %d). Comprueba que la línea tenga el servicio activo.", provLabel, sapiRes.StatusCode),
		})
		return
	}

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

	if sapiRes.ValidationKey != "" {
		cfg.ValidationKey = sapiRes.ValidationKey
	}
	if sapiRes.JSessionID != "" {
		cfg.JSessionID = sapiRes.JSessionID
	}
	if sapiRes.LastRefreshDate > 0 {
		cfg.LastRefreshDate = sapiRes.LastRefreshDate
	}
	if sapiRes.RefreshedOAuth != nil {
		if sapiRes.RefreshedOAuth.AccessToken != "" {
			cfg.AccessToken = sapiRes.RefreshedOAuth.AccessToken
		}
		if sapiRes.RefreshedOAuth.RefreshToken != "" {
			cfg.RefreshToken = sapiRes.RefreshedOAuth.RefreshToken
		}
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
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Introduce un teléfono válido",
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
	if phone == "" {
		phone = s.client.GetConfig().Phone
	}

	_ = config.DeleteAccountConfig(phone)
	s.client.SetConfig(config.AccountConfig{})
	s.SetRunning(false)

	logger.Log("[OAuth] 🗑️ Dispositivo desvinculado y credenciales eliminadas por el usuario.")
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}
