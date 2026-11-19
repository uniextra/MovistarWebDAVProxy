package sapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
)

func newDesktopTestClient(t *testing.T, srvURL string, lastRefresh int64) *Client {
	t.Helper()
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("SAPI_USER_AGENT", "")
	t.Setenv("DESKTOP_USER_AGENT", "")
	t.Setenv("OAUTH_REFRESH_FALLBACK", "")
	t.Setenv("TOKENS_DIR", t.TempDir())
	client, err := NewClient(config.AccountConfig{
		Provider:        "o2",
		Phone:           "612345678",
		AccessToken:     "access-1",
		RefreshToken:    "refresh-1",
		ValidationKey:   "vk-123",
		JSessionID:      "jsess-456",
		DeviceID:        "fol-lIK4tXvlwgUheGsxRRaJGA==",
		DeviceName:      "PC Windows",
		LastRefreshDate: lastRefresh,
		ExpiresIn:       "3600",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.SetBaseURLs(srvURL+"/sapi", srvURL+"/upload")
	return client
}

func decodeOAuthHeader(t *testing.T, h string) map[string]interface{} {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "oauth "))
	if err != nil {
		t.Fatalf("Authorization %q is not oauth base64: %v", h, err)
	}
	var root struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("Authorization payload: %v", err)
	}
	return root.Data
}

// Las cabeceras deben ser las del cliente oficial de Windows (captura real).
func TestDesktopProfile_RequestHeadersMatchOfficialClient(t *testing.T) {
	var got http.Header
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	client := newDesktopTestClient(t, srv.URL, time.Now().UnixMilli())
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sapi/profile/role?action=get", nil)
	resp, err := client.DoRequest(req)
	if err != nil {
		t.Fatalf("DoRequest: %v", err)
	}
	resp.Body.Close()

	want := map[string]string{
		"User-Agent":      "omh windows client 4.0.1",
		"X-Deviceid":      "fol-lIK4tXvlwgUheGsxRRaJGA==",
		"Accept":          "*/*",
		"Accept-Language": "es-ES,en,*",
		"Cookie":          "JSESSIONID=jsess-456",
	}
	for k, v := range want {
		if g := got.Get(k); g != v {
			t.Errorf("%s = %q, want %q", k, g, v)
		}
	}
	for _, absent := range []string{"Referer", "X-Requested-With", "X-Devicename"} {
		if g := got.Get(absent); g != "" {
			t.Errorf("%s = %q, want absent (the official client does not send it)", absent, g)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(got.Get("X-Request-Id")) {
		t.Errorf("x-request-id = %q, want a UUID", got.Get("X-Request-Id"))
	}
	if !strings.Contains(gotURL, "validationkey=vk-123") {
		t.Errorf("URL = %q, want validationkey parameter", gotURL)
	}
	if d := decodeOAuthHeader(t, got.Get("Authorization")); d["platform"] != "windows" {
		t.Errorf("Authorization platform = %v, want windows", d["platform"])
	}
}

// Renovación como el cliente oficial: con lastrefreshdate + expiresin vencidos la
// petición se envía igual; SAPI responde 200 con una cabecera Authorization (los
// mismos tokens, sin lastrefreshdate) y el cliente pone lastrefreshdate a "ahora".
// La siguiente petición ya lleva la fecha nueva.
func TestDesktopProfile_RenewsFromSAPIAuthorizationHeader(t *testing.T) {
	expired := time.Now().Add(-62 * time.Minute).UnixMilli()
	var calls int32
	var secondLRD float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		d := decodeOAuthHeader(t, r.Header.Get("Authorization"))
		lrd, _ := d["lastrefreshdate"].(float64)
		if n == 1 {
			if int64(lrd) != expired {
				t.Errorf("first request lastrefreshdate = %v, want the expired %d", lrd, expired)
			}
			payload, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{
				"accesstoken": "access-1", "tokentype": "Bearer", "expiresin": "3600",
				"refreshtoken": "refresh-1", "scope": "openid", "id_token": "x.y.z",
			}})
			w.Header().Set("Authorization", "oauth "+base64.StdEncoding.EncodeToString(payload))
		} else {
			secondLRD = lrd
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	client := newDesktopTestClient(t, srv.URL, expired)
	before := time.Now().UnixMilli()
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sapi/subscription/plan?action=get", nil)
		resp, err := client.DoRequest(req)
		if err != nil {
			t.Fatalf("DoRequest %d: %v", i+1, err)
		}
		resp.Body.Close()
	}

	cfg := client.GetConfig()
	if cfg.LastRefreshDate < before {
		t.Errorf("LastRefreshDate = %d, want >= %d (time of the SAPI response)", cfg.LastRefreshDate, before)
	}
	if int64(secondLRD) < before {
		t.Errorf("second request lastrefreshdate = %v, want the renewed value", secondLRD)
	}
	if cfg.AccessToken != "access-1" || cfg.RefreshToken != "refresh-1" {
		t.Errorf("tokens changed unexpectedly: %q / %q", cfg.AccessToken, cfg.RefreshToken)
	}
	if client.IsSessionExpired() {
		t.Errorf("session marked expired after a normal renewal")
	}
}

