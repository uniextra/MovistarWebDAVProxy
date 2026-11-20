package sapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
)

type StorageSpace struct {
	UsedBytes  int64                  `json:"used_bytes"`
	TotalBytes int64                  `json:"total_bytes"`
	FreeBytes  int64                  `json:"free_bytes"`
	NoLimit    bool                   `json:"nolimit"`
	Raw        map[string]interface{} `json:"raw"`
}

// FlexDate handles date fields in SAPI JSON that may be either string (RFC3339, YYYYMMDDTHHMMSSZ, etc.)
// or integer/float (Unix epoch timestamp in milliseconds or seconds).
type FlexDate string

func (fd *FlexDate) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*fd = ""
		return nil
	}

	// Quoted string
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err == nil {
			*fd = FlexDate(s)
			return nil
		}
		*fd = FlexDate(trimmed[1 : len(trimmed)-1])
		return nil
	}

	// Number (ms or seconds timestamp, e.g. 1793186708858)
	*fd = FlexDate(trimmed)
	return nil
}

func (fd FlexDate) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(fd))
}

func (fd FlexDate) String() string {
	return string(fd)
}

func (fd FlexDate) Time() time.Time {
	str := strings.TrimSpace(string(fd))
	if str == "" || str == "0" {
		return time.Now()
	}

	// 1. Check if numeric timestamp
	if num, err := strconv.ParseInt(str, 10, 64); err == nil && num > 0 {
		if num > 100000000000 { // Milliseconds (> ~year 1973 in ms)
			return time.UnixMilli(num)
		}
		return time.Unix(num, 0)
	}

	// 2. Float timestamp (e.g. 1793186708.858)
	if f, err := strconv.ParseFloat(str, 64); err == nil && f > 0 {
		if f > 100000000000 {
			return time.UnixMilli(int64(f))
		}
		sec := int64(f)
		nsec := int64((f - float64(sec)) * 1e9)
		return time.Unix(sec, nsec)
	}

	// 3. String date formats
	layouts := []string{
		"20060102T150405Z",
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		time.RFC1123,
		time.RFC1123Z,
		time.ANSIC,
		time.UnixDate,
		time.RubyDate,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, str); err == nil {
			return t
		}
	}

	return time.Now()
}

type FolderItem struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name"`
	ParentID         int64    `json:"parentid"`
	Magic            bool     `json:"magic"`
	Offline          bool     `json:"offline"`
	CreationDate     FlexDate `json:"creationdate"`
	ModificationDate FlexDate `json:"modificationdate"`
}

func (f *FolderItem) UnmarshalJSON(data []byte) error {
	type Alias FolderItem
	aux := struct {
		ID       interface{} `json:"id"`
		ParentID interface{} `json:"parentid"`
		Date     FlexDate    `json:"date"`
		*Alias
	}{
		Alias: (*Alias)(f),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.ID != nil {
		switch v := aux.ID.(type) {
		case float64:
			f.ID = int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				f.ID = n
			}
		case json.Number:
			if n, err := v.Int64(); err == nil {
				f.ID = n
			}
		}
	}
	if aux.ParentID != nil {
		switch v := aux.ParentID.(type) {
		case float64:
			f.ParentID = int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				f.ParentID = n
			}
		case json.Number:
			if n, err := v.Int64(); err == nil {
				f.ParentID = n
			}
		}
	}
	if f.CreationDate == "" && aux.Date != "" {
		f.CreationDate = aux.Date
	}
	if f.ModificationDate == "" && aux.Date != "" {
		f.ModificationDate = aux.Date
	}
	return nil
}

type MediaItem struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Size             int64    `json:"size"`
	URL              string   `json:"url"`
	ETag             string   `json:"etag"`
	FolderID         int64    `json:"folderid"`
	CreationDate     FlexDate `json:"creationdate"`
	ModificationDate FlexDate `json:"modificationdate"`
}

