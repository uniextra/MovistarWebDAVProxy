package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	OAuthAuthorizeURL = "https://apiseg.telefonica.es/openid/connect/cus/col/auth/oauth/v2/authorize"
	OAuthTokenURL     = "https://apiseg.telefonica.es/openid/connect/cus/col/auth/oauth/v2/token"
	OAuthClientID     = "b99e5095-7a36-42c1-83b6-6f05d0f13ea1"
	OAuthClientSecret = "e22fdd6b-636a-4bfc-bb4a-aa8ed6d131d8"
	OAuthRedirectURI  = "https://micloud.movistar.es/ui/html/clientoauth.html"
	ClientPlatform    = "windows"
	DesktopUserAgent  = "omh windows client 4.0.1"
	SAPIUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
	SAPIBaseURL       = "https://micloud.movistar.es/sapi"
)

func newDesktopTransport() http.RoundTripper {
	return NewFingerprintedTransport()
}

// GetSAPIUserAgent permite cambiar la huella de navegador de SAPI sin
// recompilar cuando CloudFront vuelve a endurecer sus reglas.
func GetSAPIUserAgent() string {
	if value := strings.TrimSpace(os.Getenv("SAPI_USER_AGENT")); value != "" {
		return value
	}
	if IsDesktopProfile() {
		return GetDesktopUserAgent()
	}
	return SAPIUserAgent
	// ua, _ := GetSAPIUserAgentInfo()	return ua si se quiere usar función secundaria
}

// GetSAPIUserAgentInfo devuelve el User-Agent activo y true si se ha modificado
// mediante SAPI_USER_AGENT respecto a la constante por defecto SAPIUserAgent.
func GetSAPIUserAgentInfo() (string, bool) {
	def := SAPIUserAgent
	if IsDesktopProfile() {
		def = GetDesktopUserAgent()
	}
	if value := strings.TrimSpace(os.Getenv("SAPI_USER_AGENT")); value != "" && value != def {
		return value, true
	}
	return def, false
}

// DesktopUserAgent permite cambiar DesktopUserAgent.
func GetDesktopUserAgent() string {
	if value := strings.TrimSpace(os.Getenv("DESKTOP_USER_AGENT")); value != "" {
		return value
	}
	return DesktopUserAgent
}

// GetDesktopUserAgentInfo devuelve el User-Agent de escritorio activo y true si
// se ha modificado mediante DESKTOP_USER_AGENT respecto a DesktopUserAgent original.
func GetDesktopUserAgentInfo() (string, bool) {
	if value := strings.TrimSpace(os.Getenv("DESKTOP_USER_AGENT")); value != "" && value != DesktopUserAgent {
		return value, true
	}
	return DesktopUserAgent, false
}


var (
	authCodeQueryRegex = regexp.MustCompile(`code=([A-Za-z0-9\-_.~]+)`)
	validAuthCodeRegex = regexp.MustCompile(`^[A-Za-z0-9\-_.~]{1,512}$`)
	stateQueryRegex    = regexp.MustCompile(`state=([A-Za-z0-9\-_.~%]+)`)
	oauthHTTPClient    = &http.Client{Timeout: 30 * time.Second, Transport: newDesktopTransport()}
	sapiLoginClient    = &http.Client{Timeout: 20 * time.Second, Transport: newDesktopTransport()}
)

func GetOAuthAuthorizeURL(provider string) string {
	if env := strings.TrimSpace(os.Getenv("OAUTH_AUTHORIZE_URL")); env != "" {
		return env
	}
	if strings.EqualFold(provider, "o2") {
		return "https://apiseg.telefonica.es/openid/connect/auth/oauth/v2/o2/cus/authorize"
	}
	return OAuthAuthorizeURL
}