// Sin cabecera Authorization en la respuesta, el perfil "desktop" no "desliza"
// lastrefreshdate: conserva la fecha real, como el cliente oficial.
func TestDesktopProfile_DoesNotSlideLastRefreshDate(t *testing.T) {
	old := time.Now().Add(-40 * time.Minute).UnixMilli()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	client := newDesktopTestClient(t, srv.URL, old)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sapi/profile/role?action=get", nil)
	resp, err := client.DoRequest(req)
	if err != nil {
		t.Fatalf("DoRequest: %v", err)
	}
	resp.Body.Close()
	if got := client.GetConfig().LastRefreshDate; got != old {
		t.Errorf("LastRefreshDate = %d, want unchanged %d", got, old)
	}
}

// Si SAPI rechaza la sesión, el perfil "desktop" no canjea el refresh_token en
// apiseg (el cliente oficial nunca lo hace). Un login fallido no la da por perdida
// al momento (issue #60): se conserva, se espera antes de volver a intentarlo y
// solo se marca para revincular si O2 la sigue rechazando durante un buen rato.
func TestDesktopProfile_NoApisegRefreshOn401(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logins.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"AUTH-1000"}}`))
	}))
	defer srv.Close()

	// Fuera de la ventana de 30 s en la que se omite cualquier renovación.
	client := newDesktopTestClient(t, srv.URL, time.Now().Add(-5*time.Minute).UnixMilli())
	start := time.Now()
	ok, _ := client.refreshSessionInternal(false)
	if ok {
		t.Fatalf("refreshSessionInternal succeeded against a server that only answers 401")
	}
	if client.IsSessionExpired() || !client.RefreshTransient() {
		t.Fatalf("un primer login fallido debe ser transitorio (expirada=%v transitorio=%v)", client.IsSessionExpired(), client.RefreshTransient())
	}
	// El canje en apiseg reintenta con esperas de segundos; aquí no debe ocurrir.
	if time.Since(start) > 3*time.Second {
		t.Errorf("refresh took %v; it looks like it tried apiseg", time.Since(start))
	}

	// Durante la espera no se repite el login (ráfagas de logins = bloqueo del WAF).
	before := logins.Load()
	if ok, _ := client.refreshSessionInternal(false); ok || logins.Load() != before {
		t.Fatalf("se repitió el login durante la espera (%d → %d)", before, logins.Load())
	}

	// Si O2 sigue rechazándolo durante el tiempo y los intentos fijados, se da por caducada.
	t.Setenv("SAPI_LOGIN_GIVEUP_REJECTIONS", "2")
	t.Setenv("SAPI_LOGIN_GIVEUP_SEC", "0.000001")
	client.mu.Lock()
	client.loginRetryAt = time.Time{}
	client.mu.Unlock()
	client.refreshSessionInternal(false)
	if !client.IsSessionExpired() {
		t.Errorf("tras rechazos repetidos la sesión debe marcarse para revincular")
	}
}

// El 401 vacío del borde de CloudFront no dice nada de las credenciales: nunca
// marca la sesión como caducada, por muchas veces que ocurra.
func TestDesktopProfile_CloudFrontEmpty401NeverExpires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CloudFront")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("SAPI_LOGIN_GIVEUP_REJECTIONS", "1")
	t.Setenv("SAPI_LOGIN_GIVEUP_SEC", "0.000001")

	client := newDesktopTestClient(t, srv.URL, time.Now().Add(-5*time.Minute).UnixMilli())
	for i := 0; i < 5; i++ {
		client.mu.Lock()
		client.loginRetryAt = time.Time{}
		client.mu.Unlock()
		client.refreshSessionInternal(false)
	}
	if client.IsSessionExpired() {
		t.Fatal("un 401 vacío de CloudFront no debe dar la sesión por caducada")
	}
}