func (m *MediaItem) UnmarshalJSON(data []byte) error {
	type Alias MediaItem
	aux := struct {
		ID       interface{} `json:"id"`
		Size     interface{} `json:"size"`
		FolderID interface{} `json:"folderid"`
		Folder   interface{} `json:"folder"`
		Date     FlexDate    `json:"date"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.ID != nil {
		switch v := aux.ID.(type) {
		case string:
			m.ID = v
		case float64:
			m.ID = strconv.FormatInt(int64(v), 10)
		case json.Number:
			m.ID = v.String()
		}
	}
	if aux.Size != nil {
		switch v := aux.Size.(type) {
		case float64:
			m.Size = int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				m.Size = n
			}
		case json.Number:
			if n, err := v.Int64(); err == nil {
				m.Size = n
			}
		}
	}
	if aux.FolderID != nil {
		switch v := aux.FolderID.(type) {
		case float64:
			m.FolderID = int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				m.FolderID = n
			}
		case json.Number:
			if n, err := v.Int64(); err == nil {
				m.FolderID = n
			}
		}
	} else if aux.Folder != nil {
		switch v := aux.Folder.(type) {
		case float64:
			m.FolderID = int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				m.FolderID = n
			}
		case json.Number:
			if n, err := v.Int64(); err == nil {
				m.FolderID = n
			}
		}
	}
	if m.CreationDate == "" && aux.Date != "" {
		m.CreationDate = aux.Date
	}
	if m.ModificationDate == "" && aux.Date != "" {
		m.ModificationDate = aux.Date
	}
	m.Name = fromCloudName(m.Name)
	return nil
}

type Client struct {
	mu               sync.RWMutex
	refreshMu        sync.Mutex
	cfg              config.AccountConfig
	httpClient       *http.Client
	baseURL          string
	uploadURL        string
	maxUploadMB      int
	rootFolderID     int64
	sessionExpired   bool
	refreshTransient bool
	// Fallos de login/oauth en el perfil desktop (ver desktopLoginFailed).
	loginFailures     int
	loginFailingSince time.Time
	loginRetryAt      time.Time
	OnSessionExpired  func(err error)

	// web, si no es nil, sustituye la autenticación del cliente de escritorio por
	// la sesión de la web (ver EnableWebSession).
	web WebSession
	// freshURLs guarda, por ID, la última URL de descarga renovada en modo web.
	freshURLs sync.Map

	sapiSem        chan struct{}
	pacingMu       sync.Mutex
	lastReqTime    time.Time
	pacingInterval time.Duration

	refreshFn func() bool // solo para pruebas: sustituye a RefreshSession (que llama a O2 de verdad)

	waf       wafBreaker // pausa las llamadas a la API de O2 tras un 403 del cortafuegos (waf_breaker.go)
	wafUpload wafBreaker // pausa las subidas (servidor de subidas) tras un 403 del cortafuegos en ese servidor

	hookMu           sync.Mutex
	onMediaRefreshed func(MediaRefresh)       // relectura tras subir (upload_oficial.go)
	settling         map[string]chan struct{} // subidas aún en validación o sin releer (ID → se cierra al terminar)

	validationFirst time.Duration // primera consulta de validación tras subir (WithValidationPolling)
	validationMax   time.Duration // espera máxima entre consultas de validación
}

// ErrFileTooLarge se devuelve cuando SAPI rechaza un archivo por superar el tamaño máximo permitido (MED-1020).
var ErrFileTooLarge = errors.New("SAPI MED-1020: file size exceeds configured max file size limit")

// ErrMediaUnsupported (MED-1050): O2 no admite el archivo con el tipo que indica su
// extensión (p. ej. un .jpg cuyo contenido no es una imagen). Es definitivo: la app
// oficial lo deja sin subir y no lo reintenta.
var ErrMediaUnsupported = errors.New("SAPI MED-1050: the file extension or the format is not supported")

// ErrSessionRefreshTransient indica que la renovación de sesión falló por un
// problema transitorio (red/5xx), no porque el refresh_token esté revocado.
var ErrSessionRefreshTransient = errors.New("SAPI session refresh temporarily failed")

// Option ajusta un Client al crearlo (NewClient).
type Option func(*Client)

// WithValidationPolling cambia, para este cliente, cuándo se consulta la validación
// de un archivo recién subido: la primera consulta a los first y, después, cada vez
// más espaciadas hasta un máximo de max entre consultas. Por defecto,
// SAPI_VALIDATION_FIRST_SEC y SAPI_VALIDATION_MAX_SEC (10 s y 1 min, como la app).
func WithValidationPolling(first, max time.Duration) Option {
	return func(c *Client) {
		if first > 0 {
			c.validationFirst = first
		}
		if max > 0 {
			c.validationMax = max
		}
	}
}

func NewClient(cfg config.AccountConfig, opts ...Option) (*Client, error) {
	// Transporte HTTP/2 con ClientHello emitido por uTLS, imitando un navegador
	// convencional para no disparar el AWS CloudFront WAF Bot Control (403).
	transport := auth.NewFingerprintedTransport()
	logger.Log("[SAPI] 🔐 Huella TLS (uTLS): %s", auth.FingerprintName())

	// No usamos CookieJar automático en httpClient para evitar que respuestas 401/403
	// HTML de SAPI inyecten cookies anónimas o duplicadas: las cabeceras Cookie de
	// sesión (JSESSIONID / validationkey) se gestionan explícitamente desde c.cfg.
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   0,
	}

	baseURL := auth.GetSAPIBaseURL(cfg.Provider)
	uploadURL := auth.GetUploadBaseURL(cfg.Provider)

	maxConcurrent := 1
	if envMax := strings.TrimSpace(os.Getenv("SAPI_MAX_CONCURRENT")); envMax != "" {
		if n, err := strconv.Atoi(envMax); err == nil && n > 0 {
			maxConcurrent = n
		}
	}

	pacing := 100 * time.Millisecond
	if envPacing := strings.TrimSpace(os.Getenv("SAPI_PACING_MS")); envPacing != "" {
		if ms, err := strconv.Atoi(envPacing); err == nil && ms >= 0 {
			pacing = time.Duration(ms) * time.Millisecond
		}
	}

	c := &Client{
		cfg:            cfg,
		httpClient:     httpClient,
		baseURL:        baseURL,
		uploadURL:      uploadURL,
		maxUploadMB:    4096,
		sapiSem:        make(chan struct{}, maxConcurrent),
		pacingInterval: pacing,

		validationFirst: config.ValidationFirstWait(),
		validationMax:   config.ValidationMaxWait(),
	}
	for _, opt := range opts {
		opt(c)
	}

	c.syncCookiesLocked()

	return c, nil
}

// WebSession es la sesión de la web oficial que usa el modo web (webclient.Session).
type WebSession interface {
	CheckLogin(ctx context.Context) bool
	LoggedIn() bool
}

// EnableWebSession hace que el cliente hable con Movistar Cloud como la web:
// todas las peticiones pasan por rt (webclient.Transport) y la validez de la
// sesión la decide ws en lugar de los tokens OAuth del cliente de escritorio.
func (c *Client) EnableWebSession(ws WebSession, rt http.RoundTripper) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.web = ws
	c.httpClient.Transport = rt
	c.httpClient.Jar = nil
	c.sessionExpired = false
}

// SetMaxConcurrentRequests fija cuántas peticiones a Movistar pueden estar en curso
// a la vez. Una subida ocupa su hueco mientras envía el cuerpo (decenas de segundos
// por GB), así que con un solo hueco bloquearía todas las lecturas. Debe llamarse
// al arrancar, antes de servir peticiones.
func (c *Client) SetMaxConcurrentRequests(n int) {
	if n < 1 {
		n = 1
	}
	c.sapiSem = make(chan struct{}, n)
}

// WebMode indica si el cliente usa la sesión de la web.
func (c *Client) WebMode() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.web != nil
}

func (c *Client) syncCookiesLocked() {
	if c.httpClient == nil || c.httpClient.Jar == nil {
		return
	}
	if c.cfg.ValidationKey == "" && c.cfg.JSessionID == "" {
		return
	}
	cookieDomain := "micloud.movistar.es"
	if strings.EqualFold(c.cfg.Provider, "o2") {
		cookieDomain = "cloud.o2online.es"
	}
	sapiURL, err := url.Parse("https://" + cookieDomain)
	if err != nil {
		return
	}
	var cookies []*http.Cookie
	if c.cfg.ValidationKey != "" {
		cookies = append(cookies, &http.Cookie{
			Name:   "validationkey",
			Value:  c.cfg.ValidationKey,
			Path:   "/",
			Domain: cookieDomain,
		}, &http.Cookie{
			Name:   "validationKey",
			Value:  c.cfg.ValidationKey,
			Path:   "/",
			Domain: cookieDomain,
		})
	}
	if c.cfg.JSessionID != "" {
		cookies = append(cookies, &http.Cookie{
			Name:   "JSESSIONID",
			Value:  c.cfg.JSessionID,
			Path:   "/",
			Domain: cookieDomain,
		})
	}
	c.httpClient.Jar.SetCookies(sapiURL, cookies)
}

func (c *Client) GetConfig() config.AccountConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

func (c *Client) SetConfig(cfg config.AccountConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	c.baseURL = auth.GetSAPIBaseURL(cfg.Provider)
	c.uploadURL = auth.GetUploadBaseURL(cfg.Provider)
	c.rootFolderID = 0
	c.sessionExpired = false
	c.syncCookiesLocked()
}

func (c *Client) IsSessionExpired() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.web != nil {
		return !c.web.LoggedIn()
	}
	return c.sessionExpired
}

func (c *Client) RefreshTransient() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.refreshTransient
}

func (c *Client) SetBaseURLs(baseURL, uploadURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if baseURL != "" {
		c.baseURL = strings.TrimSuffix(baseURL, "/")
	}
	if uploadURL != "" {
		c.uploadURL = strings.TrimSuffix(uploadURL, "/")
	}
}

func (c *Client) HasCredentials() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.web != nil {
		return c.web.LoggedIn()
	}
	return c.cfg.RefreshToken != "" || c.cfg.AccessToken != "" || (c.cfg.JSessionID != "" && c.cfg.ValidationKey != "")
}

func (c *Client) appendValidationKey(rawURL string) string {
	c.mu.RLock()
	vk := c.cfg.ValidationKey
	c.mu.RUnlock()

	if vk == "" {
		return rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	// La app oficial pide system/information sin validationkey.
	if strings.HasSuffix(parsed.Path, "/system/information") && auth.IsDesktopProfile() {
		return rawURL
	}

	q := parsed.Query()
	q.Set("validationkey", vk)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func (c *Client) processSAPIResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if c.WebMode() {
		// En modo web las credenciales viven en el navegador; no hay tokens que rotar.
		return false
	}

	isUploadHost := resp.Request != nil && resp.Request.URL != nil &&
		strings.HasPrefix(strings.ToLower(resp.Request.URL.Hostname()), "upload.")

	var changed bool
	var tokensRotated bool
	c.mu.Lock()

	// 1. Cabecera Authorization devuelta con tokens rotados
	if authResp := resp.Header.Get("Authorization"); authResp != "" {
		if rotated := auth.ParseOAuthResponseHeader(authResp); rotated != nil {
			if rotated.AccessToken != "" && rotated.AccessToken != c.cfg.AccessToken {
				c.cfg.AccessToken = rotated.AccessToken
				changed = true
				tokensRotated = true
			}
			if rotated.RefreshToken != "" && rotated.RefreshToken != c.cfg.RefreshToken {
				c.cfg.RefreshToken = rotated.RefreshToken
				changed = true
				tokensRotated = true
			}
			if rotated.LastRefreshDate > 0 && rotated.LastRefreshDate != c.cfg.LastRefreshDate {
				c.cfg.LastRefreshDate = rotated.LastRefreshDate
				changed = true
			} else if rotated.LastRefreshDate <= 0 && rotated.AccessToken != "" && resp.StatusCode < 400 && auth.IsDesktopProfile() {
				// Así renueva el cliente oficial de escritorio: cuando
				// lastrefreshdate + expiresin ya ha pasado, envía la petición igual y
				// SAPI responde 200 con una cabecera Authorization (en la captura,
				// con los mismos tokens y sin lastrefreshdate). El cliente guarda lo
				// recibido y pone lastrefreshdate a la hora de esa respuesta. Nunca
				// llama a apiseg.telefonica.es para renovar.
				c.cfg.LastRefreshDate = time.Now().UnixMilli()
				changed = true
				tokensRotated = true
			}
			if rotated.ExpiresIn != "" && rotated.ExpiresIn != c.cfg.ExpiresIn {
				c.cfg.ExpiresIn = rotated.ExpiresIn
				changed = true
			}
		}
	}

	// Si la sesión fue marcada como expirada/revocada y esta respuesta no trae nuevos tokens OAuth,
	// no capturar cookies ni avanzar LastRefreshDate desde descargas CDN o respuestas cacheadas.
	if !c.sessionExpired || tokensRotated {
		// 2. Cookies de sesión (solo en respuestas < 400 para no capturar cookies
		// anónimas de páginas HTML de error 401/403 como si fueran una sesión autenticada;
		// además, no sobrescribir una JSESSIONID activa de SAPI desde el host de subidas upload.*)
		if resp.StatusCode < 400 {
			for _, cookie := range resp.Cookies() {
				if strings.EqualFold(cookie.Name, "validationkey") && cookie.Value != "" && cookie.Value != c.cfg.ValidationKey {
					c.cfg.ValidationKey = cookie.Value
					changed = true
				} else if strings.EqualFold(cookie.Name, "JSESSIONID") && cookie.Value != "" && cookie.Value != c.cfg.JSessionID && !(isUploadHost && c.cfg.JSessionID != "") {
					c.cfg.JSessionID = cookie.Value
					changed = true
				}
			}
		}

		// 3. Ventana deslizante de LastRefreshDate: SAPI rechaza cabeceras oauth cuando
		// (now - lastrefreshdate) supera expiresin (3600s). Durante copias largas (>= 1h),
		// si las peticiones 2xx siguen siendo válidas y han pasado > 25 min, refrescamos el timestamp.
		// En el perfil "desktop" no se usa: el cliente oficial mantiene la fecha real
		// y SAPI la renueva con la cabecera Authorization de la respuesta.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && c.cfg.AccessToken != "" && !auth.IsDesktopProfile() {
			if c.cfg.LastRefreshDate > 1750000000000 && time.Since(time.UnixMilli(c.cfg.LastRefreshDate)) > 25*time.Minute {
				c.cfg.LastRefreshDate = time.Now().UnixMilli()
				changed = true
			}
		}
	}

	if changed {
		c.syncCookiesLocked()
		if tokensRotated {
			c.sessionExpired = false
		}
	}
	cfgCopy := c.cfg
	c.mu.Unlock()

	if changed {
		_ = config.SaveAccountConfig(cfgCopy)
		logger.Log("[OAuth] ✔ Nuevas credenciales/cookies capturadas y guardadas en disco.")
	}
	return changed
}

func summarizeHTTPErrorBody(b []byte) string {
	s := strings.TrimSpace(strings.ToValidUTF8(string(b), "?"))
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "<!doctype html") || strings.Contains(lower, "<html") || strings.HasPrefix(lower, "<!--?xml") {
		title := ""
		if start := strings.Index(lower, "<title>"); start != -1 {
			rest := s[start+len("<title>"):]
			restLower := lower[start+len("<title>"):]
			if end := strings.Index(restLower, "</title>"); end != -1 {
				title = strings.Join(strings.Fields(rest[:end]), " ")
			}
		}
		if title != "" {
			return fmt.Sprintf("HTML error page (title=%q, %d bytes)", title, len(b))
		}
		return fmt.Sprintf("HTML error page (%d bytes)", len(b))
	}
	collapsed := strings.Join(strings.Fields(s), " ")
	const maxRunes = 240
	runes := []rune(collapsed)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return collapsed
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func (c *Client) newSAPILoginClient(sapiBase, provider string) (*http.Client, string) {
	if !isLoopbackURL(sapiBase) {
		if loginClient, primedJSess, err := auth.NewSAPILoginSession(provider); err == nil && loginClient != nil {
			return loginClient, primedJSess
		}
	}
	jar, _ := cookiejar.New(nil)
	var transport http.RoundTripper
	var timeout time.Duration
	if c.httpClient != nil {
		transport = c.httpClient.Transport
		timeout = c.httpClient.Timeout
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		Jar:       jar,
	}, ""
}

func (c *Client) RefreshSession() bool {
	ok, _ := c.refreshSessionInternal(false)
	return ok
}

func (c *Client) refreshSessionInternal(forceOAuth bool) (bool, bool) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	ws := c.web
	c.mu.RUnlock()
	if ws != nil {
		// No hay OAuth que renovar: solo se comprueba si la sesión de la web sigue viva.
		// Si caducó, hay que volver a iniciarla desde el panel.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return ws.CheckLogin(ctx), false
	}

	c.mu.RLock()
	cfg := c.cfg
	alreadyExpired := c.sessionExpired
	lastRefresh := c.cfg.LastRefreshDate
	c.mu.RUnlock()

	if alreadyExpired {
		return false, false
	}

	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		return false, false
	}

	// Tras un login fallido no se vuelve a intentar hasta que pasa la espera: cada
	// 401 de las peticiones en curso pediría otro login y O2 (CloudFront/WAF)
	// bloquea las ráfagas de logins.
	if !forceOAuth && auth.IsDesktopProfile() {
		c.mu.RLock()
		retryAt := c.loginRetryAt
		c.mu.RUnlock()
		if time.Now().Before(retryAt) {
			c.mu.Lock()
			c.refreshTransient = true
			c.mu.Unlock()
			return false, true
		}
	}

	logger.Log("[OAuth] 🔄 Renovando sesión SAPI...")
	c.mu.Lock()
	c.refreshTransient = false
	c.mu.Unlock()

	// 1. Intento primario: SAPILoginOAuth con access_token actual
	c.mu.RLock()
	sapiBase := c.baseURL
	c.mu.RUnlock()
	if sapiBase == "" {
		sapiBase = auth.GetSAPIBaseURL(cfg.Provider)
	}

	var step1 *auth.SAPILoginResult
	if !forceOAuth && cfg.AccessToken != "" {
		wasStaleLRD := cfg.LastRefreshDate > 1750000000000 && time.Since(time.UnixMilli(cfg.LastRefreshDate)) > 45*time.Minute
		loginLRD := cfg.LastRefreshDate
		if loginLRD <= 0 || wasStaleLRD {
			loginLRD = time.Now().UnixMilli()
		}
		loginClient, primedJSess := c.newSAPILoginClient(sapiBase, cfg.Provider)
		res := auth.SAPILoginOAuth(loginClient, sapiBase, cfg.AccessToken, cfg.RefreshToken, cfg.DeviceID, cfg.DeviceName, loginLRD, cfg.ExpiresIn)
		step1 = &res
		// Un 200 con cookies NO garantiza que el access_token siga siendo válido:
		// SAPI puede devolver cookies de sesión y un cuerpo de error. Exigimos además
		// evidencia de una sesión OAuth real (credenciales rotadas o validationkey sin error),
		// haber obtenido por primera vez la JSESSIONID en arranque en frío, o estar renovando
		// una sesión cuya marca LastRefreshDate superó los 45 minutos.
		hasSession := res.ValidationKey != "" || res.RefreshedOAuth != nil || (cfg.JSessionID == "" && primedJSess != "") || (wasStaleLRD && (res.JSessionID != "" || primedJSess != ""))
		if res.StatusCode == http.StatusOK && res.Error == nil && hasSession {
			c.mu.Lock()
			c.sessionExpired = false
			c.loginFailures, c.loginFailingSince, c.loginRetryAt = 0, time.Time{}, time.Time{}
			if res.ValidationKey != "" {
				c.cfg.ValidationKey = res.ValidationKey
			}
			if res.JSessionID != "" {
				c.cfg.JSessionID = res.JSessionID
			} else if primedJSess != "" && (c.cfg.JSessionID == "" || wasStaleLRD) {
				c.cfg.JSessionID = primedJSess
			}
			if res.LastRefreshDate > 0 {
				c.cfg.LastRefreshDate = res.LastRefreshDate
			} else if res.ResponseTime > 0 {
				c.cfg.LastRefreshDate = res.ResponseTime
			} else if wasStaleLRD || c.cfg.LastRefreshDate <= 0 {
				c.cfg.LastRefreshDate = loginLRD
			}
			if res.RefreshedOAuth != nil {
				if res.RefreshedOAuth.AccessToken != "" {
					c.cfg.AccessToken = res.RefreshedOAuth.AccessToken
				}
				if res.RefreshedOAuth.RefreshToken != "" {
					c.cfg.RefreshToken = res.RefreshedOAuth.RefreshToken
				}
			}
			c.syncCookiesLocked()
			cfgCopy := c.cfg
			c.mu.Unlock()

			_ = config.SaveAccountConfig(cfgCopy)
			logger.Log("[OAuth] ✔ Sesión SAPI refrescada con éxito (%s).", cfgCopy.Provider)
			return true, false
		}
		logger.Log("[OAuth] ℹ️ Paso 1 (SAPILoginOAuth) no renovó sesión para %s (HTTP %d, vk=%v, js=%v, err=%v); escalando a Paso 2...", cfg.Provider, res.StatusCode, res.ValidationKey != "", res.JSessionID != "" || primedJSess != "", res.Error)
	}

	// Evitar rotar el refresh_token repetidamente si ya se renovó recientemente (<30s)
	// y ya existe una sesión (JSESSIONID/validationkey) activa. Devolvemos usedStep2=true
	// para impedir que doRequestLocked escale a forceOAuth=true saltándose el cooldown.
	if !forceOAuth && lastRefresh > 0 && time.Since(time.UnixMilli(lastRefresh)) < 30*time.Second && (cfg.ValidationKey != "" || cfg.JSessionID != "") {
		logger.Log("[OAuth] ℹ️ Tokens renovados recientemente (<30s). Omitiendo rotación redundante de refresh_token.")
		return true, true
	}

	// El cliente oficial de escritorio nunca renueva en apiseg.telefonica.es: quien
	// rota los tokens es SAPI (cabecera Authorization de sus respuestas). Canjear el
	// refresh_token por nuestra cuenta compite con esa rotación y acaba en
	// "invalid_request / Invalid Token". En el perfil "desktop" solo se hace si se
	// pide expresamente con OAUTH_REFRESH_FALLBACK=true.
	if !apisegRefreshAllowed() {
		if c.desktopLoginFailed(step1) {
			return false, true
		}
		c.mu.Lock()
		wasExpired := c.sessionExpired
		c.sessionExpired = true
		c.refreshTransient = false
		c.mu.Unlock()
		logger.Log("[!] ⚠️ SAPI rechaza la sesión y el perfil de escritorio no renueva en apiseg (OAUTH_REFRESH_FALLBACK desactivado). Revincula la cuenta desde el panel web.")
		if !wasExpired && c.OnSessionExpired != nil {
			go c.OnSessionExpired(errors.New("sesión expirada (HTTP 401)"))
		}
		return false, true
	}

	// 2. Si falla o expiró, canjear refresh_token en apiseg.telefonica.es.
	// Reintentamos errores transitorios: un fallo puntual no debe convertir un
	// DELETE de HyperBackup en un 405 y dejar el repositorio en state=broken.
	var refreshErr error
	var tokens *auth.OAuthTokens
	if cfg.RefreshToken != "" {
		const refreshAttempts = 3
		for attempt := 0; attempt < refreshAttempts; attempt++ {
			logger.Log("[OAuth] 🔄 Canjeando refresh_token en apiseg.telefonica.es (%s) (%d/%d)...", cfg.Provider, attempt+1, refreshAttempts)
			c.httpClient.CloseIdleConnections()
			tokens, refreshErr = auth.RefreshOAuthToken(cfg.RefreshToken, cfg.Provider)
			if refreshErr == nil {
				break
			}
			if isTerminalRefreshError(refreshErr) {
				break
			}
			if attempt < refreshAttempts-1 {
				backoff := time.Duration(1<<attempt) * time.Second
				logger.Log("[OAuth] ⏳ Error transitorio al renovar sesión: %v. Reintentando en %v...", refreshErr, backoff)
				time.Sleep(backoff)
			}
		}
		if refreshErr == nil && tokens != nil && tokens.AccessToken != "" {
			c.mu.Lock()
			c.sessionExpired = false
			c.cfg.AccessToken = tokens.AccessToken
			if tokens.RefreshToken != "" {
				c.cfg.RefreshToken = tokens.RefreshToken
			}
			c.cfg.LastRefreshDate = time.Now().UnixMilli()
			cfgCopy := c.cfg
			c.mu.Unlock()

			// Persistir inmediatamente los nuevos tokens para proteger contra RTR (Refresh Token Rotation)
			_ = config.SaveAccountConfig(cfgCopy)

			// Con nuevo access_token, inicializar SAPI tanto para Movistar como para O2
			// para autenticar la cookie JSESSIONID antes de las llamadas SAPI.
			loginClient, primedJSess := c.newSAPILoginClient(sapiBase, cfgCopy.Provider)

			var res auth.SAPILoginResult
			for attempt := 0; attempt < 3; attempt++ {
				res = auth.SAPILoginOAuth(loginClient, sapiBase, tokens.AccessToken, cfgCopy.RefreshToken, cfgCopy.DeviceID, cfgCopy.DeviceName, cfgCopy.LastRefreshDate, cfgCopy.ExpiresIn)
				if res.StatusCode == http.StatusOK && res.Error == nil {
					break
				}
				if attempt < 2 {
					logger.Log("[OAuth] ⏳ SAPI devolvió %d tras refresco; reintentando login en %v...", res.StatusCode, time.Duration(attempt+1)*time.Second)
					time.Sleep(time.Duration(attempt+1) * time.Second)
				}
			}
			if res.StatusCode == http.StatusOK && res.Error == nil {
				c.mu.Lock()
				c.sessionExpired = false
				if res.ValidationKey != "" {
					c.cfg.ValidationKey = res.ValidationKey
				}
				if res.JSessionID != "" {
					c.cfg.JSessionID = res.JSessionID
				} else if primedJSess != "" {
					c.cfg.JSessionID = primedJSess
				}
				if res.LastRefreshDate > 0 {
					c.cfg.LastRefreshDate = res.LastRefreshDate
				}
				if res.RefreshedOAuth != nil {
					if res.RefreshedOAuth.AccessToken != "" {
						c.cfg.AccessToken = res.RefreshedOAuth.AccessToken
					}
					if res.RefreshedOAuth.RefreshToken != "" {
						c.cfg.RefreshToken = res.RefreshedOAuth.RefreshToken
					}
				}
				c.syncCookiesLocked()
				cfgCopy2 := c.cfg
				c.mu.Unlock()

				_ = config.SaveAccountConfig(cfgCopy2)
				logger.Log("[OAuth] ✔ Tokens renovados y sesión SAPI inicializada con éxito (%s).", cfgCopy.Provider)
				return true, true
			}
			// Si SAPI devolvió error tras renovar tokens en apiseg, no marcamos la sesión
			// como expirada permanentemente: el access_token recién emitido es válido y se
			// permite que las peticiones continúen con él (conservando la JSESSIONID preparada).
			logger.Log("[OAuth] ⚠️ Inicialización SAPI devolvió HTTP %d tras refresco OAuth (%v) para %s. Continuando con nuevo access_token...", res.StatusCode, res.Error, cfgCopy.Provider)
			c.mu.Lock()
			c.sessionExpired = false
			if primedJSess != "" && c.cfg.JSessionID == "" {
				c.cfg.JSessionID = primedJSess
			}
			c.syncCookiesLocked()
			cfgCopy2 := c.cfg
			c.mu.Unlock()
			_ = config.SaveAccountConfig(cfgCopy2)
			return true, true
		} else if refreshErr != nil {
			logger.Log("[OAuth] ❌ Error canjeando refresh_token: %v", refreshErr)
		}
	}

	if refreshErr != nil && isTerminalRefreshError(refreshErr) {
		c.mu.Lock()
		wasExpired := c.sessionExpired
		c.sessionExpired = true
		c.refreshTransient = false
		c.mu.Unlock()

		logger.Log("[!] ⚠️ ERROR 401: El refresh_token ha expirado o ha sido revocado. Pausando reintentos automáticos hasta revinculación en el panel web.")
		if !wasExpired && c.OnSessionExpired != nil {
			go c.OnSessionExpired(errors.New("sesión expirada (HTTP 401)"))
		}
	} else {
		if refreshErr != nil {
			c.mu.Lock()
			c.refreshTransient = true
			c.mu.Unlock()
		}
		logger.Log("[!] ⚠️ Error transitorio durante renovación de sesión. Conservando credenciales para futuros reintentos.")
	}
	return false, true
}

// apisegRefreshAllowed indica si se puede canjear el refresh_token en apiseg:
// siempre en el perfil "browser"; en "desktop", solo con OAUTH_REFRESH_FALLBACK=true.
func apisegRefreshAllowed() bool {
	if !auth.IsDesktopProfile() {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OAUTH_REFRESH_FALLBACK")), "true")
}

func isTerminalRefreshError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "401") || strings.Contains(msg, "invalid_grant")
}

func (c *Client) SetPacingInterval(d time.Duration) {
	c.pacingMu.Lock()
	c.pacingInterval = d
	c.pacingMu.Unlock()
}

func (c *Client) paceRequest() {
	c.pacingMu.Lock()
	defer c.pacingMu.Unlock()
	if c.pacingInterval <= 0 {
		return
	}
	now := time.Now()
	if !c.lastReqTime.IsZero() {
		elapsed := now.Sub(c.lastReqTime)
		if elapsed < c.pacingInterval {
			sleepDur := c.pacingInterval - elapsed
			time.Sleep(sleepDur)
			now = time.Now()
		}
	}
	c.lastReqTime = now
}

func (c *Client) acquireSlot() {
	if c.sapiSem != nil {
		c.sapiSem <- struct{}{}
	}
	c.paceRequest()
}

func (c *Client) releaseSlot() {
	if c.sapiSem != nil {
		<-c.sapiSem
	}
}

func (c *Client) refreshSession() bool {
	ok, _ := c.refreshSessionWithMode(false)
	return ok
}

func (c *Client) refreshSessionWithMode(forceOAuth bool) (bool, bool) {
	if c.refreshFn != nil {
		return c.refreshFn(), true
	}
	return c.refreshSessionInternal(forceOAuth)
}

func (c *Client) DoRequest(req *http.Request) (*http.Response, error) {
	freno, que, quien := c.breakerFor(req)
	if until := freno.blockedUntil(); !until.IsZero() {
		return nil, wafBlockedError(until, req.Method, req.URL.Path)
	}
	// El envío de contenido al servidor de subidas no ocupa el hueco de la API: dura
	// todo lo que tarde en llegar el archivo (en flujo, lo que tarde el cliente en
	// mandarlo), y retenerlo bloqueaba cualquier otra llamada a la API mientras
	// tanto: el interbloqueo con restic -> rclone, que necesita respuesta a otras
	// peticiones para seguir enviando. Las subidas tienen su propio límite (uploadSem)
	// y su propio freno del cortafuegos; la app oficial también sigue con su latido y
	// sus consultas a la API mientras sube.
	if freno == &c.wafUpload {
		c.paceRequest()
	} else {
		c.acquireSlot()
		defer c.releaseSlot()
	}
	// Las peticiones que esperaban turno cuando saltó el bloqueo tampoco se envían.
	if until := freno.blockedUntil(); !until.IsZero() {
		return nil, wafBlockedError(until, req.Method, req.URL.Path)
	}
	resp, err := c.doRequestLocked(req)
	if err == nil && resp != nil {
		if isWAFBlockResponse(resp) {
			freno.trip(que)
		} else if resp.StatusCode < 500 {
			freno.ok(quien)
		}
	}
	return resp, err
}

func (c *Client) doRequestLocked(req *http.Request) (*http.Response, error) {
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()

	// Inyectar cabeceras comunes
	desktop := auth.IsDesktopProfile()
	userAgent := auth.GetSAPIUserAgent()
	req.Header.Set("User-Agent", userAgent)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
	if desktop {
		// Mismas cabeceras que el cliente oficial de escritorio (sin Referer,
		// X-Requested-With ni X-devicename; x-request-id por petición).
		if req.Header.Get("Accept-Language") == "" {
			req.Header.Set("Accept-Language", auth.DesktopAcceptLanguage)
		}
		if req.Header.Get("x-request-id") == "" {
			req.Header.Set("x-request-id", auth.NewRequestID())
		}
	} else {
		if req.Header.Get("Referer") == "" && req.URL != nil && req.URL.Host != "" {
			req.Header.Set("Referer", req.URL.Scheme+"://"+req.URL.Host+"/")
		}
		if req.Header.Get("X-Requested-With") == "" {
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
		}
		if req.Header.Get("Accept-Language") == "" {
			req.Header.Set("Accept-Language", "es-ES,es;q=0.9,en;q=0.8")
		}
	}
	if cfg.DeviceID != "" {
		req.Header.Set("X-deviceid", auth.SanitizeDeviceID(cfg.DeviceID))
	}
	if cfg.DeviceName != "" && !desktop {
		req.Header.Set("X-devicename", cfg.DeviceName)
	}
	if cfg.AccessToken != "" {
		req.Header.Set("Authorization", auth.BuildOAuthHeader(cfg.AccessToken, cfg.RefreshToken, cfg.LastRefreshDate, cfg.ExpiresIn))
	}
	if req.Header.Get("Cookie") == "" && (cfg.ValidationKey != "" || cfg.JSessionID != "") {
		req.Header.Set("Cookie", sessionCookieHeader(cfg.JSessionID, cfg.ValidationKey))
	}

	// Asegurar validationkey en la URL si no está presente
	req.URL, _ = url.Parse(c.appendValidationKey(req.URL.String()))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			return nil, fmt.Errorf("SAPI request failed (%s %s): %w", req.Method, req.URL.Path, urlErr.Err)
		}
		return nil, err
	}

	credentialsRotated := c.processSAPIResponse(resp)

	// El cliente oficial también procesa la cabecera Authorization de las
	// respuestas 401: si SAPI entrega tokens rotados, la petición se reintenta
	// inmediatamente con la nueva credencial.
	if resp.StatusCode == http.StatusUnauthorized && credentialsRotated && (req.Body == nil || req.GetBody != nil) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		retried, retryErr := c.retryRequestWithCurrentConfig(req)
		if retryErr != nil {
			return nil, retryErr
		}
		return retried, nil
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if req.Body != nil && req.GetBody == nil {
			// Cuerpo en flujo (una subida): no se puede repetir aquí, pero sí renovar la sesión
			// para que el reintento del llamante (la subida desde disco) ya vaya con credenciales
			// válidas. Antes solo la renovaban las llamadas a la API; con la API en pausa por el
			// WAF nadie la renovaba y cada subida daba 401 (12/11/2026).
			if !credentialsRotated && c.HasCredentials() && !c.IsSessionExpired() {
				c.refreshSession()
			}
			return resp, nil
		}
		if c.HasCredentials() && !c.IsSessionExpired() {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ok, usedStep2 := c.refreshSessionWithMode(false); ok {
				resp2, err2 := c.retryRequestWithCurrentConfig(req)
				if err2 == nil && resp2.StatusCode == http.StatusUnauthorized && !usedStep2 {
					_, _ = io.Copy(io.Discard, io.LimitReader(resp2.Body, 4096))
					_ = resp2.Body.Close()
					if ok2, _ := c.refreshSessionWithMode(true); ok2 {
						resp2, err2 = c.retryRequestWithCurrentConfig(req)
					} else {
						resp2 = nil
						err2 = nil
					}
				}
				if err2 == nil && resp2 != nil {
					return resp2, nil
				}
				if err2 != nil {
					var urlErr *url.Error
					if errors.As(err2, &urlErr) && urlErr.Err != nil {
						return resp2, fmt.Errorf("SAPI retry failed (%s %s): %w", req.Method, req.URL.Path, urlErr.Err)
					}
					return resp2, err2
				}
			}
			if c.RefreshTransient() {
				return nil, fmt.Errorf("%w (%s %s)", ErrSessionRefreshTransient, req.Method, req.URL.Path)
			}
			// Restaurar cuerpo legible si RefreshSession falló para evitar "http: read on closed response body"
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
	}

	// Manejo de retroceso suave ante HTTP 429 (Too Many Requests) o 503 (Service Unavailable)
	if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusRequestTimeout) &&
		(req.Body == nil || req.GetBody != nil) {
		const maxBackoffRetries = 4
		for retry := 1; retry <= maxBackoffRetries; retry++ {
			backoff := time.Duration(1<<(retry-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			jitter := time.Duration((time.Now().UnixNano()%400)+100) * time.Millisecond
			sleepDur := backoff + jitter
			if resp.StatusCode == http.StatusRequestTimeout {
				c.httpClient.CloseIdleConnections()
			}
			logger.Log("[SAPI] ⏳ Rate limit, timeout o servicio ocupado (%s %s HTTP %d). Reintentando en %v (%d/%d)...", req.Method, req.URL.Path, resp.StatusCode, sleepDur, retry, maxBackoffRetries)
			resp.Body.Close()
			time.Sleep(sleepDur)

			var retryBody io.Reader
			if req.GetBody != nil {
				rb, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				retryBody = rb
			}
			newReq, err := http.NewRequest(req.Method, req.URL.String(), retryBody)
			if err != nil {
				return nil, err
			}
			newReq.Header = req.Header.Clone()
			respRetry, errRetry := c.httpClient.Do(newReq)
			if errRetry != nil {
				return nil, errRetry
			}
			c.processSAPIResponse(respRetry)
			if respRetry.StatusCode != http.StatusTooManyRequests && respRetry.StatusCode != http.StatusServiceUnavailable {
				return respRetry, nil
			}
			resp = respRetry
		}
	}

	return resp, nil
}

// sessionCookieHeader construye la cabecera Cookie de sesión. El cliente oficial de
// escritorio envía solo JSESSIONID (la validationkey va en la URL); el perfil
// "browser" añade además validationKey/validationkey como la web.
func sessionCookieHeader(jsessionID, validationKey string) string {
	var parts []string
	if jsessionID != "" {
		parts = append(parts, "JSESSIONID="+jsessionID)
	}
	if validationKey != "" && (!auth.IsDesktopProfile() || jsessionID == "") {
		parts = append(parts, "validationKey="+validationKey)
		parts = append(parts, "validationkey="+validationKey)
	}
	return strings.Join(parts, "; ")
}

func (c *Client) retryRequestWithCurrentConfig(req *http.Request) (*http.Response, error) {
	cfg := c.GetConfig()
	var retryBody io.Reader
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		retryBody = body
	}
	newReq, err := http.NewRequest(req.Method, req.URL.String(), retryBody)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header.Clone()
	newReq.Header.Del("Authorization")
	newReq.Header.Del("Cookie")
	if cfg.AccessToken != "" {
		newReq.Header.Set("Authorization", auth.BuildOAuthHeader(cfg.AccessToken, cfg.RefreshToken, cfg.LastRefreshDate, cfg.ExpiresIn))
	}
	if cfg.JSessionID != "" || cfg.ValidationKey != "" {
		newReq.Header.Set("Cookie", sessionCookieHeader(cfg.JSessionID, cfg.ValidationKey))
	}
	newReq.URL, _ = url.Parse(c.appendValidationKey(newReq.URL.String()))
	resp, err := c.httpClient.Do(newReq)
	if err != nil {
		return nil, err
	}
	c.processSAPIResponse(resp)
	return resp, nil
}

func (c *Client) GetSystemInfo() (map[string]interface{}, error) {
	u := fmt.Sprintf("%s/system/information?action=get", c.baseURL)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("sesión SAPI no autorizada (HTTP 401): vincula la cuenta en el panel web")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("get system info failed (HTTP %d): %s", resp.StatusCode, string(b))
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) GetStorageSpace() (*StorageSpace, error) {
	u := fmt.Sprintf("%s/media?action=get-storage-space&softdeleted=true", c.baseURL)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("sesión SAPI no autorizada (HTTP 401): vincula la cuenta en el panel web")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get storage space failed (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var root struct {
		Data struct {
			Used    interface{} `json:"used"`
			Quota   interface{} `json:"quota"`
			Total   interface{} `json:"total"`
			NoLimit bool        `json:"nolimit"`
		} `json:"data"`
	}

	if err := json.Unmarshal(bodyBytes, &root); err != nil {
		return nil, err
	}

	toInt64 := func(v interface{}) int64 {
		if v == nil {
			return 0
		}
		switch val := v.(type) {
		case float64:
			return int64(val)
		case string:
			n, _ := strconv.ParseInt(val, 10, 64)
			return n
		}
		return 0
	}

	usedBytes := toInt64(root.Data.Used)
	quotaBytes := toInt64(root.Data.Quota)
	if quotaBytes == 0 {
		quotaBytes = toInt64(root.Data.Total)
	}

	noLimit := root.Data.NoLimit
	if quotaBytes > 1000000000000000000 {
		noLimit = true
		quotaBytes = 0
	}

	var freeBytes int64
	if !noLimit && quotaBytes > usedBytes {
		freeBytes = quotaBytes - usedBytes
	}

	var rawMap map[string]interface{}
	_ = json.Unmarshal(bodyBytes, &rawMap)

	return &StorageSpace{
		UsedBytes:  usedBytes,
		TotalBytes: quotaBytes,
		FreeBytes:  freeBytes,
		NoLimit:    noLimit,
		Raw:        rawMap,
	}, nil
}

func (c *Client) GetRootFolderID() (int64, error) {
	c.mu.RLock()
	if c.rootFolderID != 0 {
		fid := c.rootFolderID
		c.mu.RUnlock()
		return fid, nil
	}
	c.mu.RUnlock()

	u := fmt.Sprintf("%s/media/folder/root?action=get", c.baseURL)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.DoRequest(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return 0, errors.New("sesión SAPI no autorizada (HTTP 401): vincula la cuenta en el panel web")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("get root folder failed (HTTP %d): %s", resp.StatusCode, string(b))
	}

	var root struct {
		Data struct {
			Folders []FolderItem `json:"folders"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return 0, err
	}

	for _, f := range root.Data.Folders {
		if f.Magic {
			c.mu.Lock()
			c.rootFolderID = f.ID
			c.mu.Unlock()
			return f.ID, nil
		}
	}
	if len(root.Data.Folders) > 0 {
		f := root.Data.Folders[0]
		c.mu.Lock()
		c.rootFolderID = f.ID
		c.mu.Unlock()
		return f.ID, nil
	}

	return 0, errors.New("no root folder found")
}