func GetOAuthTokenURL(provider string) string {
	if env := strings.TrimSpace(os.Getenv("OAUTH_TOKEN_URL")); env != "" {
		return env
	}
	if strings.EqualFold(provider, "o2") {
		return "https://apiseg.telefonica.es/openid/connect/auth/oauth/v2/o2/cus/token"
	}
	return OAuthTokenURL
}

func GetOAuthClientID(provider string) string {
	if strings.EqualFold(provider, "o2") {
		if env := strings.TrimSpace(os.Getenv("OAUTH_CLIENT_ID_O2")); env != "" {
			return env
		}
		return "21ce2068-4966-45dc-8b7f-a4a25a3a462f"
	}
	if env := strings.TrimSpace(os.Getenv("OAUTH_CLIENT_ID_MOVISTAR")); env != "" {
		return env
	}
	return OAuthClientID
}

func GetOAuthClientSecret(provider string) string {
	if strings.EqualFold(provider, "o2") {
		if env := strings.TrimSpace(os.Getenv("OAUTH_CLIENT_SECRET_O2")); env != "" {
			return env
		}
		return "aa892341-04b1-4fd5-be41-db60d4e900e7"
	}
	if env := strings.TrimSpace(os.Getenv("OAUTH_CLIENT_SECRET_MOVISTAR")); env != "" {
		return env
	}
	return OAuthClientSecret
}

func GetOAuthRedirectURI(provider string) string {
	if strings.EqualFold(provider, "o2") {
		return "https://cloud.o2online.es/ui/html/clientoauth.html"
	}
	return "https://micloud.movistar.es/ui/html/clientoauth.html"
}

// NewSAPILoginSession prepara un cliente con cookies compartidas entre la
// página web de retorno y /sapi/login/oauth. El cliente oficial mantiene esa
// misma sesión de navegador durante ambos pasos.
func NewSAPILoginSession(provider string) (*http.Client, string, error) {
	client, cookies, err := NewSAPILoginSessionAt(provider, GetOAuthRedirectURI(provider))
	return client, cookies.JSessionID, err
}

// NewSAPILoginSessionAt imita mejor al webview oficial: carga la URL final
// clientoauth.html, con code/state, antes de canjear el código OAuth.
func NewSAPILoginSessionAt(provider, sessionURL string) (*http.Client, SAPILoginSessionCookies, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, SAPILoginSessionCookies{}, err
	}

	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: newDesktopTransport(),
		Jar:       jar,
	}
	if strings.TrimSpace(sessionURL) == "" {
		sessionURL = GetOAuthRedirectURI(provider)
	}
	cookies, err := primeSAPILoginSession(client, sessionURL)
	return client, cookies, err
}

