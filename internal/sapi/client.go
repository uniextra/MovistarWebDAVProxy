package sapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
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
	}
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
	OnSessionExpired func(err error)

	sapiSem          chan struct{}
	pacingMu         sync.Mutex
	lastReqTime      time.Time
	pacingInterval   time.Duration
	wafCooldownMu    sync.RWMutex
	wafCooldownUntil time.Time
	wafCooldownBase  time.Duration
}

func NewClient(cfg config.AccountConfig) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		TLSClientConfig:   auth.NewDesktopTLSConfig(),
		ForceAttemptHTTP2: true,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20, // <- IMPRESCINDIBLE: evita destruir sockets idle hacia micloud.movistar.es
	}

	httpClient := &http.Client{
		Jar:       jar,
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

	pacing := 150 * time.Millisecond
	if envPacing := strings.TrimSpace(os.Getenv("SAPI_PACING_MS")); envPacing != "" {
		if ms, err := strconv.Atoi(envPacing); err == nil && ms >= 0 {
			pacing = time.Duration(ms) * time.Millisecond
		}
	}

	c := &Client{
		cfg:             cfg,
		httpClient:      httpClient,
		baseURL:         baseURL,
		uploadURL:       uploadURL,
		maxUploadMB:     4096,
		sapiSem:         make(chan struct{}, maxConcurrent),
		pacingInterval:  pacing,
		wafCooldownBase: 25 * time.Second,
	}

	c.syncCookiesLocked()

	return c, nil
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
	c.syncCookiesLocked()
}

func (c *Client) SetBaseURLs(baseURL, uploadURL string) {
	c.mu.Lock()
	if baseURL != "" {
		c.baseURL = strings.TrimSuffix(baseURL, "/")
	}
	if uploadURL != "" {
		c.uploadURL = strings.TrimSuffix(uploadURL, "/")
	}
	c.mu.Unlock()
	if (strings.HasPrefix(baseURL, "http://127.0.0.1:") || strings.HasPrefix(baseURL, "http://localhost:")) && os.Getenv("SAPI_PACING_MS") == "" {
		c.SetPacingInterval(10 * time.Millisecond)
	}
}

func (c *Client) HasCredentials() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.RefreshToken != "" || c.cfg.AccessToken != "" || (c.cfg.JSessionID != "" && c.cfg.ValidationKey != "")
}

