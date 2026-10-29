package webui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
	"movistar-webdav-proxy/internal/webdav"
)

//go:embed templates/index.html
var indexHTML []byte

type Server struct {
	client       *sapi.Client
	webdavServer *webdav.Server
	loginMu      sync.Mutex
	loginState   map[string]*pendingLogin
}

type pendingLogin struct {
	Verifier string
	State    string
	Time     time.Time
}

func NewServer(client *sapi.Client, webdavServer *webdav.Server) *Server {
	return &Server{
		client:       client,
		webdavServer: webdavServer,
		loginState:   make(map[string]*pendingLogin),
	}
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

	return mux
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok", "version": "v2.0.2"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{"version": "v2.0.2"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.client.GetConfig()
	status := "stopped"
	if s.IsRunning() {
		status = "running"
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status": status,
		"phone":  cfg.Phone,
	})
}

func (s *Server) handleVerifyPhone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Teléfono inválido"})
		return
	}

	cfg := config.LoadAccountConfig(phone)
	hasCredentials := cfg.RefreshToken != "" || (cfg.JSessionID != "" && cfg.ValidationKey != "")
	hasDeviceToken := cfg.RefreshToken != ""

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":           "ok",
		"phone":            phone,
		"has_credentials":  hasCredentials,
		"has_device_token": hasDeviceToken,
		"device_id":        cfg.DeviceID,
	})
}

func (s *Server) handleStorageInfo(w http.ResponseWriter, r *http.Request) {
	space, err := s.client.GetStorageSpace()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"status": "error", "message": err.Error()})
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
		"max_upload_size_mb": 4096,
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	s.SetRunning(true)
	logger.Log("[WebDAV] ▶ Proxy reanudado por el usuario.")
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleStartLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Introduce un teléfono válido"})
		return
	}

	verifier, state, authURL := auth.BuildOAuthLoginURL()

	s.loginMu.Lock()
	s.loginState[phone] = &pendingLogin{
		Verifier: verifier,
		State:    state,
		Time:     time.Now(),
	}
	s.loginMu.Unlock()

	logger.Log("[OAuth] 🔑 URL de autorización generada para %s", phone)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"auth_url": authURL,
	})
}

func (s *Server) handleCompleteLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone     string `json:"phone"`
		CodeOrURL string `json:"code_or_url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	phone := config.SanitizePhone(body.Phone)
	rawCode := body.CodeOrURL

	if phone == "" || rawCode == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Datos incompletos"})
		return
	}

	s.loginMu.Lock()
	pending, ok := s.loginState[phone]
	s.loginMu.Unlock()

	var verifier string
	if ok && pending != nil {
		verifier = pending.Verifier
	}

	code := auth.ExtractAuthCode(rawCode)
	if code == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "No se encontró el código de autorización"})
		return
	}

	logger.Log("[OAuth] 🔄 Canjeando código para %s en apiseg.telefonica.es...", phone)
	tokens, err := auth.ExchangeCodeForTokens(code, verifier)
	if err != nil {
		logger.Log("[OAuth] ❌ Error en canje: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	existingCfg := config.LoadAccountConfig(phone)
	devID := existingCfg.DeviceID
	if devID == "" {
		devID = auth.GenerateDeviceID()
	}
	logger.Log("[OAuth] ✔ Tokens obtenidos. Inicializando sesión SAPI para dispositivo %s...", devID)

	nowMS := time.Now().UnixMilli()
	sapiRes := auth.SAPILoginOAuth(nil, tokens.AccessToken, tokens.RefreshToken, devID, "PC Windows", nowMS, strconv.Itoa(tokens.ExpiresIn))

	if sapiRes.StatusCode != http.StatusOK && (sapiRes.ValidationKey == "" && sapiRes.JSessionID == "") {
		logger.Log("[OAuth] ❌ Error inicializando sesión SAPI (HTTP %d): %v", sapiRes.StatusCode, sapiRes.Error)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Error iniciando sesión en Movistar Cloud (HTTP %d). Comprueba que la línea tenga el servicio activo.", sapiRes.StatusCode),
		})
		return
	}

	cfg := config.AccountConfig{
		Phone:           phone,
		DeviceID:        devID,
		DeviceName:      "PC Windows",
		RefreshToken:    tokens.RefreshToken,
		AccessToken:     tokens.AccessToken,
		LastRefreshDate: nowMS,
		ExpiresIn:       strconv.Itoa(tokens.ExpiresIn),
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
	s.SetRunning(true)

	logger.Log("[OAuth] ✔ ¡Dispositivo vinculado y guardado con éxito! Proxy WebDAV activo.")
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, map[string]interface{}{"status": "error", "message": "Método no permitido"})
		return
	}

	var body struct {
		Phone string `json:"phone"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
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