func primeSAPILoginSession(client *http.Client, sessionURL string) (SAPILoginSessionCookies, error) {
	parsedURL, err := url.Parse(sessionURL)
	if err != nil {
		return SAPILoginSessionCookies{}, err
	}

	req, err := http.NewRequest(http.MethodGet, sessionURL, nil)
	if err != nil {
		return SAPILoginSessionCookies{}, err
	}
	req.Header.Set("User-Agent", GetSAPIUserAgent())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", parsedURL.Scheme+"://"+parsedURL.Host+"/")

	resp, err := client.Do(req)
	if err != nil {
		return SAPILoginSessionCookies{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return SAPILoginSessionCookies{}, fmt.Errorf("prepare SAPI login session: HTTP %d", resp.StatusCode)
	}

	var cookies SAPILoginSessionCookies
	if client.Jar != nil {
		for _, cookie := range client.Jar.Cookies(parsedURL) {
			cookies.Names = append(cookies.Names, cookie.Name)
			switch {
			case strings.EqualFold(cookie.Name, "JSESSIONID"):
				cookies.JSessionID = cookie.Value
			case strings.EqualFold(cookie.Name, "validationkey"):
				cookies.ValidationKey = cookie.Value
			}
		}
	}
	return cookies, nil
}

func GetSAPIBaseURL(provider string) string {
	if env := strings.TrimSpace(os.Getenv("SAPI_BASE_URL")); env != "" {
		return env
	}
	if strings.EqualFold(provider, "o2") {
		return "https://cloud.o2online.es/sapi"
	}
	return "https://micloud.movistar.es/sapi"
}

func GetUploadBaseURL(provider string) string {
	if strings.EqualFold(provider, "o2") {
		return "https://upload.cloud.o2online.es"
	}
	return "https://upload.micloud.movistar.es"
}

type OAuthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

type OAuthCredentialData struct {
	AccessToken     string `json:"accesstoken"`
	LastRefreshDate int64  `json:"lastrefreshdate"`
	ExpiresIn       string `json:"expiresin"`
	RefreshToken    string `json:"refreshtoken"`
	Platform        string `json:"platform"`
}

type OAuthCredentialHeader struct {
	Data OAuthCredentialData `json:"data"`
}

type RotatedOAuth struct {
	AccessToken     string
	RefreshToken    string
	ExpiresIn       string
	LastRefreshDate int64
}

type SAPILoginResult struct {
	StatusCode          int
	ValidationKey       string
	JSessionID          string
	RefreshedOAuth      *RotatedOAuth
	LastRefreshDate     int64
	ResponseTime        int64
	RawResponse         string
	RequestURL          string
	Protocol            string
	ContentType         string
	Server              string
	XCache              string
	CloudFrontRequestID string
	WWWAuthenticate     string
	ResponseCookieNames []string
	BodyPreview         string
	Error               error
}

type SAPILoginSessionCookies struct {
	JSessionID    string
	ValidationKey string
	Names         []string
}

var sensitiveLoginBodyRegex = regexp.MustCompile(`(?i)(accesstoken|access_token|refreshtoken|refresh_token|validationkey|jsessionid)([=:]\s*["']?)[^&"'\s<]+`)

func summarizeSAPILoginBody(body []byte, secrets ...string) string {
	preview := strings.ToValidUTF8(string(body), "?")
	for _, secret := range secrets {
		if len(secret) >= 4 {
			preview = strings.ReplaceAll(preview, secret, "<redacted>")
		}
	}
	preview = sensitiveLoginBodyRegex.ReplaceAllString(preview, `${1}${2}<redacted>`)
	preview = strings.Join(strings.Fields(preview), " ")
	const maxPreview = 600
	if len(preview) > maxPreview {
		preview = preview[:maxPreview] + "…"
	}
	return preview
}

func mustRandRead(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand.Read failed: " + err.Error())
	}
}

// SanitizeDeviceID adapta el identificador de dispositivo al perfil activo.
// Perfil "desktop": formato del cliente oficial "fol-<base64>"; un "web-…" heredado
// se convierte de forma estable. Perfil "browser": "web-…" sin '+', '/', '='.
func SanitizeDeviceID(devID string) string {
	devID = strings.TrimSpace(devID)
	if IsDesktopProfile() {
		if devID == "" || strings.HasPrefix(devID, "fol-") {
			return devID
		}
		return desktopDeviceIDFrom(devID)
	}
	if strings.HasPrefix(devID, "fol-") {
		clean := strings.TrimPrefix(devID, "fol-")
		clean = strings.NewReplacer("+", "", "/", "", "=", "").Replace(clean)
		return "web-" + clean
	}
	return devID
}

func GenerateDeviceID() string {
	if IsDesktopProfile() {
		return generateDesktopDeviceID()
	}
	b := make([]byte, 16)
	mustRandRead(b)
	return fmt.Sprintf("web-%x", b)
}

func GeneratePKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	mustRandRead(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	if len(verifier) > 64 {
		verifier = verifier[:64]
	}
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge
}