func (c *Client) appendValidationKey(rawURL string) string {
	c.mu.RLock()
	vk := c.cfg.ValidationKey
	c.mu.RUnlock()

	if vk == "" {
		return rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	q := u.Query()
	q.Set("validationkey", vk)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) processSAPIResponse(resp *http.Response) {
	if resp == nil || resp.StatusCode >= 400 {
		return
	}

	var changed bool
	c.mu.Lock()

	// 1. Cabecera Authorization devuelta con tokens rotados
	if authResp := resp.Header.Get("Authorization"); authResp != "" {
		if rotated := auth.ParseOAuthResponseHeader(authResp); rotated != nil {
			if rotated.AccessToken != "" && rotated.AccessToken != c.cfg.AccessToken {
				c.cfg.AccessToken = rotated.AccessToken
				changed = true
			}
			if rotated.RefreshToken != "" && rotated.RefreshToken != c.cfg.RefreshToken {
				c.cfg.RefreshToken = rotated.RefreshToken
				changed = true
			}
			if rotated.LastRefreshDate > 0 && rotated.LastRefreshDate != c.cfg.LastRefreshDate {
				c.cfg.LastRefreshDate = rotated.LastRefreshDate
				changed = true
			}
			if rotated.ExpiresIn != "" && rotated.ExpiresIn != c.cfg.ExpiresIn {
				c.cfg.ExpiresIn = rotated.ExpiresIn
				changed = true
			}
		}
	}

	// 2. Cookies de sesión
	for _, cookie := range resp.Cookies() {
		if strings.EqualFold(cookie.Name, "validationkey") && cookie.Value != "" && cookie.Value != c.cfg.ValidationKey {
			c.cfg.ValidationKey = cookie.Value
			changed = true
		} else if strings.EqualFold(cookie.Name, "JSESSIONID") && cookie.Value != "" && cookie.Value != c.cfg.JSessionID {
			c.cfg.JSessionID = cookie.Value
			changed = true
		}
	}

	if changed {
		c.syncCookiesLocked()
	}
	cfgCopy := c.cfg
	c.mu.Unlock()

	if changed {
		_ = config.SaveAccountConfig(cfgCopy)
		logger.Log("[OAuth] ✔ Nuevas credenciales/cookies capturadas y guardadas en disco.")
	}
}

func (c *Client) RefreshSession() bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()

	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		return false
	}

	logger.Log("[OAuth] 🔄 Renovando sesión SAPI...")

	// 1. Intento primario: SAPILoginOAuth con access_token actual
	sapiBase := auth.GetSAPIBaseURL(cfg.Provider)
	if cfg.AccessToken != "" {
		res := auth.SAPILoginOAuth(c.httpClient, sapiBase, cfg.AccessToken, cfg.RefreshToken, cfg.DeviceID, cfg.DeviceName, cfg.LastRefreshDate, cfg.ExpiresIn)
		if res.StatusCode == http.StatusOK && (res.ValidationKey != "" || res.JSessionID != "") {
			c.mu.Lock()
			if res.ValidationKey != "" {
				c.cfg.ValidationKey = res.ValidationKey
			}
			if res.JSessionID != "" {
				c.cfg.JSessionID = res.JSessionID
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
			cfgCopy := c.cfg
			c.mu.Unlock()

			_ = config.SaveAccountConfig(cfgCopy)
			logger.Log("[OAuth] ✔ Sesión SAPI refrescada con éxito.")
			return true
		}
	}

	// 2. Si falla o expiró, canjear refresh_token en apiseg.telefonica.es
	var refreshErr error
	if cfg.RefreshToken != "" {
		logger.Log("[OAuth] 🔄 Canjeando refresh_token en apiseg.telefonica.es (%s)...", cfg.Provider)
		tokens, err := auth.RefreshOAuthToken(cfg.RefreshToken, cfg.Provider)
		refreshErr = err
		if err == nil && tokens != nil && tokens.AccessToken != "" {
			c.mu.Lock()
			c.cfg.AccessToken = tokens.AccessToken
			if tokens.RefreshToken != "" {
				c.cfg.RefreshToken = tokens.RefreshToken
			}
			c.cfg.LastRefreshDate = time.Now().UnixMilli()
			cfgCopy := c.cfg
			c.mu.Unlock()

			// Persistir inmediatamente los nuevos tokens para proteger contra RTR (Refresh Token Rotation)
			_ = config.SaveAccountConfig(cfgCopy)

			// Con nuevo access_token, inicializar SAPI
			res := auth.SAPILoginOAuth(c.httpClient, sapiBase, tokens.AccessToken, cfgCopy.RefreshToken, cfgCopy.DeviceID, cfgCopy.DeviceName, cfgCopy.LastRefreshDate, cfgCopy.ExpiresIn)
			if res.StatusCode == http.StatusOK {
				c.mu.Lock()
				if res.ValidationKey != "" {
					c.cfg.ValidationKey = res.ValidationKey
				}
				if res.JSessionID != "" {
					c.cfg.JSessionID = res.JSessionID
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
				logger.Log("[OAuth] ✔ Tokens renovados y sesión SAPI inicializada con éxito.")
				return true
			} else {
				if res.StatusCode == http.StatusForbidden && isCloudFrontWAFResponseBody(nil, res.RawResponse) {
					logger.Log("[OAuth] ⚠️ Inicialización SAPI bloqueada temporalmente por CloudFront WAF (HTTP 403). Conservando credenciales.")
					c.triggerWAFCooldown(30 * time.Second)
					return false
				}
				logger.Log("[OAuth] ⚠️ Inicialización SAPI devolvió HTTP %d tras refresco: %v", res.StatusCode, res.Error)
			}
		} else if err != nil {
			logger.Log("[OAuth] ❌ Error canjeando refresh_token: %v", err)
		}
	}

	if refreshErr != nil && (strings.Contains(refreshErr.Error(), "401") || strings.Contains(refreshErr.Error(), "invalid_grant")) {
		logger.Log("[!] ⚠️ ERROR 401: No se pudo renovar la sesión automáticamente. Requiere revinculación en el panel web.")
		if c.OnSessionExpired != nil {
			go c.OnSessionExpired(errors.New("sesión expirada (HTTP 401)"))
		}
	} else {
		logger.Log("[!] ⚠️ Error transitorio durante renovación de sesión. Conservando credenciales para futuros reintentos.")
	}
	return false
}

// isCloudFrontWAFResponseBody verifica si las cabeceras o el cuerpo de la respuesta HTTP corresponden
// a un bloqueo de rate limiting / WAF por parte de AWS CloudFront (HTTP 403 Forbidden).
func isCloudFrontWAFResponseBody(header http.Header, bodyStr string) bool {
	bodyLower := strings.ToLower(bodyStr)
	hasCloudFront := strings.Contains(bodyLower, "cloudfront") ||
		strings.Contains(strings.ToLower(header.Get("Server")), "cloudfront") ||
		header.Get("X-Amz-Cf-Id") != "" ||
		strings.Contains(strings.ToLower(header.Get("Via")), "cloudfront")

	if hasCloudFront {
		if strings.Contains(bodyLower, "the request could not be satisfied") ||
			strings.Contains(bodyLower, "403 error") ||
			strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/html") {
			return true
		}
	}
	return false
}

