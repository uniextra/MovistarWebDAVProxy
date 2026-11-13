package webclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"movistar-webdav-proxy/internal/logger"
)

// PortalURL es el origen de la web de Movistar Cloud; las llamadas a SAPI se
// ejecutan desde una pestaña abierta en él, igual que las hace la propia web.
const PortalURL = "https://micloud.movistar.es"

// sapiJS replica cómo la web llama a SAPI: cookies de sesión, validationkey en
// la URL y cabecera X-deviceid "web-<huella>". El cuerpo viaja como texto.
const sapiJS = `window.__sapi = async (method, path, body, ctype, deviceId) => {
  const m = document.cookie.match(/validationKey=([^;]+)/);
  const vk = m ? decodeURIComponent(m[1]) : '';
  const sep = path.includes('?') ? '&' : '?';
  const opts = {method, credentials: 'include', headers: {'Accept': 'application/json', 'X-deviceid': 'web-' + deviceId}};
  if (body !== null) { opts.headers['Content-Type'] = ctype || 'application/json;charset=UTF-8'; opts.body = body; }
  const r = await fetch(path + (vk ? sep + 'validationkey=' + encodeURIComponent(vk) : ''), opts);
  return {status: r.status, ctype: r.headers.get('content-type') || '', body: await r.text()};
}; true`

// ErrNotLoggedIn indica que la sesión de la web no está iniciada o ha caducado.
var ErrNotLoggedIn = errors.New("sesión web de Movistar Cloud no iniciada o caducada")

// Options configura el Chromium de la sesión.
type Options struct {
	ProfileDir string // perfil persistente (cookies de la sesión)
	ChromePath string // vacío = autodetectar
	NoSandbox  bool   // necesario en contenedores
}

// Session mantiene un Chromium sin pantalla con la sesión de la web iniciada.
type Session struct {
	opts     Options
	deviceID string

	mu          sync.Mutex // protege allocCancel/ctx/cancel y el relanzamiento
	allocCancel context.CancelFunc
	ctx         context.Context
	cancel      context.CancelFunc
	userAgent   string

	stateMu    sync.RWMutex
	loggedIn   bool
	checkedAt  time.Time
	loginSince time.Time // desde cuándo está activa la sesión
	expires    time.Time // caducidad de JSESSIONID (24 h desde el login, la actividad no la alarga)
	remember   bool      // hay cookie PLC ("No cerrar sesión")

	calls chan struct{} // concurrencia de llamadas a SAPI (la web usa 2)

	// renewMu: las llamadas desde la pestaña y la lectura de credenciales lo toman
	// en lectura; la renovación, en escritura. La PLC rota en cada reentrada y si
	// dos peticiones sin JSESSIONID la usan a la vez Movistar la invalida.
	renewMu      sync.RWMutex
	lastRenewAt  time.Time
	lastRenewTry time.Time
}

// Response es la respuesta de una llamada a SAPI hecha desde la pestaña.
type Response struct {
	Status      int    `json:"status"`
	ContentType string `json:"ctype"`
	Body        string `json:"body"`
}

// NewSession arranca Chromium con el perfil indicado y abre la web.
func NewSession(opts Options) (*Session, error) {
	if err := os.MkdirAll(opts.ProfileDir, 0o700); err != nil {
		return nil, err
	}
	s := &Session{opts: opts, deviceID: loadDeviceID(opts.ProfileDir), calls: make(chan struct{}, 2)}
	if err := s.start(); err != nil {
		return nil, err
	}
	go s.watchLogin()
	go s.renewLoop()
	return s, nil
}

// watchLogin comprueba la sesión cada pocos segundos mientras no esté iniciada,
// para detectar enseguida que el usuario ha entrado desde la vista remota.
func (s *Session) watchLogin() {
	for {
		time.Sleep(5 * time.Second)
		if s.LoggedIn() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s.CheckLogin(ctx)
		cancel()
	}
}