func BuildOAuthLoginURL(provider string) (verifier, state, authURL string) {
	verifier, challenge := GeneratePKCE()
	stateB := make([]byte, 12)
	nonceB := make([]byte, 12)
	mustRandRead(stateB)
	mustRandRead(nonceB)
	state = base64.RawURLEncoding.EncodeToString(stateB)
	nonce := base64.RawURLEncoding.EncodeToString(nonceB)

	redirectURI := GetOAuthRedirectURI(provider)
	clientID := GetOAuthClientID(provider)
	authorizeURL := GetOAuthAuthorizeURL(provider)

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", "openid")
	params.Set("nonce", nonce)
	params.Set("state", state)
	params.Set("code_challenge_method", "S256")
	params.Set("code_challenge", challenge)
	params.Set("access_type", "offline")
	params.Set("lang", "es")
	params.Set("acr_values", "2")

	authURL = fmt.Sprintf("%s?%s", authorizeURL, params.Encode())
	return verifier, state, authURL
}

func ExtractAuthCode(input string) string {
	cleaned := strings.TrimSpace(input)
	if cleaned == "" {
		return ""
	}
	var candidate string
	if strings.Contains(cleaned, "code=") {
		if u, err := url.Parse(cleaned); err == nil && u.Query().Get("code") != "" {
			candidate = u.Query().Get("code")
		} else if m := authCodeQueryRegex.FindStringSubmatch(cleaned); len(m) > 1 {
			candidate = m[1]
		}
	} else {
		candidate = cleaned
	}

	if !validAuthCodeRegex.MatchString(candidate) {
		return ""
	}
	return candidate
}

// ExtractState extrae el parámetro state si la entrada es una URL de redirección OAuth.
func ExtractState(input string) (string, bool) {
	cleaned := strings.TrimSpace(input)
	if !strings.Contains(cleaned, "state=") && !strings.HasPrefix(strings.ToLower(cleaned), "http") {
		return "", false
	}
	if u, err := url.Parse(cleaned); err == nil {
		if q := u.Query(); q.Has("state") {
			return q.Get("state"), true
		}
	}
	if m := stateQueryRegex.FindStringSubmatch(cleaned); len(m) > 1 {
		unesc, err := url.QueryUnescape(m[1])
		if err == nil {
			return unesc, true
		}
		return m[1], true
	}
	return "", true
}

// getOAuthUserAgent devuelve el User-Agent para peticiones OAuth (canje y refresco).
// Si el usuario especificó explícitamente DESKTOP_USER_AGENT, se respeta; en caso contrario,
// usa GetSAPIUserAgent() para alinear la cabecera con la huella TLS (uTLS Chrome-133)
// y los tokens web tanto en Movistar como en O2.
func getOAuthUserAgent() string {
	if ua, custom := GetDesktopUserAgentInfo(); custom {
		return ua
	}
	return GetSAPIUserAgent()
}

func ExchangeCodeForTokens(code, verifier, provider string) (*OAuthTokens, error) {
	redirectURI := GetOAuthRedirectURI(provider)
	tokenURL := GetOAuthTokenURL(provider)
	clientID := GetOAuthClientID(provider)
	clientSecret := GetOAuthClientSecret(provider)

	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", redirectURI)
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	if verifier != "" {
		data.Set("code_verifier", verifier)
	}

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}

	basicAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", clientID, clientSecret)))
	req.Header.Set("Authorization", "Basic "+basicAuth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("User-Agent", getOAuthUserAgent())

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OAuth token exchange failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var tokens OAuthTokens
	if err := json.Unmarshal(bodyBytes, &tokens); err != nil {
		return nil, err
	}
	return &tokens, nil
}

func doRefreshOAuthTokenRequest(tokenURL, clientID, clientSecret, refreshToken string, includeBodyCreds bool) (*OAuthTokens, int, []byte, error) {
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	if includeBodyCreds {
		data.Set("client_id", clientID)
		data.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, 0, nil, err
	}

	basicAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", clientID, clientSecret)))
	req.Header.Set("Authorization", "Basic "+basicAuth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("User-Agent", getOAuthUserAgent())

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, bodyBytes, nil
	}

	var tokens OAuthTokens
	if err := json.Unmarshal(bodyBytes, &tokens); err != nil {
		return nil, resp.StatusCode, bodyBytes, err
	}
	return &tokens, resp.StatusCode, bodyBytes, nil
}