func parseFlexibleID(v interface{}) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return strconv.FormatInt(int64(val), 10)
	case int64:
		return strconv.FormatInt(val, 10)
	case int:
		return strconv.Itoa(val)
	case json.Number:
		return val.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

func parseFlexibleInt64(v interface{}) int64 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case int64:
		return val
	case int:
		return int64(val)
	case float64:
		return int64(val)
	case string:
		n, _ := strconv.ParseInt(val, 10, 64)
		return n
	case json.Number:
		n, _ := val.Int64()
		return n
	}
	return 0
}

func (c *Client) ListAllSubfolders(parentID int64) ([]FolderItem, error) {
	var allFolders []FolderItem
	offset := 0
	limit := 200

	for {
		var u string
		if offset > 0 {
			u = fmt.Sprintf("%s/media/folder?action=list&parentid=%d&limit=%d&offset=%d", c.baseURL, parentID, limit, offset)
		} else {
			u = fmt.Sprintf("%s/media/folder?action=list&parentid=%d&limit=%d", c.baseURL, parentID, limit)
		}

		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.DoRequest(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, fmt.Errorf("list subfolders failed (HTTP %d, parentid=%d): %s", resp.StatusCode, parentID, string(b))
		}

		var res struct {
			Data struct {
				Folders []FolderItem `json:"folders"`
			} `json:"data"`
			Error interface{} `json:"error"`
		}

		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			logger.Log("[SAPI] ❌ Error decodificando subcarpetas (parentid=%d): %v", parentID, err)
			return nil, fmt.Errorf("decode subfolders error: %w", err)
		}
		if res.Error != nil {
			logger.Log("[SAPI] ❌ Error API en subcarpetas (parentid=%d): %v", parentID, res.Error)
			return nil, fmt.Errorf("API error in list subfolders: %v", res.Error)
		}

		if len(res.Data.Folders) == 0 {
			break
		}
		allFolders = append(allFolders, res.Data.Folders...)
		if len(res.Data.Folders) < limit {
			break
		}
		offset += len(res.Data.Folders)
	}

	return allFolders, nil
}