func (s *Session) start() error {
	chromeDir := filepath.Join(s.opts.ProfileDir, "chrome")
	// Chromium ata SingletonLock/Cookie/Socket al hostname; en Docker cambia en cada
	// contenedor y el perfil quedaría "en uso por otro ordenador". Hay una sola
	// instancia por perfil, así que se limpian al arrancar.
	if stale, _ := filepath.Glob(filepath.Join(chromeDir, "Singleton*")); len(stale) > 0 {
		for _, f := range stale {
			_ = os.Remove(f)
		}
	}

	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.UserDataDir(chromeDir),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(1280, 900),
	}
	if goruntime.GOOS == "linux" {
		// Misma clave de cifrado de cookies en todos los arranques (sin llavero en el servidor).
		opts = append(opts, chromedp.Flag("password-store", "basic"))
	}
	if s.opts.NoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	if s.opts.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(s.opts.ChromePath))
	}

	actx, acancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(actx)
	if err := chromedp.Do(ctx, chromedp.Navigate(PortalURL+"/")); err != nil {
		cancel()
		acancel()
		return fmt.Errorf("arrancar Chromium: %w", err)
	}
	ua, _ := chromedp.Run(ctx, chromedp.Evaluate[string]("navigator.userAgent"))

	s.mu.Lock()
	s.allocCancel, s.ctx, s.cancel, s.userAgent = acancel, ctx, cancel, ua
	s.mu.Unlock()
	logger.Log("[WebSession] 🌐 Chromium iniciado (%s)", ua)
	return nil
}