func RefreshOAuthToken(refreshToken, provider string) (*OAuthTokens, error) {
	tokenURL := GetOAuthTokenURL(provider)
	clientID := GetOAuthClientID(provider)
	clientSecret := GetOAuthClientSecret(provider)

	// Conforme a RFC 6749 §2.3.1, enviamos primero solo Authorization: Basic.
	tokens, statusCode, bodyBytes, err := doRefreshOAuthTokenRequest(tokenURL, clientID, clientSecret, refreshToken, false)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusOK && tokens != nil {
		return tokens, nil
	}

	// Fallback de compatibilidad incluyendo client_id y client_secret en el cuerpo
	if statusCode >= 400 && statusCode != http.StatusTooManyRequests {
		tokens2, statusCode2, bodyBytes2, err2 := doRefreshOAuthTokenRequest(tokenURL, clientID, clientSecret, refreshToken, true)
		if err2 != nil {
			return nil, err2
		}
		if statusCode2 == http.StatusOK && tokens2 != nil {
			return tokens2, nil
		}
		statusCode = statusCode2
		bodyBytes = bodyBytes2
	}

	summary := summarizeSAPILoginBody(bodyBytes, refreshToken, clientSecret)
	if summary != "" {
		return nil, fmt.Errorf("OAuth token refresh failed (HTTP %d): %s", statusCode, summary)
	}
	return nil, fmt.Errorf("OAuth token refresh failed (HTTP %d)", statusCode)
}

func BuildOAuthHeader(accessToken, refreshToken string, lastRefreshDate int64, expiresIn string) string {
	lrd := lastRefreshDate
	if lrd <= 0 {
		lrd = time.Now().UnixMilli()
	} else if lrd > 0 && lrd < 100000000000 {
		lrd = lrd * 1000
	}

	exp := expiresIn
	if exp == "" {
		exp = "3600"
	}

	cred := OAuthCredentialHeader{
		Data: OAuthCredentialData{
			AccessToken:     accessToken,
			ExpiresIn:       exp,
			LastRefreshDate: lrd,
			Platform:        ClientPlatform,
			RefreshToken:    refreshToken,
		},
	}

	b, _ := json.Marshal(cred)
	return fmt.Sprintf("oauth %s", base64.StdEncoding.EncodeToString(b))
}

func ParseOAuthResponseHeader(headerVal string) *RotatedOAuth {
	cleaned := strings.TrimSpace(headerVal)
	if !strings.HasPrefix(strings.ToLower(cleaned), "oauth ") {
		return nil
	}
	b64Part := strings.TrimSpace(cleaned[6:])
	decoded, err := base64.StdEncoding.DecodeString(b64Part)
	if err != nil {
		return nil
	}

	var root map[string]interface{}
	if err := json.Unmarshal(decoded, &root); err != nil {
		return nil
	}

	inner := root
	if dataMap, ok := root["data"].(map[string]interface{}); ok {
		inner = dataMap
	}

	getString := func(m map[string]interface{}, keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok && v != nil {
				switch val := v.(type) {
				case float64:
					return fmt.Sprintf("%.0f", val)
				case json.Number:
					return val.String()
				default:
					return fmt.Sprintf("%v", val)
				}
			}
		}
		return ""
	}

	acc := getString(inner, "accesstoken", "access_token")
	ref := getString(inner, "refreshtoken", "refresh_token")
	exp := getString(inner, "expiresin", "expires_in")
	if exp == "" {
		exp = "3600"
	}

	var lrd int64
	lrdStr := getString(inner, "lastrefreshdate", "last_refresh_date")
	if lrdStr == "" {
		lrdStr = getString(root, "responsetime", "response_time")
	}

	if lrdStr != "" {
		fmt.Sscanf(lrdStr, "%d", &lrd)
	}
	if lrd <= 0 {
		lrd = time.Now().UnixMilli()
	} else if lrd > 0 && lrd < 100000000000 {
		lrd = lrd * 1000
	}

	if acc != "" || ref != "" {
		return &RotatedOAuth{
			AccessToken:     acc,
			RefreshToken:    ref,
			ExpiresIn:       exp,
			LastRefreshDate: lrd,
		}
	}
	return nil
}