func (c *Client) ListAllMediaInFolder(folderID int64) ([]MediaItem, error) {
	var allFiles []MediaItem
	offset := 0
	limit := 200

	for {
		var u string
		if offset > 0 {
			u = fmt.Sprintf("%s/media?action=get&folderid=%d&limit=%d&offset=%d", c.baseURL, folderID, limit, offset)
		} else {
			u = fmt.Sprintf("%s/media?action=get&folderid=%d&limit=%d", c.baseURL, folderID, limit)
		}

		payload := map[string]interface{}{
			"data": map[string]interface{}{
				"fields": []string{"name", "modificationdate", "creationdate", "size", "url", "etag", "folderid"},
			},
		}
		bodyBytes, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", u, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")

		resp, err := c.DoRequest(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, fmt.Errorf("list media failed (HTTP %d, folderid=%d): %s", resp.StatusCode, folderID, string(b))
		}

		var res struct {
			Data struct {
				Media []MediaItem `json:"media"`
			} `json:"data"`
			Error interface{} `json:"error"`
		}

		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			logger.Log("[SAPI] ❌ Error decodificando media (folderid=%d): %v", folderID, err)
			return nil, fmt.Errorf("decode media error: %w", err)
		}
		if res.Error != nil {
			logger.Log("[SAPI] ❌ Error API en media (folderid=%d): %v", folderID, res.Error)
			return nil, fmt.Errorf("API error in list media: %v", res.Error)
		}

		if len(res.Data.Media) == 0 {
			break
		}
		for i := range res.Data.Media {
			res.Data.Media[i].FolderID = folderID
		}
		allFiles = append(allFiles, res.Data.Media...)
		if len(res.Data.Media) < limit {
			break
		}
		offset += len(res.Data.Media)
	}

	return allFiles, nil
}