// isCloudFrontWAFBlock comprueba si una respuesta HTTP es un bloqueo 403 de CloudFront WAF.
// Restaura resp.Body mediante io.MultiReader para no perder ningún byte downstream.
func isCloudFrontWAFBlock(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		return false
	}
	sample, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(sample), resp.Body))
	return isCloudFrontWAFResponseBody(resp.Header, string(sample))
}

func (c *Client) GetWAFCooldownRemaining() time.Duration {
	c.wafCooldownMu.RLock()
	defer c.wafCooldownMu.RUnlock()
	if time.Now().Before(c.wafCooldownUntil) {
		return time.Until(c.wafCooldownUntil)
	}
	return 0
}

func (c *Client) getWAFCooldownRemaining() time.Duration {
	return c.GetWAFCooldownRemaining()
}

func (c *Client) triggerWAFCooldown(d time.Duration) {
	c.wafCooldownMu.Lock()
	defer c.wafCooldownMu.Unlock()
	target := time.Now().Add(d)
	if target.After(c.wafCooldownUntil) {
		c.wafCooldownUntil = target
		logger.Log("[SAPI] 🛑 Pausa global de seguridad por CloudFront WAF activada durante %v...", d.Round(time.Second))
	}
}

func (c *Client) waitForWAFCooldown() {
	for {
		rem := c.getWAFCooldownRemaining()
		if rem <= 0 {
			break
		}
		logger.Log("[SAPI] ⏳ Petición en cola esperando fin de pausa WAF (%v restantes)...", rem.Round(time.Second))
		time.Sleep(rem)
	}
}