// tab devuelve el contexto de la pestaña, relanzando Chromium si ha muerto.
func (s *Session) tab() (context.Context, error) {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	if ctx != nil && ctx.Err() == nil {
		return ctx, nil
	}
	logger.Log("[WebSession] ⚠️ Chromium no responde; relanzando...")
	s.Close()
	if err := s.start(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx, nil
}

// Do ejecuta una llamada a SAPI desde la pestaña de la web. path es la ruta
// (p. ej. "/sapi/media?action=get"); body es nil para peticiones sin cuerpo.
func (s *Session) Do(ctx context.Context, method, path string, body []byte, contentType string) (*Response, error) {
	select {
	case s.calls <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.calls }()
	s.renewMu.RLock()
	defer s.renewMu.RUnlock()

	tab, err := s.tab()
	if err != nil {
		return nil, err
	}
	resp, err := s.eval(tab, method, path, body, contentType)
	if err != nil {
		// La pestaña puede haber salido de micloud (p. ej. redirigida al login):
		// se vuelve al portal y se reintenta una vez.
		if nerr := chromedp.Do(tab, chromedp.Navigate(PortalURL+"/")); nerr != nil {
			return nil, err
		}
		resp, err = s.eval(tab, method, path, body, contentType)
		if err != nil {
			return nil, err
		}
	}
	if resp.Status == 401 {
		s.setLoggedIn(false)
	} else if resp.Status < 400 {
		s.setLoggedIn(true)
	}
	return resp, nil
}

func (s *Session) eval(tab context.Context, method, path string, body []byte, contentType string) (*Response, error) {
	if _, err := chromedp.Run(tab, chromedp.Evaluate[bool](sapiJS)); err != nil {
		return nil, err
	}
	bodyJS := "null"
	if body != nil {
		b, _ := json.Marshal(string(body))
		bodyJS = string(b)
	}
	expr := fmt.Sprintf("window.__sapi(%q, %q, %s, %q, %q)", method, path, bodyJS, contentType, s.deviceID)
	r, err := chromedp.Run(tab, chromedp.Evaluate[Response](expr, chromedp.EvalAwaitPromise))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Credentials devuelve lo necesario para las subidas directas a upload.*: la
// cabecera Cookie de la sesión y la validationKey.
func (s *Session) Credentials(uploadURL string) (cookieHeader, validationKey string, err error) {
	s.renewMu.RLock()
	defer s.renewMu.RUnlock()
	tab, err := s.tab()
	if err != nil {
		return "", "", err
	}
	res, err := chromedp.Call(tab, network.GetCookies, network.GetCookiesParams{URLs: []string{PortalURL + "/", uploadURL + "/"}})
	if err != nil {
		return "", "", err
	}
	var parts []string
	for _, c := range res.Cookies {
		if c.Name == rememberCookie {
			// Solo la usa la renovación: una subida sin JSESSIONID válida que la
			// llevara reentraría por su cuenta y la gastaría.
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
		if c.Name == "validationKey" {
			validationKey, _ = url.QueryUnescape(c.Value)
		}
	}
	return strings.Join(parts, "; "), validationKey, nil
}

// CheckLogin consulta a SAPI si la sesión sigue iniciada.
func (s *Session) CheckLogin(ctx context.Context) bool {
	r, err := s.Do(ctx, "GET", "/sapi/profile/role?action=get", nil, "")
	ok := err == nil && r.Status == 200
	s.setLoggedIn(ok)
	return ok
}

// LoggedIn devuelve el último estado conocido de la sesión.
func (s *Session) LoggedIn() bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.loggedIn
}

func (s *Session) setLoggedIn(v bool) {
	s.stateMu.Lock()
	if v && (!s.loggedIn || s.loginSince.IsZero()) {
		s.loginSince = time.Now()
	}
	if s.loggedIn != v {
		if v {
			logger.Log("[WebSession] ✔ Sesión web de Movistar Cloud activa.")
		} else {
			logger.Log("[WebSession] ⚠️ Sesión web caducada o no iniciada. Inicia sesión desde el panel.")
		}
	}
	s.loggedIn = v
	s.checkedAt = time.Now()
	s.stateMu.Unlock()
}

// Status resume el estado de la sesión para el panel.
func (s *Session) Status() map[string]interface{} {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	out := map[string]interface{}{"logged_in": s.loggedIn}
	if !s.checkedAt.IsZero() {
		out["checked_at"] = s.checkedAt.Format(time.RFC3339)
	}
	if s.loggedIn && !s.loginSince.IsZero() {
		out["since"] = s.loginSince.Format(time.RFC3339)
	}
	if s.loggedIn && !s.expires.IsZero() {
		out["expires"] = s.expires.Format(time.RFC3339)
	}
	out["remember"] = s.remember
	if !s.lastRenewAt.IsZero() {
		out["renewed_at"] = s.lastRenewAt.Format(time.RFC3339)
	}
	return out
}

// DeviceID es la huella que se envía en X-deviceid ("web-" + DeviceID).
func (s *Session) DeviceID() string { return s.deviceID }

// UserAgent es el del Chromium de la sesión, para las peticiones directas.
func (s *Session) UserAgent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userAgent
}

// LoginView devuelve la vista remota sobre la pestaña actual, para el panel.
func (s *Session) LoginView() *LoginView {
	return &LoginView{tab: s.tab, LoggedIn: s.LoggedIn}
}

// Close cierra Chromium de forma ordenada para que vuelque las cookies a disco.
func (s *Session) Close() {
	s.mu.Lock()
	ctx, cancel, acancel := s.ctx, s.cancel, s.allocCancel
	s.ctx, s.cancel, s.allocCancel = nil, nil, nil
	s.mu.Unlock()
	if ctx != nil {
		tctx, tcancel := context.WithTimeout(ctx, 20*time.Second)
		_ = chromedp.Cancel(tctx)
		tcancel()
	}
	if cancel != nil {
		cancel()
	}
	if acancel != nil {
		acancel()
	}
}

func loadDeviceID(dir string) string {
	p := filepath.Join(dir, "device-id")
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) == 32 {
		return string(bytes.TrimSpace(b))
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	id := hex.EncodeToString(buf)
	_ = os.WriteFile(p, []byte(id), 0o600)
	return id
}