// GetMediaByID consulta los datos de un único archivo por su ID, incluida su URL de
// descarga. Devuelve (nil, nil) si SAPI responde correctamente pero todavía no lo
// conoce (p. ej. justo después de subirlo, mientras se indexa).
func (c *Client) GetMediaByID(mediaID string) (*MediaItem, error) {
	u := fmt.Sprintf("%s/media?action=get&origin=omh,dropbox", c.baseURL)
	payload := map[string]interface{}{
		"data": map[string]interface{}{
			"ids":    []string{mediaID},
			"fields": []string{"name", "size", "url", "etag", "folderid", "modificationdate"},
		},
	}
	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", u, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")

	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("get media failed (HTTP %d, id=%s): %s", resp.StatusCode, mediaID, string(b))
	}

	var res struct {
		Data struct {
			Media []MediaItem `json:"media"`
		} `json:"data"`
		Error interface{} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res); err != nil {
		return nil, fmt.Errorf("decode get media error (id=%s): %w", mediaID, err)
	}
	if res.Error != nil {
		return nil, fmt.Errorf("API error in get media (id=%s): %v", mediaID, res.Error)
	}
	for i := range res.Data.Media {
		if res.Data.Media[i].ID == mediaID {
			return &res.Data.Media[i], nil
		}
	}
	return nil, nil
}