func (c *Client) SetWAFCooldownBase(d time.Duration) {
	c.mu.Lock()
	c.wafCooldownBase = d
	c.mu.Unlock()
	if d < time.Second {
		c.SetPacingInterval(5 * time.Millisecond)
	}
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
	c.waitForWAFCooldown()
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

func (c *Client) DoRequest(req *http.Request) (*http.Response, error) {
	c.acquireSlot()
	holdingSlot := true
	defer func() {
		if holdingSlot {
			c.releaseSlot()
		}
	}()

	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()

	// Inyectar cabeceras comunes
	req.Header.Set("User-Agent", auth.DesktopUserAgent)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
	if cfg.DeviceID != "" {
		req.Header.Set("X-deviceid", cfg.DeviceID)
	}
	if cfg.DeviceName != "" {
		req.Header.Set("X-devicename", cfg.DeviceName)
	}
	if cfg.AccessToken != "" {
		req.Header.Set("Authorization", auth.BuildOAuthHeader(cfg.AccessToken, cfg.RefreshToken, cfg.LastRefreshDate, cfg.ExpiresIn))
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

	c.processSAPIResponse(resp)

	if resp.StatusCode == http.StatusUnauthorized {
		if req.Body != nil && req.GetBody == nil {
			return resp, nil
		}
		if c.HasCredentials() {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if c.RefreshSession() {
				// Reintentar petición con nuevas credenciales
				c.mu.RLock()
				newCfg := c.cfg
				c.mu.RUnlock()

				var retryBody io.Reader
				if req.GetBody != nil {
					rb, err := req.GetBody()
					if err != nil {
						resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
						return resp, err
					}
					retryBody = rb
				}

				newReq, err := http.NewRequest(req.Method, req.URL.String(), retryBody)
				if err != nil {
					resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					return resp, err
				}
				newReq.Header = req.Header.Clone()
				newReq.Header.Set("Authorization", auth.BuildOAuthHeader(newCfg.AccessToken, newCfg.RefreshToken, newCfg.LastRefreshDate, newCfg.ExpiresIn))
				newReq.URL, _ = url.Parse(c.appendValidationKey(newReq.URL.String()))

				resp2, err2 := c.httpClient.Do(newReq)
				if err2 == nil {
					c.processSAPIResponse(resp2)
					return resp2, nil
				}
				var urlErr *url.Error
				if errors.As(err2, &urlErr) && urlErr.Err != nil {
					return resp2, fmt.Errorf("SAPI retry failed (%s %s): %w", newReq.Method, newReq.URL.Path, urlErr.Err)
				}
				return resp2, err2
			}
			// Restaurar cuerpo legible si RefreshSession falló para evitar "http: read on closed response body"
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
	}

	// Manejo de retroceso ante HTTP 403 de AWS CloudFront WAF (bloqueo por ráfagas de HyperBackup / restic)
	isWAF := isCloudFrontWAFBlock(resp)
	if isWAF && (req.Body == nil || req.GetBody != nil) {
		const maxWAFRetries = 3
		c.mu.RLock()
		base := c.wafCooldownBase
		c.mu.RUnlock()
		if base == 0 {
			base = 25 * time.Second
		}
		for retry := 1; retry <= maxWAFRetries; retry++ {
			step := 5 * time.Second
			jitterMax := int64(1000 * time.Millisecond)
			if base < time.Second {
				step = base
				jitterMax = int64(10 * time.Millisecond)
			}
			cooldown := base + time.Duration(retry-1)*step
			jitter := time.Duration(time.Now().UnixNano() % jitterMax)
			sleepDur := cooldown + jitter
			c.triggerWAFCooldown(sleepDur)
			logger.Log("[SAPI] ⏳ AWS CloudFront WAF 403 detectado (%s %s). Pausando todas las peticiones durante %v para drenar WAF (%d/%d)...", req.Method, req.URL.Path, sleepDur.Round(time.Second), retry, maxWAFRetries)
			resp.Body.Close()

			// Liberar semáforo durante la espera WAF y re-adquirir con espaciado para evitar estampidas (convoy)
			if holdingSlot {
				c.releaseSlot()
				holdingSlot = false
			}
			time.Sleep(sleepDur)
			c.acquireSlot()
			holdingSlot = true

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
			if !isCloudFrontWAFBlock(respRetry) {
				logger.Log("[SAPI] ✔ Conexión con Movistar/O2 Cloud reanudada con éxito tras bloqueo WAF.")
				return respRetry, nil
			}
			resp = respRetry
		}
	} else if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) &&
		(req.Body == nil || req.GetBody != nil) {
		const maxBackoffRetries = 4
		for retry := 1; retry <= maxBackoffRetries; retry++ {
			backoff := time.Duration(1<<(retry-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			jitter := time.Duration((time.Now().UnixNano()%400)+100) * time.Millisecond
			sleepDur := backoff + jitter
			logger.Log("[SAPI] ⏳ Rate limit o servicio ocupado (%s %s HTTP %d). Reintentando en %v (%d/%d)...", req.Method, req.URL.Path, resp.StatusCode, sleepDur, retry, maxBackoffRetries)
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
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("create folder failed (%d): %s", resp.StatusCode, string(respBody))
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

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("update folder failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) DeleteFolder(folderID int64, permanent bool) error {
	action := "softdelete"
	if permanent {
		action = "delete"
	}
	u := fmt.Sprintf("%s/media/folder?action=%s&id=%d", c.baseURL, action, folderID)
	payload := map[string]interface{}{
		"data": map[string]interface{}{
			"folders": []int64{folderID},
		},
	}
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
		return fmt.Errorf("delete folder failed (%d): %s", resp.StatusCode, string(b))
	}

	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err == nil && res.Error != nil {
		return fmt.Errorf("delete folder API error: %s (%s)", res.Error.Message, res.Error.Code)
	}
	return nil
}

// mediaKindForName devuelve el tipo de media con el que SAPI clasifica un archivo
// según su extensión: "audio", "video", "picture" o "file" (genérico).
//
// Comprobado con O2 Cloud subiendo y moviendo archivos reales de cada formato:
//   - audio:   mp3, m4a (AAC y ALAC), wav
//   - vídeo:   mp4
//   - imagen:  jpg, jpeg, png, gif, webp, bmp, tif, tiff
//   - genérico (la app no los trata como multimedia): aac, ogg, opus, wma, aiff, flac,
//     mkv, mov, m4v, 3gp, avi, webm, wmv, mpg, ts, flv, heic, heif, avif, svg y
//     cualquier otro.
//
// Si una extensión clasificada como multimedia resultara genérica para SAPI,
// UpdateMedia recurre a la operación genérica.
func mediaKindForName(name string) string {
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
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "tif", "tiff":
		return "picture"
	}
	return "file"
}

// mediaMoveRetries y mediaMoveRetryDelay controlan los reintentos cuando SAPI todavía
// está validando un archivo recién subido (MED-1017), igual que en subidas y borrados.
var (
	mediaMoveRetries    = 6
	mediaMoveRetryDelay = 1500 * time.Millisecond
)

// UpdateMedia renombra y/o mueve un archivo (cambia su nombre y su carpeta).
//
// currentName es el nombre actual del archivo en la nube y newName el deseado.
// SAPI usa una operación distinta según el tipo de media (upload/audio, upload/video,
// upload/picture o upload/file); con una que no corresponde responde con un error en el
// cuerpo (a menudo con HTTP 200). El tipo se deduce del nombre actual (el tipo real del
// archivo guardado) y, si falla, del nuevo y de la genérica. Si SAPI aún está validando
// el archivo (MED-1017) se reintenta. Devuelve error si ninguna operación lo confirma.
func (c *Client) UpdateMedia(mediaID, currentName, newName string, folderID int64) error {
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
			err = c.saveMediaMetadata(kind, mediaID, newName, folderID)
			if err == nil || !strings.Contains(err.Error(), "MED-1017") || attempt >= mediaMoveRetries {
				break
			}
			logger.Log("[SAPI] ⏳ '%s' aún en validación (MED-1017) al moverlo. Reintentando en %s (%d/%d)...", newName, mediaMoveRetryDelay, attempt+1, mediaMoveRetries)
			time.Sleep(mediaMoveRetryDelay)
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
		data["name"] = name
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

func (c *Client) DeleteFile(mediaID string) error {
	const maxRetries = 10
	const delay = 1500 * time.Millisecond

	for attempt := 0; attempt <= maxRetries; attempt++ {
		u := fmt.Sprintf("%s/media?action=softdelete&id=%s", c.baseURL, mediaID)
		req, err := http.NewRequest("POST", u, bytes.NewReader([]byte("{}")))
		if err != nil {
			return err
		}
		resp, err := c.DoRequest(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()

		bStr := string(b)
		if strings.Contains(bStr, "MED-1017") {
			if attempt < maxRetries {
				logger.Log("[SAPI] ⏳ Archivo en validación asíncrona (MED-1017) durante borrado. Reintentando en 1.5s (%d/%d)...", attempt+1, maxRetries)
				time.Sleep(delay)
				continue
			}
			return fmt.Errorf("delete file failed: still in async validation (MED-1017) after %d retries: %s", maxRetries, bStr)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("delete file failed (%d): %s", resp.StatusCode, bStr)
		}
		return nil
	}
	return errors.New("delete file failed tras agotar reintentos")
}

// UploadFileToFolder sube un archivo. Si modTime no es cero, se envía como fecha de
// modificación del archivo en la nube (p. ej. la original, recibida vía X-OC-Mtime);
// si es cero, se usa la hora actual.
func (c *Client) UploadFileToFolder(fileName string, reader io.Reader, size int64, folderID int64, mimeType string, existingMediaID string, modTime time.Time) (*MediaItem, error) {
	c.mu.RLock()
	uploadEndpoint := fmt.Sprintf("%s/sapi/upload?action=save&acceptasynchronous=true", c.uploadURL)
	c.mu.RUnlock()

	nowRFC := time.Now().UTC().Format("20060102T150405Z")
	modRFC := nowRFC
	if !modTime.IsZero() {
		modRFC = modTime.UTC().Format("20060102T150405Z")
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	metaData := map[string]interface{}{
		"name":             fileName,
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

	const maxRetries = 14
	const retryDelay = 1500 * time.Millisecond
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
			partFile, err := mpw.CreateFormFile("file", fileName)
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
				logger.Log("[SAPI] ⏳ Servidor validando media asíncronamente (MED-1017). Reintentando en 1.5s (%d/%d)...", attempt+1, maxRetries)
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("upload failed: still in async validation (MED-1017) after %d retries: %s", maxRetries, bStr)
		}

		// Manejo de retroceso ante CloudFront WAF 403
		isWAF := resp.StatusCode == http.StatusForbidden && isCloudFrontWAFResponseBody(resp.Header, bStr)
		if isWAF {
			if attempt < maxRetries && canRetry {
				cooldown := 30*time.Second + time.Duration((time.Now().UnixNano()%1000))*time.Millisecond
				c.triggerWAFCooldown(cooldown)
				logger.Log("[SAPI] ⏳ Subida pausada por CloudFront WAF 403. Pausando %v para drenar WAF (%d/%d)...", cooldown.Round(time.Second), attempt+1, maxRetries)
				time.Sleep(cooldown)
				continue
			}
			return nil, fmt.Errorf("upload failed: blocked by CloudFront WAF (HTTP 403) after %d retries: %s", maxRetries, bStr)
		}

		// Manejo de retroceso ante HTTP 429 (Too Many Requests) o 503 (Service Unavailable)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			if attempt < maxRetries && canRetry {
				backoff := time.Duration(1<<attempt) * time.Second
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
				jitter := time.Duration((time.Now().UnixNano()%400)+100) * time.Millisecond
				sleepDur := backoff + jitter
				logger.Log("[SAPI] ⏳ Subida pausada por rate limit o servicio ocupado (HTTP %d). Reintentando en %v (%d/%d)...", resp.StatusCode, sleepDur, attempt+1, maxRetries)
				time.Sleep(sleepDur)
				continue
			}
			return nil, fmt.Errorf("upload failed: rate limited or service unavailable (HTTP %d) after %d retries: %s", resp.StatusCode, maxRetries, bStr)
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
			return nil, fmt.Errorf("upload failed (%d): %s", resp.StatusCode, bStr)
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

		if res.Error != nil && res.Error.Code == "MED-1017" {
			if attempt < maxRetries {
				logger.Log("[SAPI] ⏳ Servidor validando asíncronamente (MED-1017 en error body). Reintentando en 1.5s (%d/%d)...", attempt+1, maxRetries)
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("upload failed: still in async validation (MED-1017) after %d retries: %s", maxRetries, bStr)
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
