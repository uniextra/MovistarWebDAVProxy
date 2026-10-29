package sapi

import (
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
	mu           sync.RWMutex
	refreshMu    sync.Mutex
	cfg          config.AccountConfig
	httpClient   *http.Client
	baseURL      string
	uploadURL    string
	maxUploadMB  int
	rootFolderID int64
}

func NewClient(cfg config.AccountConfig) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
	}

	httpClient := &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   0,
	}

	c := &Client{
		cfg:         cfg,
		httpClient:  httpClient,
		baseURL:     auth.SAPIBaseURL,
		uploadURL:   "https://upload.micloud.movistar.es",
		maxUploadMB: 4096,
	}

	// Inyectar cookies iniciales si existen
	if c.cfg.ValidationKey != "" || c.cfg.JSessionID != "" {
		sapiURL, _ := url.Parse("https://micloud.movistar.es")
		var cookies []*http.Cookie
		if c.cfg.ValidationKey != "" {
			cookies = append(cookies, &http.Cookie{
				Name:   "validationkey",
				Value:  c.cfg.ValidationKey,
				Path:   "/",
				Domain: "micloud.movistar.es",
			})
		}
		if c.cfg.JSessionID != "" {
			cookies = append(cookies, &http.Cookie{
				Name:   "JSESSIONID",
				Value:  c.cfg.JSessionID,
				Path:   "/",
				Domain: "micloud.movistar.es",
			})
		}
		jar.SetCookies(sapiURL, cookies)
	}

	return c, nil
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
	if resp == nil {
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

	logger.Log("[OAuth] 🔄 Renovando sesión SAPI...")

	// 1. Intento primario: SAPILoginOAuth con access_token actual
	if cfg.AccessToken != "" {
		res := auth.SAPILoginOAuth(c.httpClient, cfg.AccessToken, cfg.RefreshToken, cfg.DeviceID, cfg.DeviceName, cfg.LastRefreshDate, cfg.ExpiresIn)
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
			cfgCopy := c.cfg
			c.mu.Unlock()

			_ = config.SaveAccountConfig(cfgCopy)
			logger.Log("[OAuth] ✔ Sesión SAPI refrescada con éxito.")
			return true
		}
	}

	// 2. Si falla o expiró, canjear refresh_token en apiseg.telefonica.es
	if cfg.RefreshToken != "" {
		logger.Log("[OAuth] 🔄 Canjeando refresh_token en apiseg.telefonica.es...")
		tokens, err := auth.RefreshOAuthToken(cfg.RefreshToken)
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
			res := auth.SAPILoginOAuth(c.httpClient, tokens.AccessToken, cfgCopy.RefreshToken, cfgCopy.DeviceID, cfgCopy.DeviceName, cfgCopy.LastRefreshDate, cfgCopy.ExpiresIn)
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
				cfgCopy2 := c.cfg
				c.mu.Unlock()

				_ = config.SaveAccountConfig(cfgCopy2)
				logger.Log("[OAuth] ✔ Tokens renovados y sesión SAPI inicializada con éxito.")
				return true
			} else {
				logger.Log("[OAuth] ⚠️ Inicialización SAPI devolvió HTTP %d tras refresco: %v", res.StatusCode, res.Error)
			}
		} else if err != nil {
			logger.Log("[OAuth] ❌ Error canjeando refresh_token: %v", err)
		}
	}

	logger.Log("[!] ⚠️ ERROR 401: No se pudo renovar la sesión automáticamente. Requiere revinculación en el panel web.")
	return false
}

func (c *Client) DoRequest(req *http.Request) (*http.Response, error) {
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
		return nil, err
	}

	c.processSAPIResponse(resp)

	if resp.StatusCode == http.StatusUnauthorized {
		if req.Body != nil && req.GetBody == nil {
			return resp, nil
		}
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
					return resp, err
				}
				retryBody = rb
			}

			newReq, err := http.NewRequest(req.Method, req.URL.String(), retryBody)
			if err != nil {
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
			return resp2, err2
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

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
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
			b, _ := io.ReadAll(resp.Body)
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
			b, _ := io.ReadAll(resp.Body)
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

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create folder failed (%d): %s", resp.StatusCode, string(b))
	}

	var res struct {
		ID   interface{} `json:"id"`
		Data struct {
			ID      interface{}  `json:"id"`
			Folders []FolderItem `json:"folders"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
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
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update folder failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) DeleteFolder(folderID int64, permanent bool) error {
	action := "softdelete"
	if permanent {
		action = "delete"
	}
	u := fmt.Sprintf("%s/media/folder?action=%s", c.baseURL, action)
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

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete folder failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) UpdateMedia(mediaID string, name string, folderID int64) error {
	u := fmt.Sprintf("%s/upload/file?action=save-metadata", c.baseURL)
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

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update media failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) DeleteFile(mediaID string) error {
	u := fmt.Sprintf("%s/media?action=softdelete&id=%s", c.baseURL, mediaID)
	req, err := http.NewRequest("POST", u, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	resp, err := c.DoRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete file failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) UploadFileToFolder(fileName string, reader io.Reader, size int64, folderID int64, mimeType string) (*MediaItem, error) {
	u := fmt.Sprintf("%s/sapi/upload?action=save&acceptasynchronous=true", c.uploadURL)
	nowRFC := time.Now().UTC().Format("20060102T150405Z")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	meta := map[string]interface{}{
		"data": map[string]interface{}{
			"name":             fileName,
			"size":             size,
			"modificationdate": nowRFC,
			"creationdate":     nowRFC,
			"contenttype":      mimeType,
			"folderid":         folderID,
		},
	}
	metaBytes, _ := json.Marshal(meta)

	pr, pw := io.Pipe()
	mpw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer mpw.Close()

		// 1. Part: data
		partData, err := mpw.CreateFormField("data")
		if err != nil {
			return
		}
		if _, err := partData.Write(metaBytes); err != nil {
			return
		}

		// 2. Part: file
		partFile, err := mpw.CreateFormFile("file", fileName)
		if err != nil {
			return
		}
		if _, err := io.Copy(partFile, reader); err != nil {
			return
		}
	}()

	req, err := http.NewRequest("POST", u, pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mpw.FormDataContentType())

	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upload failed (%d): %s", resp.StatusCode, string(b))
	}

	var res struct {
		ID   interface{} `json:"id"`
		URL  string      `json:"url"`
		Name string      `json:"name"`
		Data struct {
			ID   interface{} `json:"id"`
			URL  string      `json:"url"`
			Name string      `json:"name"`
		} `json:"data"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&res)
	finalID := parseFlexibleID(res.ID)
	if finalID == "" {
		finalID = parseFlexibleID(res.Data.ID)
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
		_ = c.UpdateMedia(finalID, fileName, folderID)
	}

	return &MediaItem{
		ID:               finalID,
		Name:             fileName,
		Size:             size,
		URL:              finalURL,
		FolderID:         folderID,
		CreationDate:     FlexDate(nowRFC),
		ModificationDate: FlexDate(nowRFC),
	}, nil
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
		dlURL = fmt.Sprintf("https://micloud.movistar.es%s", dlURL)
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
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("cloud returned error JSON instead of file: %s", string(b))
	}

	return resp.Body, nil
}