func (c *Client) CreateFolder(name string, parentID int64) (*FolderItem, error) {
	var respBody []byte
	var status int
	if c.officialClient() {
		var err error
		respBody, status, err = c.officialFolderSave(officialFolderCreateData(name, parentID, time.Now()))
		if err != nil {
			return nil, err
		}
	} else {
		u := fmt.Sprintf("%s/media/folder?action=save", c.baseURL)
		payload := map[string]interface{}{
			"data": map[string]interface{}{
				"name":     name,
				"parentid": parentID,
				"magic":    false,
				"offline":  false,
			},
		}
		bodyBytes, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", u, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")

		resp, err := c.DoRequest(req)
		if err != nil {
			return nil, err
		}
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		status = resp.StatusCode
	}
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusNoContent {
		return nil, fmt.Errorf("create folder failed (%d): %s", status, string(respBody))
	}

	var res struct {
		ID   interface{} `json:"id"`
		Data struct {
			ID      interface{}  `json:"id"`
			Folders []FolderItem `json:"folders"`
		} `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(respBody, &res)
	if res.Error != nil && (res.Error.Code != "" || res.Error.Message != "") {
		return nil, fmt.Errorf("create folder API error: %s (%s)", res.Error.Message, res.Error.Code)
	}

	createdID := parseFlexibleInt64(res.ID)
	if createdID == 0 {
		createdID = parseFlexibleInt64(res.Data.ID)
	}
	if createdID == 0 && len(res.Data.Folders) > 0 {
		for _, f := range res.Data.Folders {
			if strings.EqualFold(f.Name, name) {
				createdID = f.ID
				break
			}
		}
		if createdID == 0 {
			createdID = res.Data.Folders[0].ID
		}
	}
	if createdID == 0 {
		subs, _ := c.ListAllSubfolders(parentID)
		for _, f := range subs {
			if strings.EqualFold(f.Name, name) {
				createdID = f.ID
				break
			}
		}
	}

	if createdID == 0 {
		return nil, fmt.Errorf("create folder: O2/Movistar Cloud no devolvió ID para la carpeta '%s' (respuesta: %.300s)", name, string(respBody))
	}

	nowRFC := time.Now().UTC().Format("20060102T150405Z")
	return &FolderItem{
		ID:               createdID,
		Name:             name,
		ParentID:         parentID,
		CreationDate:     FlexDate(nowRFC),
		ModificationDate: FlexDate(nowRFC),
	}, nil
}

func (c *Client) UpdateFolder(folderID int64, name string, parentID int64) error {
	u := fmt.Sprintf("%s/media/folder?action=save", c.baseURL)
	data := map[string]interface{}{
		"id":   folderID,
		"name": name,
	}
	if parentID != 0 {
		data["parentid"] = parentID
	}
	payload := map[string]interface{}{"data": data}
	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", u, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	resp, err := c.DoRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("update folder failed (%d): %s", resp.StatusCode, string(b))
	}
	// SAPI puede responder 200 con el error en el cuerpo.
	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err == nil && res.Error != nil {
		if res.Error.Code == "FOL-1036" {
			return ErrFolderUpdatesLimit
		}
		return fmt.Errorf("update folder API error: %s (%s)", res.Error.Message, res.Error.Code)
	}
	return nil
}

// ErrFolderUpdatesLimit (FOL-1036): la carpeta tiene más elementos de los que SAPI
// deja mover o llevar a la papelera de una vez (folders.softdelete-updates-limit y
// folders.move-updates-limit de /system/information).
var ErrFolderUpdatesLimit = errors.New("la carpeta supera el límite de elementos por operación de Movistar Cloud (FOL-1036)")

func (c *Client) DeleteFolder(folderID int64, permanent bool) error {
	if c.officialClient() && !permanent {
		// Como la app: media/folder?action=softdelete&id=<id>, cuerpo vacío.
		err := c.deleteFolderLegacy(folderID, false)
		if err == nil || errors.Is(err, ErrFolderUpdatesLimit) {
			return err
		}
		logOfficialFallback("Borrado de la carpeta", folderID, err)
		return c.deleteFolderModern(folderID, false)
	}
	err := c.deleteFolderModern(folderID, permanent)
	if err == nil || errors.Is(err, ErrFolderUpdatesLimit) {
		return err
	}
	logger.Log("[SAPI] ↪ Borrado moderno de la carpeta %d rechazado (%v); se usa la llamada genérica.", folderID, err)
	return c.deleteFolderLegacy(folderID, permanent)
}

// deleteFolderModern borra usando el endpoint moderno: a la papelera con action=softdelete y
// {"data":{"ids":[id]}}, o definitivamente con action=delete y {"data":{"folders":[id]}}.
func (c *Client) deleteFolderModern(folderID int64, permanent bool) error {
	action, key := "softdelete", "ids"
	if permanent {
		action, key = "delete", "folders"
	}
	body, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{key: []int64{folderID}}})
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/media/folder?action=%s", c.baseURL, action), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	resp, err := c.DoRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err == nil && res.Error != nil {
		switch res.Error.Code {
		case "MED-1022":
			return nil
		case "FOL-1036":
			return ErrFolderUpdatesLimit
		}
		return fmt.Errorf("delete folder API error: %s (%s)", res.Error.Message, res.Error.Code)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("delete folder failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) deleteFolderWeb(folderID int64, permanent bool) error {
	return c.deleteFolderModern(folderID, permanent)
}

func (c *Client) deleteFolderLegacy(folderID int64, permanent bool) error {
	action := "softdelete"
	if permanent {
		action = "delete"
	}
	u := fmt.Sprintf("%s/media/folder?action=%s&id=%d", c.baseURL, action, folderID)
	req, err := http.NewRequest("POST", u, strings.NewReader(""))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	resp, err := c.DoRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	bStr := string(b)
	if strings.Contains(bStr, "MED-1022") {
		return nil
	}

	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err == nil && res.Error != nil {
		if res.Error.Code == "MED-1022" {
			return nil
		}
		if res.Error.Code == "FOL-1036" {
			return ErrFolderUpdatesLimit
		}
		return fmt.Errorf("delete folder API error: %s (%s)", res.Error.Message, res.Error.Code)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("delete folder failed (%d): %s", resp.StatusCode, bStr)
	}

	return nil
}

// mediaKindForName devuelve el tipo de media con el que SAPI clasifica un archivo
// según su extensión: "audio", "video", "picture" o "file" (genérico).
//
// Comprobado con O2 Cloud subiendo y moviendo archivos reales de cada formato:
//   - audio:   mp3, m4a (AAC y ALAC), wav
//   - vídeo:   mp4
//   - imagen:  jpg, jpeg, png, gif, webp, bmp, tif, tiff y heic (la app oficial de
//     Windows sube el HEIC con upload/picture; capturado el 18/11/2026)
//   - genérico (la app no los trata como multimedia): aac, ogg, opus, wma, aiff, flac,
//     mkv, mov, m4v, 3gp, avi, webm, wmv, mpg, ts, flv, avif, svg y cualquier otro.
//   - heif también es genérico: O2 rechaza un .heif como imagen al registrarlo, por la
//     extensión, aunque sea una foto real (MED-1050 en save-metadata), y como genérico
//     lo guarda intacto (comprobado el 20/11/2026).
//
// Si una extensión clasificada como multimedia resultara genérica para SAPI,
// UpdateMedia recurre a la operación genérica.
func mediaKindForName(name string) string {
	switch strings.ToLower(name) {
	case "audio", "video", "picture", "file":
		return strings.ToLower(name)
	}
	ext := strings.ToLower(name)
	if i := strings.LastIndex(ext, "."); i >= 0 {
		ext = ext[i+1:]
	} else {
		return "file"
	}
	switch ext {
	case "mp3", "m4a", "wav":
		return "audio"
	case "mp4":
		return "video"
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "tif", "tiff", "heic":
		return "picture"
	}
	return "file"
}

// mediaMoveRetries y mediaMoveRetryDelay controlan los reintentos cuando SAPI todavía
// está validando un archivo recién subido (MED-1017), igual que en subidas y borrados.
// La espera crece con cada intento hasta mediaMoveMaxDelay (~70 s en total): algunos
// archivos tardan más de 10 s en validarse.
var (
	mediaMoveRetries    = 10
	mediaMoveRetryDelay = 1500 * time.Millisecond
	mediaMoveMaxDelay   = 10 * time.Second
)

// webAsyncUploadMin es el minAsyncUploadFileSizeBytes de la web: solo los archivos
// mayores se suben con acceptasynchronous=true (validación en segundo plano); los
// pequeños se validan en la propia subida y se pueden mover enseguida.
const webAsyncUploadMin = 209715200

func uploadAcceptsAsync(webMode bool, size int64) bool {
	return !webMode || size > webAsyncUploadMin
}

// mediaValidationPoll y mediaValidationMaxWait: consulta del estado de validación
// tras un MED-1017, como hace la web con lo recién subido.
var (
	mediaValidationPoll    = 2 * time.Second
	mediaValidationMaxWait = 2 * time.Minute
)

// MediaValidationStatus devuelve el estado de validación de un archivo
// (POST /media?action=get-validation-status, el que consulta la web tras subir).
// "V" y "A" indican que aún se está validando.
func (c *Client) MediaValidationStatus(mediaID string) (string, error) {
	return c.mediaValidationStatus(mediaID, 0)
}

// mediaValidationStatus consulta el estado; con folderID, el cuerpo es el de la app
// de escritorio: {"data":{"ids":[{"folder_id":…,"id":…}]}}.
func (c *Client) mediaValidationStatus(mediaID string, folderID int64) (string, error) {
	u := fmt.Sprintf("%s/media?action=get-validation-status", c.baseURL)
	entry := map[string]interface{}{"id": mediaID}
	if folderID > 0 {
		if n, err := strconv.ParseInt(mediaID, 10, 64); err == nil {
			entry = map[string]interface{}{"folder_id": folderID, "id": n}
		} else {
			entry["folder_id"] = folderID
		}
	}
	body, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"ids": []map[string]interface{}{entry}}})
	req, err := http.NewRequest("POST", u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	resp, err := c.DoRequest(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("validation status failed (%d): %s", resp.StatusCode, string(b))
	}
	var res struct {
		Data struct {
			IDs []struct {
				ID     json.RawMessage `json:"id"`
				Status string          `json:"status"`
			} `json:"ids"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return "", fmt.Errorf("validation status: %w", err)
	}
	for _, it := range res.Data.IDs {
		if strings.Trim(string(it.ID), `"`) == mediaID {
			return it.Status, nil
		}
	}
	return "", fmt.Errorf("%w: %s", errValidationNoData, mediaID)
}

// errValidationNoData: O2 no devuelve el estado de ese ID (ya no existe: borrado o
// sustituido).
var errValidationNoData = errors.New("validation status: sin datos")

// waitMediaValidated espera a que el archivo deje de estar en validación. Devuelve
// false si no se pudo saber o se agotó la espera.
func (c *Client) waitMediaValidated(mediaID string) bool {
	return c.waitMediaValidatedIn(mediaID, 0)
}

func (c *Client) waitMediaValidatedIn(mediaID string, folderID int64) bool {
	deadline := time.Now().Add(mediaValidationMaxWait)
	for {
		st, err := c.mediaValidationStatus(mediaID, folderID)
		if err != nil {
			return false
		}
		if st != "V" && st != "A" {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(mediaValidationPoll)
	}
}

func mediaMoveDelay(attempt int) time.Duration {
	d := mediaMoveRetryDelay * time.Duration(attempt+1)
	if d > mediaMoveMaxDelay {
		d = mediaMoveMaxDelay
	}
	return d
}

// UpdateMedia renombra y/o mueve un archivo (cambia su nombre y su carpeta).
//
// currentName es el nombre actual del archivo en la nube y newName el deseado.
// SAPI usa una operación distinta según el tipo de media (upload/audio, upload/video,
// upload/picture o upload/file); con una que no corresponde responde con un error en el
// cuerpo (a menudo con HTTP 200). El tipo se deduce del nombre actual (el tipo real del
// archivo guardado) y, si falla, del nuevo y de la genérica. Si SAPI aún está validando
// el archivo (MED-1017) se reintenta. Devuelve error si ninguna operación lo confirma.
func (c *Client) UpdateMedia(mediaID, currentName, newName string, folderID int64) error {
	return c.updateMediaKinds(mediaID, currentName, newName, func(kind string) error {
		return c.saveMediaMetadata(kind, mediaID, newName, folderID)
	})
}

// updateMediaKinds prueba save con el tipo deducido del nombre actual, del nuevo y
// el genérico, esperando a la validación si SAPI responde MED-1017.
func (c *Client) updateMediaKinds(mediaID, currentName, newName string, save func(kind string) error) error {
	var kinds []string
	for _, k := range []string{mediaKindForName(currentName), mediaKindForName(newName), "file"} {
		dup := false
		for _, e := range kinds {
			if e == k {
				dup = true
			}
		}
		if !dup {
			kinds = append(kinds, k)
		}
	}

	var lastErr error
	for i, kind := range kinds {
		var err error
		for attempt := 0; ; attempt++ {
			err = save(kind)
			if err == nil || !strings.Contains(err.Error(), "MED-1017") || attempt >= mediaMoveRetries {
				break
			}
			// Se pregunta el estado de validación; si no se puede saber, se espera a ciegas.
			if c.waitMediaValidated(mediaID) {
				logger.Log("[SAPI] ⏳ '%s' estaba en validación (MED-1017); validado, reintento el movimiento (%d/%d).", newName, attempt+1, mediaMoveRetries)
				continue
			}
			delay := mediaMoveDelay(attempt)
			logger.Log("[SAPI] ⏳ '%s' aún en validación (MED-1017) al moverlo. Reintentando en %s (%d/%d)...", newName, delay, attempt+1, mediaMoveRetries)
			time.Sleep(delay)
		}
		if err == nil {
			if i > 0 {
				logger.Log("[SAPI] ↪ Movimiento de '%s' (ID: %s) confirmado con upload/%s tras rechazo de upload/%s", newName, mediaID, kind, kinds[0])
			}
			return nil
		}
		// MED-1050 (extensión o formato no soportado, p. ej. al cambiar la extensión de un
		// audio, vídeo o imagen) es una negativa definitiva: otra operación no lo arregla.
		if strings.Contains(err.Error(), "MED-1050") {
			lastErr = err
			break
		}
		if i < len(kinds)-1 {
			logger.Log("[SAPI] ↪ upload/%s no aceptó mover '%s' (ID: %s): %v. Probando upload/%s...", kind, newName, mediaID, err, kinds[i+1])
		}
		lastErr = err
	}
	logger.Log("[SAPI] ❌ No se pudo mover '%s' (ID: %s): %v", newName, mediaID, lastErr)
	return lastErr
}

func (c *Client) saveMediaMetadata(kind, mediaID, name string, folderID int64) error {
	u := fmt.Sprintf("%s/upload/%s?action=save-metadata", c.baseURL, kind)
	data := map[string]interface{}{"id": mediaID}
	if name != "" {
		data["name"] = toCloudName(name)
	}
	if folderID != 0 {
		data["folderid"] = folderID
	}
	payload := map[string]interface{}{"data": data}
	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", u, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	resp, err := c.DoRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("update media (%s) failed (%d): %s", kind, resp.StatusCode, string(b))
	}
	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err == nil && res.Error != nil {
		return fmt.Errorf("update media (%s) API error: %s (%s)", kind, res.Error.Message, res.Error.Code)
	}
	return nil
}

