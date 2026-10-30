package auth

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	DesktopUserAgent  = "omh windows client 32.0.7.1"
	SAPIBaseURL       = "https://micloud.movistar.es/sapi"
)

func GetOAuthAuthorizeURL(provider string) string {
	if strings.EqualFold(provider, "o2") {
		return "https://apiseg.telefonica.es/openid/connect/auth/oauth/v2/o2/cus/authorize"
	}
	return OAuthAuthorizeURL
}

func GetOAuthTokenURL(provider string) string {
	if strings.EqualFold(provider, "o2") {
		return "https://apiseg.telefonica.es/openid/connect/auth/oauth/v2/o2/cus/token"
	}
	return OAuthTokenURL
}

func GetOAuthClientID(provider string) string {
	if strings.EqualFold(provider, "o2") {
		return "21ce2068-4966-45dc-8b7f-a4a25a3a462f"
	}
	return OAuthClientID
}

func GetOAuthClientSecret(provider string) string {
	if strings.EqualFold(provider, "o2") {
		return "aa892341-04b1-4fd5-be41-db60d4e900e7"
	}
	return OAuthClientSecret
}

func GetOAuthRedirectURI(provider string) string {
	if strings.EqualFold(provider, "o2") {
		return "https://cloud.o2online.es/ui/html/clientoauth.html"
	}
	return "https://micloud.movistar.es/ui/html/clientoauth.html"
}

func GetSAPIBaseURL(provider string) string {
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
	ExpiresIn       string `json:"expiresin"`
	LastRefreshDate int64  `json:"lastrefreshdate"`
	Platform        string `json:"platform"`
	RefreshToken    string `json:"refreshtoken"`
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
	StatusCode      int
	ValidationKey   string
	JSessionID      string
	RefreshedOAuth  *RotatedOAuth
	LastRefreshDate int64
	RawResponse     string
	Error           error
}

func GenerateDeviceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	seed := fmt.Sprintf("PC+%x", b)
	h := md5.Sum([]byte(seed))
	b64 := base64.StdEncoding.EncodeToString(h[:])
	return fmt.Sprintf("fol-%s", b64)
}

func GeneratePKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
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
	_, _ = rand.Read(stateB)
	_, _ = rand.Read(nonceB)
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
	if strings.Contains(cleaned, "code=") {
		if u, err := url.Parse(cleaned); err == nil && u.Query().Get("code") != "" {
			return u.Query().Get("code")
		}
		re := regexp.MustCompile(`code=([a-zA-Z0-9\-_]+)`)
		m := re.FindStringSubmatch(cleaned)
		if len(m) > 1 {
			return m[1]
		}
	}
	return cleaned
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
	req.Header.Set("User-Agent", DesktopUserAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OAuth token exchange failed (%d): %s", resp.StatusCode, string(bodyBytes))
	}

	var tokens OAuthTokens
	if err := json.Unmarshal(bodyBytes, &tokens); err != nil {
		return nil, err
	}
	return &tokens, nil
}

func RefreshOAuthToken(refreshToken, provider string) (*OAuthTokens, error) {
	tokenURL := GetOAuthTokenURL(provider)
	clientID := GetOAuthClientID(provider)
	clientSecret := GetOAuthClientSecret(provider)

	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}

	basicAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", clientID, clientSecret)))
	req.Header.Set("Authorization", "Basic "+basicAuth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("User-Agent", DesktopUserAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OAuth token refresh failed (%d): %s", resp.StatusCode, string(bodyBytes))
	}

	var tokens OAuthTokens
	if err := json.Unmarshal(bodyBytes, &tokens); err != nil {
		return nil, err
	}
	return &tokens, nil
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
				return fmt.Sprintf("%v", v)
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
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
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

	authHeader := BuildOAuthHeader(accessToken, refreshToken, lastRefreshDate, expiresIn)
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("User-Agent", DesktopUserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if deviceID != "" {
		req.Header.Set("X-deviceid", deviceID)
	}
	if deviceName != "" {
		req.Header.Set("X-devicename", deviceName)
	}

	resp, err := client.Do(req)
	if err != nil {
		return SAPILoginResult{Error: err}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return SAPILoginResult{StatusCode: resp.StatusCode, Error: err}
	}

	result := SAPILoginResult{
		StatusCode:  resp.StatusCode,
		RawResponse: string(bodyBytes),
	}

	// 1. Extraer cabecera Authorization rotada si existe
	if authResp := resp.Header.Get("Authorization"); authResp != "" {
		result.RefreshedOAuth = ParseOAuthResponseHeader(authResp)
	}

	// 2. Extraer cookies de sesión
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "validationkey" {
			result.ValidationKey = cookie.Value
		} else if cookie.Name == "JSESSIONID" {
			result.JSessionID = cookie.Value
		}
	}

	// 3. Parsear JSON body
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &bodyMap); err == nil {
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

		if rt := findKey("responsetime", "responseTime", "lastrefreshdate", "lastRefreshDate"); rt != "" {
			var lrd int64
			fmt.Sscanf(rt, "%d", &lrd)
			if lrd > 0 {
				if lrd < 100000000000 {
					lrd = lrd * 1000
				}
				result.LastRefreshDate = lrd
			}
		}
	}

	if result.StatusCode != http.StatusOK {
		result.Error = errors.New(fmt.Sprintf("SAPI login returned HTTP %d", result.StatusCode))
	}

	return result
}