func SAPILoginOAuth(client *http.Client, sapiBaseURL, accessToken, refreshToken, deviceID, deviceName string, lastRefreshDate int64, expiresIn string) SAPILoginResult {
	authHeader := BuildOAuthHeader(accessToken, refreshToken, lastRefreshDate, expiresIn)
	return sapiLoginOAuthRequest(client, sapiBaseURL, authHeader, GetSAPIUserAgent(), accessToken, refreshToken, deviceID, deviceName)
}

// SAPILoginOAuthWithAuthorization permite probar la misma petición con una
// cabecera o User-Agent alternativos sin duplicar la lógica de diagnóstico.
func SAPILoginOAuthWithAuthorization(client *http.Client, sapiBaseURL, authorization, userAgent string) SAPILoginResult {
	return sapiLoginOAuthRequest(client, sapiBaseURL, authorization, userAgent, "", "", "", "")
}

func sapiLoginOAuthRequest(client *http.Client, sapiBaseURL, authorization, userAgent, previewSecretOne, previewSecretTwo, deviceID, deviceName string) SAPILoginResult {
	if client == nil {
		client = sapiLoginClient
	}

	endpoint := sapiBaseURL
	if endpoint == "" {
		endpoint = SAPIBaseURL
	}
	endpoint = strings.TrimSuffix(endpoint, "/")

	targetURL := fmt.Sprintf("%s/login/oauth?action=login&responsetime=true", endpoint)
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return SAPILoginResult{Error: err}
	}

	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if userAgent == "" {
		userAgent = GetSAPIUserAgent()
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	if IsDesktopProfile() {
		// Como el cliente oficial: sin Referer, X-Requested-With ni X-devicename.
		req.Header.Set("Accept-Language", DesktopAcceptLanguage)
		req.Header.Set("x-request-id", NewRequestID())
		if deviceID != "" {
			req.Header.Set("X-deviceid", SanitizeDeviceID(deviceID))
		}
	} else {
		if parsedEndpoint, parseErr := url.Parse(endpoint); parseErr == nil && parsedEndpoint.Host != "" {
			req.Header.Set("Referer", parsedEndpoint.Scheme+"://"+parsedEndpoint.Host+"/")
		}
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Accept-Language", "es-ES,es;q=0.9,en;q=0.8")
		if deviceID != "" {
			req.Header.Set("X-deviceid", SanitizeDeviceID(deviceID))
		}
		if deviceName != "" {
			req.Header.Set("X-devicename", deviceName)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return SAPILoginResult{Error: err}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return SAPILoginResult{StatusCode: resp.StatusCode, Error: err}
	}

	result := SAPILoginResult{
		StatusCode:          resp.StatusCode,
		RawResponse:         string(bodyBytes),
		RequestURL:          targetURL,
		Protocol:            resp.Proto,
		ContentType:         resp.Header.Get("Content-Type"),
		Server:              resp.Header.Get("Server"),
		XCache:              resp.Header.Get("X-Cache"),
		CloudFrontRequestID: resp.Header.Get("X-Amz-Cf-Id"),
		WWWAuthenticate:     resp.Header.Get("WWW-Authenticate"),
		BodyPreview:         summarizeSAPILoginBody(bodyBytes, previewSecretOne, previewSecretTwo),
	}

	// 1. Extraer cabecera Authorization (o WWW-Authenticate / Authentication-Info) rotada si existe
	for _, h := range []string{"Authorization", "WWW-Authenticate", "Authentication-Info"} {
		if authResp := resp.Header.Get(h); authResp != "" {
			if rot := ParseOAuthResponseHeader(authResp); rot != nil {
				result.RefreshedOAuth = rot
				break
			}
		}
	}

	// 2. Extraer cookies de sesión
	for _, cookie := range resp.Cookies() {
		result.ResponseCookieNames = append(result.ResponseCookieNames, cookie.Name)
		if strings.EqualFold(cookie.Name, "validationkey") {
			result.ValidationKey = cookie.Value
		} else if strings.EqualFold(cookie.Name, "JSESSIONID") {
			result.JSessionID = cookie.Value
		}
	}
	if result.StatusCode == http.StatusOK && client != nil && client.Jar != nil {
		for _, u := range []*url.URL{req.URL, {Scheme: req.URL.Scheme, Host: req.URL.Host, Path: "/"}} {
			if u == nil || u.Host == "" {
				continue
			}
			for _, cookie := range client.Jar.Cookies(u) {
				if result.ValidationKey == "" && strings.EqualFold(cookie.Name, "validationkey") {
					result.ValidationKey = cookie.Value
				} else if result.JSessionID == "" && strings.EqualFold(cookie.Name, "JSESSIONID") {
					result.JSessionID = cookie.Value
				}
			}
		}
	}

	// 3. Parsear JSON body
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &bodyMap); err == nil {
		if errVal, ok := bodyMap["error"]; ok && errVal != nil {
			result.Error = fmt.Errorf("SAPI login error: %v", errVal)
		}

		findKey := func(keys ...string) string {
			for _, k := range keys {
				if v, ok := bodyMap[k]; ok && v != nil {
					if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" && s != "<nil>" {
						return s
					}
				}
				if dataMap, ok := bodyMap["data"].(map[string]interface{}); ok {
					if v, ok := dataMap[k]; ok && v != nil {
						if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" && s != "<nil>" {
							return s
						}
					}
				}
			}
			return ""
		}

		if result.ValidationKey == "" {
			result.ValidationKey = findKey("validationkey", "validationKey", "ValidationKey")
		}
		if result.JSessionID == "" {
			result.JSessionID = findKey("jsessionid", "jsessionId", "JSessionID", "sessionId", "sessionid")
		}

		if rt := findKey("responsetime", "responseTime"); rt != "" {
			var respTime int64
			fmt.Sscanf(rt, "%d", &respTime)
			if respTime > 0 {
				if respTime < 100000000000 {
					respTime = respTime * 1000
				}
				result.ResponseTime = respTime
			}
		}

		if lrdStr := findKey("lastrefreshdate", "lastRefreshDate"); lrdStr != "" {
			var lrd int64
			fmt.Sscanf(lrdStr, "%d", &lrd)
			if lrd > 0 {
				if lrd < 100000000000 {
					lrd = lrd * 1000
				}
				result.LastRefreshDate = lrd
			}
		}
		if result.LastRefreshDate == 0 && result.ResponseTime > 0 {
			result.LastRefreshDate = result.ResponseTime
		}

		if result.RefreshedOAuth == nil {
			bodyAcc := findKey("accesstoken", "access_token")
			bodyRef := findKey("refreshtoken", "refresh_token")
			if bodyAcc != "" || bodyRef != "" {
				exp := findKey("expiresin", "expires_in")
				if exp == "" {
					exp = "3600"
				}
				result.RefreshedOAuth = &RotatedOAuth{
					AccessToken:     bodyAcc,
					RefreshToken:    bodyRef,
					ExpiresIn:       exp,
					LastRefreshDate: result.LastRefreshDate,
				}
			}
		}
	}

	if result.StatusCode != http.StatusOK {
		result.Error = errors.New(fmt.Sprintf("SAPI login returned HTTP %d", result.StatusCode))
	}

	return result
}