func mediaPluralForKind(kind string) string {
	switch kind {
	case "picture":
		return "pictures"
	case "video":
		return "videos"
	case "audio":
		return "audios"
	default:
		return "files"
	}
}

var (
	deleteRetries      = 5
	deleteRetryBackoff = func(attempt int) time.Duration {
		return time.Duration(2500+attempt*1500) * time.Millisecond
	}
	delete403Backoff = func(attempt int) time.Duration {
		return time.Duration(3+attempt*2) * time.Second
	}
)

// modernDeleteRequest lleva a la papelera con
// /media/<tipo>?action=delete&softdelete=true y {"data":{"<tipos>":[id]}}.
func (c *Client) modernDeleteRequest(kind, mediaID string) (*http.Request, error) {
	var id interface{} = mediaID
	if n, err := strconv.ParseInt(mediaID, 10, 64); err == nil {
		id = n
	}
	body, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{mediaPluralForKind(kind): []interface{}{id}}})
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/media/%s?action=delete&softdelete=true", c.baseURL, kind), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	return req, nil
}

func (c *Client) webDeleteRequest(kind, mediaID string) (*http.Request, error) {
	return c.modernDeleteRequest(kind, mediaID)
}

func (c *Client) DeleteFile(mediaID string, nameOrKind ...string) error {
	maxRetries := deleteRetries
	u := fmt.Sprintf("%s/media?action=softdelete&id=%s", c.baseURL, mediaID)
	// Se intenta primero la operación por tipo (/media/<kind>?action=delete&softdelete=true
	// con cuerpo JSON); si la rechaza, se vuelve a la llamada genérica de siempre (/media?action=softdelete&id=...).
	targetKind := "file"
	if len(nameOrKind) > 0 {
		targetKind = mediaKindForName(nameOrKind[0])
	}
	// Perfil desktop: primero la llamada de la app (media?action=softdelete&id=, sin
	// cuerpo); si la rechazara, la operación por tipo.
	officialFirst := c.officialClient()
	fallbackKind := ""
	if officialFirst {
		fallbackKind, targetKind = targetKind, ""
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		var req *http.Request
		var err error
		if targetKind != "" {
			req, err = c.modernDeleteRequest(targetKind, mediaID)
		} else {
			req, err = http.NewRequest("POST", u, strings.NewReader(""))
			if err == nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
			}
		}
		if err != nil {
			return err
		}

		resp, err := c.DoRequest(req)
		if err != nil {
			lastErr = err
			break
		}

		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			if targetKind != "" {
				// Un 404 de la operación por tipo no prueba que el archivo ya no esté.
				targetKind = ""
				attempt--
				continue
			}
			return nil
		}

		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()

		bStr := string(b)
		if strings.Contains(bStr, "MED-1017") {
			if attempt < maxRetries {
				// Validación asíncrona dura entre 5 y 15s. Un backoff progresivo (2.5s, 4s, 5.5s, 7s, 8.5s)
				// evita activar la regla de rate-limiting (403) de AWS CloudFront WAF por ráfagas de POSTs idénticos.
				backoff := deleteRetryBackoff(attempt)
				logger.Log("[SAPI] ⏳ Archivo en validación asíncrona (MED-1017) durante borrado. Reintentando en %v (%d/%d)...", backoff, attempt+1, maxRetries)
				time.Sleep(backoff)
				continue
			}
			lastErr = fmt.Errorf("delete file failed: still in async validation (MED-1017) after %d retries: %s", maxRetries, bStr)
			break
		}

		// MED-1022: "File already soft deleted" -> éxito idempotente (igual que la app oficial)
		if strings.Contains(bStr, "MED-1022") {
			return nil
		}

		// Si CloudFront devuelve 403 (posible rate limit WAF), reintentar con backoff
		if resp.StatusCode == http.StatusForbidden {
			// Si es el bloqueo del WAF (el freno ya está puesto), no se reintenta: cada
			// reintento cuenta para el WAF y lo alarga (Hyper Backup borra en ráfagas).
			if !c.WAFBlockedUntil().IsZero() {
				lastErr = fmt.Errorf("delete file failed: %w", ErrWAFBlocked)
				break
			}
			if attempt < maxRetries {
				backoff := delete403Backoff(attempt)
				logger.Log("[SAPI] ⚠️ 403 de CloudFront al borrar '%s'. Esperando %v (%d/%d)...", mediaID, backoff, attempt+1, maxRetries)
				time.Sleep(backoff)
				continue
			}
		}

		var res struct {
			Success interface{} `json:"success"`
			Error   *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(b, &res); err == nil && res.Error != nil {
			if res.Error.Code == "MED-1022" {
				return nil
			}
			lastErr = fmt.Errorf("delete file API error: %s (%s)", res.Error.Message, res.Error.Code)
			if targetKind != "" && !officialFirst {
				logger.Log("[SAPI] ↪ Borrado de '%s' rechazado con media/%s (%s); se usa la llamada genérica.", mediaID, targetKind, res.Error.Code)
				targetKind = ""
				attempt--
				continue
			}
			if targetKind == "" && fallbackKind != "" {
				logOfficialFallback("Borrado del archivo", mediaID, lastErr)
				targetKind, fallbackKind = fallbackKind, ""
				attempt--
				continue
			}
			break
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("delete file failed (%d): %s", resp.StatusCode, bStr)
			if targetKind != "" && !officialFirst {
				logger.Log("[SAPI] ↪ Borrado de '%s' rechazado con media/%s (%d); se usa la llamada genérica.", mediaID, targetKind, resp.StatusCode)
				targetKind = ""
				attempt--
				continue
			}
			if targetKind == "" && fallbackKind != "" {
				logOfficialFallback("Borrado del archivo", mediaID, lastErr)
				targetKind, fallbackKind = fallbackKind, ""
				attempt--
				continue
			}
			break
		}

		return nil
	}
	return lastErr
}

// UploadFileToFolder sube un archivo. Si modTime no es cero, se envía como fecha de
// modificación del archivo en la nube (p. ej. la original, recibida vía X-OC-Mtime);
// si es cero, se usa la hora actual.
func (c *Client) UploadFileToFolder(fileName string, reader io.Reader, size int64, folderID int64, mimeType string, existingMediaID string, modTime time.Time) (*MediaItem, error) {
	if existingMediaID != "" && size == 0 {
		return nil, ErrEmptyOverwrite
	}
	if c.officialClient() {
		return c.uploadOfficial(officialUpload{
			name: fileName, reader: reader, size: size, folderID: folderID,
			mime: mimeType, existing: existingMediaID, modTime: modTime,
		})
	}
	c.mu.RLock()
	uploadEndpoint := fmt.Sprintf("%s/sapi/upload?action=save", c.uploadURL)
	c.mu.RUnlock()
	if uploadAcceptsAsync(c.WebMode(), size) {
		uploadEndpoint += "&acceptasynchronous=true"
	}

	nowRFC := time.Now().UTC().Format("20060102T150405Z")
	modRFC := nowRFC
	if !modTime.IsZero() {
		modRFC = modTime.UTC().Format("20060102T150405Z")
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	metaData := map[string]interface{}{
		"name":             toCloudName(fileName),
		"size":             size,
		"modificationdate": modRFC,
		"creationdate":     nowRFC,
		"contenttype":      mimeType,
		"folderid":         folderID,
	}
	if existingMediaID != "" {
		metaData["id"] = existingMediaID
		logger.Log("[SAPI] 📝 Sobrescritura in-place para ID existente: %s (%s)", existingMediaID, fileName)
	}

	meta := map[string]interface{}{
		"data": metaData,
	}
	metaBytes, _ := json.Marshal(meta)

	const maxRetries = 5
	const retryDelay = 2500 * time.Millisecond
	_, canRetry := reader.(io.ReadSeeker)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if seeker, ok := reader.(io.ReadSeeker); ok {
			_, _ = seeker.Seek(0, io.SeekStart)
		}

		pr, pw := io.Pipe()
		mpw := multipart.NewWriter(pw)

		go func() {
			var writeErr error
			defer func() {
				if writeErr != nil {
					_ = pw.CloseWithError(writeErr)
				} else {
					_ = mpw.Close()
					_ = pw.Close()
				}
			}()

			// 1. Part: data
			partData, err := mpw.CreateFormField("data")
			if err != nil {
				writeErr = err
				return
			}
			if _, err := partData.Write(metaBytes); err != nil {
				writeErr = err
				return
			}

			// 2. Part: file
			partFile, err := mpw.CreateFormFile("file", toCloudName(fileName))
			if err != nil {
				writeErr = err
				return
			}
			if _, err := io.Copy(partFile, reader); err != nil {
				writeErr = err
				return
			}
		}()

		req, err := http.NewRequest("POST", uploadEndpoint, pr)
		if err != nil {
			_ = pr.Close()
			return nil, err
		}
		req.Header.Set("Content-Type", mpw.FormDataContentType())

		resp, err := c.DoRequest(req)
		_ = pr.Close()
		if err != nil {
			return nil, err
		}

		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		bStr := string(b)

		// MED-1017 check: Media is being validated asynchronously (~4-10s)
		if strings.Contains(bStr, "MED-1017") || strings.Contains(strings.ToLower(bStr), "media not validated") {
			if attempt < maxRetries && canRetry {
				backoff := 2500*time.Millisecond + time.Duration(attempt)*1500*time.Millisecond
				logger.Log("[SAPI] ⏳ Servidor validando media asíncronamente (MED-1017). Reintentando en %v (%d/%d)...", backoff, attempt+1, maxRetries)
				time.Sleep(backoff)
				continue
			}
			return nil, fmt.Errorf("upload failed: still in async validation (MED-1017) after %d retries: %s", maxRetries, summarizeHTTPErrorBody(b))
		}

		// Manejo de retroceso ante HTTP 429 (Too Many Requests) o errores 5xx de servidor
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode >= 500 {
			if attempt < maxRetries && canRetry {
				c.httpClient.CloseIdleConnections()
				backoff := time.Duration(1<<attempt) * time.Second
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
				jitter := time.Duration((time.Now().UnixNano()%400)+100) * time.Millisecond
				sleepDur := backoff + jitter
				logger.Log("[SAPI] ⏳ Subida pausada por rate limit o error de servidor (HTTP %d). Reintentando en %v (%d/%d)...", resp.StatusCode, sleepDur, attempt+1, maxRetries)
				time.Sleep(sleepDur)
				continue
			}
			return nil, fmt.Errorf("upload failed: rate limited or server error (HTTP %d) after %d retries: %s", resp.StatusCode, maxRetries, summarizeHTTPErrorBody(b))
		}

		// 408: el host de subida cerró una conexión keep-alive obsoleta
		// ("Server timeout waiting for the HTTP request from the client").
		// Forzamos conexión nueva y reintentamos si el cuerpo es replayable.
		if resp.StatusCode == http.StatusRequestTimeout {
			if attempt < maxRetries && canRetry {
				backoff := time.Duration(1<<attempt) * time.Second
				if backoff > 4*time.Second {
					backoff = 4 * time.Second
				}
				c.httpClient.CloseIdleConnections()
				logger.Log("[SAPI] ⏳ Conexión de subida obsoleta (HTTP 408). Reintentando con conexión nueva en %v (%d/%d)...", backoff, attempt+1, maxRetries)
				time.Sleep(backoff)
				continue
			}
			return nil, fmt.Errorf("upload failed: request timeout (HTTP 408) after %d retries: %s", maxRetries, summarizeHTTPErrorBody(b))
		}

		// 401: doRequestLocked no puede reintentar un cuerpo en flujo (io.Pipe), pero ya renovó
		// la sesión o capturó credenciales rotadas; rebobinamos y reintentamos si es replayable.
		if resp.StatusCode == http.StatusUnauthorized {
			if attempt < maxRetries && canRetry && c.HasCredentials() {
				if attempt > 0 && !c.IsSessionExpired() {
					c.refreshSessionWithMode(true)
				}
				if !c.IsSessionExpired() {
					c.httpClient.CloseIdleConnections()
					backoff := 250 * time.Millisecond
					if attempt > 0 {
						backoff = time.Duration(attempt) * time.Second
					}
					logger.Log("[SAPI] 🔄 Subida de '%s' recibió HTTP 401; reintentando con sesión renovada (%d/%d)...", fileName, attempt+1, maxRetries)
					time.Sleep(backoff)
					continue
				}
			}
			return nil, fmt.Errorf("upload failed (%d): %s", resp.StatusCode, summarizeHTTPErrorBody(b))
		}

		if resp.StatusCode == http.StatusRequestEntityTooLarge || strings.Contains(bStr, "MED-1020") {
			return nil, ErrFileTooLarge
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
			return nil, fmt.Errorf("upload failed (%d): %s", resp.StatusCode, summarizeHTTPErrorBody(b))
		}

		var res struct {
			ID    interface{} `json:"id"`
			URL   string      `json:"url"`
			Name  string      `json:"name"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Data struct {
				ID   interface{} `json:"id"`
				URL  string      `json:"url"`
				Name string      `json:"name"`
			} `json:"data"`
		}

		_ = json.Unmarshal(b, &res)

		if res.Error != nil && res.Error.Code == "MED-1020" {
			return nil, ErrFileTooLarge
		}

		if res.Error != nil && res.Error.Code == "MED-1017" {
			if attempt < maxRetries {
				backoff := 2500*time.Millisecond + time.Duration(attempt)*1500*time.Millisecond
				logger.Log("[SAPI] ⏳ Servidor validando asíncronamente (MED-1017 en error body). Reintentando en %v (%d/%d)...", backoff, attempt+1, maxRetries)
				time.Sleep(backoff)
				continue
			}
			return nil, fmt.Errorf("upload failed: still in async validation (MED-1017) after %d retries: %s", maxRetries, bStr)
		}

		if res.Error != nil && (res.Error.Code == "MED-1048" || res.Error.Code == "PAPI-0000") {
			if attempt < maxRetries && canRetry {
				c.httpClient.CloseIdleConnections()
				backoff := 2000*time.Millisecond + time.Duration(attempt)*1500*time.Millisecond
				logger.Log("[SAPI] ⚠️ Error transitorio de subida (%s: %s) para %s. Reintentando en %v (%d/%d)...", res.Error.Code, res.Error.Message, fileName, backoff, attempt+1, maxRetries)
				time.Sleep(backoff)
				continue
			}
		}

		finalID := parseFlexibleID(res.ID)
		if finalID == "" {
			finalID = parseFlexibleID(res.Data.ID)
		}
		if finalID == "" {
			if res.Error != nil && res.Error.Message != "" {
				return nil, fmt.Errorf("upload failed: SAPI error %s: %s", res.Error.Code, res.Error.Message)
			}
			return nil, errors.New("subida fallida tras agotar reintentos de validación MED-1017 (ID vacío)")
		}

		finalURL := res.URL
		if finalURL == "" {
			finalURL = res.Data.URL
		}
		cloudName := res.Name
		if cloudName == "" {
			cloudName = res.Data.Name
		}
		cloudName = fromCloudName(cloudName)

		// Corregir sufijo ' (1)' si la nube lo añadió
		if finalID != "" && cloudName != "" && cloudName != fileName && strings.Contains(cloudName, " (") {
			_ = c.UpdateMedia(finalID, cloudName, fileName, folderID)
		}

		return &MediaItem{
			ID:               finalID,
			Name:             fileName,
			Size:             size,
			URL:              finalURL,
			FolderID:         folderID,
			CreationDate:     FlexDate(nowRFC),
			ModificationDate: FlexDate(modRFC),
		}, nil
	}

	return nil, errors.New("subida fallida tras agotar reintentos de validación MED-1017")
}

func (c *Client) DownloadFileStream(item MediaItem) (io.ReadCloser, error) {
	return c.DownloadFileStreamRange(item, 0)
}

func (c *Client) DownloadFileStreamRange(item MediaItem, offset int64) (io.ReadCloser, error) {
	if item.ID == "" || strings.HasPrefix(item.ID, "virtual:") {
		return c.downloadRange(item, offset)
	}
	// Las URL de descarga van firmadas y caducan (tanto en modo normal como en modo web).
	// Se usa la última URL renovada de ese archivo y, si la descarga falla, se pide una
	// URL nueva por ID y se reintenta una vez (salvo si el WAF ya está en pausa).
	if u, ok := c.freshURLs.Load(item.ID); ok {
		item.URL = u.(string)
	}
	rc, err := c.downloadRange(item, offset)
	if err == nil {
		return rc, nil
	}
	if errors.Is(err, ErrWAFBlocked) {
		return nil, err
	}
	fresh, ferr := c.GetMediaByID(item.ID)
	if ferr != nil || fresh == nil || fresh.URL == "" || fresh.URL == item.URL {
		logger.Log("[SAPI] ⚠️ Descarga de '%s' (byte %d) fallida: %v; sin URL nueva disponible.", item.Name, offset, err)
		return nil, err
	}
	logger.Log("[SAPI] 🔄 Descarga de '%s' (byte %d) fallida (%v); reintentando con URL de descarga renovada.", item.Name, offset, err)
	c.freshURLs.Store(item.ID, fresh.URL)
	item.URL = fresh.URL
	return c.downloadRange(item, offset)
}

func (c *Client) downloadRange(item MediaItem, offset int64) (io.ReadCloser, error) {
	dlURL := item.URL
	if dlURL == "" && item.ID != "" {
		dlURL = fmt.Sprintf("%s/media/content?id=%s", c.baseURL, item.ID)
	}
	if !strings.HasPrefix(dlURL, "http") {
		c.mu.RLock()
		base := c.baseURL
		c.mu.RUnlock()
		if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
			dlURL = fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, dlURL)
		} else {
			dlURL = fmt.Sprintf("https://micloud.movistar.es%s", dlURL)
		}
	}

	req, err := http.NewRequest("GET", dlURL, nil)
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	// Un 200 a una petición con rango entregaría el archivo desde el principio
	// (datos del sitio equivocado), y un cuerpo vacío no sirve para nada.
	if offset > 0 && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download ignoró el rango (HTTP 200 a bytes=%d-)", offset)
	}
	if resp.ContentLength == 0 && item.Size > offset {
		resp.Body.Close()
		return nil, fmt.Errorf("download devolvió un cuerpo vacío (HTTP %d)", resp.StatusCode)
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") || strings.Contains(ct, "text/javascript") {
		br := bufio.NewReader(resp.Body)
		peek, _ := br.Peek(4096)

		isError := isSAPIErrorJSON(peek)
		if !isError && isDefiniteBinaryFile(item.Name) {
			isError = true
		}

		if isError {
			b, _ := io.ReadAll(io.LimitReader(br, 1<<20))
			resp.Body.Close()
			return nil, fmt.Errorf("cloud returned error JSON instead of file: %s", string(b))
		}

		return &bufioReadCloser{Reader: br, Closer: resp.Body}, nil
	}

	return resp.Body, nil
}

type bufioReadCloser struct {
	*bufio.Reader
	io.Closer
}

func isSAPIErrorJSON(b []byte) bool {
	trimmed := bytes.TrimSpace(b)
	trimmed = bytes.TrimPrefix(trimmed, []byte("\xef\xbb\xbf"))
	if !bytes.HasPrefix(trimmed, []byte("{")) {
		return false
	}

	var detector struct {
		Error     interface{} `json:"error"`
		ErrorCode interface{} `json:"error_code"`
		Status    string      `json:"status"`
	}

	if err := json.Unmarshal(trimmed, &detector); err != nil {
		return false
	}

	if detector.Error != nil {
		switch v := detector.Error.(type) {
		case map[string]interface{}:
			if v["code"] != nil || v["message"] != nil {
				return true
			}
		case string:
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	if detector.ErrorCode != nil {
		switch v := detector.ErrorCode.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return true
			}
		case float64:
			return true
		}
	}
	if strings.EqualFold(detector.Status, "error") || strings.EqualFold(detector.Status, "fail") {
		return true
	}

	return false
}

func isDefiniteBinaryFile(name string) bool {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(name, "\\", "/")))
	switch ext {
	case ".tar", ".gz", ".tgz", ".bz2", ".xz", ".zip", ".7z", ".rar", ".iso", ".img",
		".bin", ".exe", ".dll", ".so", ".dmg", ".pkg", ".deb", ".rpm",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".tiff",
		".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm",
		".mp3", ".wav", ".flac", ".aac", ".ogg", ".m4a",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx":
		return true
	default:
		return false
	}
}
