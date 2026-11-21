package webclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"movistar-webdav-proxy/internal/logger"
)

// La sesión de la web (JSESSIONID) caduca a las 24 h del login aunque haya
// actividad. Si se entró con "No cerrar sesión", Movistar deja además una cookie
// PLC: con ella, al cargar la web sin JSESSIONID se reentra sola y se obtiene otra
// sesión de 24 h. Se renueva antes de la caducidad, con las demás llamadas en
// espera, porque la PLC rota en cada reentrada.
const (
	sessionCookie  = "JSESSIONID"
	rememberCookie = "PLC"

	defaultRenewBefore = time.Hour
	renewRetryAfter    = 10 * time.Minute
	renewWait          = 60 * time.Second // una máquina pequeña tarda más en cargar la web
	// renewMaxFails: tras estos fallos seguidos se avisa de que hay que volver a entrar.
	// La PLC no se borra nunca sola: un corte de internet o Movistar caído un rato no
	// la invalida, y se sigue reintentando cada renewRetryAfter. Si de verdad dejó de
	// valer, se entra desde /web-relogin/ (sin borrar nada) o /web-login/.
	renewMaxFails = 3
)

var errNoRemember = errors.New(`no hay cookie de "No cerrar sesión" (PLC): inicia sesión con e-mail y contraseña marcándola`)

// renewBefore es el margen antes de la caducidad (WEB_SESSION_RENEW_BEFORE, p. ej. "90m").
func renewBefore() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("WEB_SESSION_RENEW_BEFORE")); err == nil && d > 0 {
		return d
	}
	return defaultRenewBefore
}

// shouldRenew decide si toca renovar: hay PLC, se conoce la caducidad, falta
// menos del margen y no se ha intentado hace poco.
func shouldRenew(now, expires, lastTry time.Time, remember bool, before time.Duration) bool {
	if !remember || expires.IsZero() {
		return false
	}
	if !lastTry.IsZero() && now.Sub(lastTry) < renewRetryAfter {
		return false
	}
	return expires.Sub(now) < before
}

// renewDue decide si renovar ahora. Además de antes de la caducidad, en cuanto la
// sesión deja de valer (401) aunque la cookie JSESSIONID aún no haya caducado:
// Movistar puede invalidarla antes de tiempo en el servidor, y la PLC sirve igual
// para reentrar. Sin esto el proxy se quedaba en «no iniciada» hasta un login manual.
func renewDue(now, expires, lastTry time.Time, remember, loggedIn bool, before time.Duration) bool {
	if !loggedIn && remember {
		expires = now
	}
	return shouldRenew(now, expires, lastTry, remember, before)
}

// sessionCookies lee la caducidad de JSESSIONID y si existe la PLC.
func sessionCookies(tab context.Context) (expires time.Time, remember bool, err error) {
	res, err := chromedp.Call(tab, network.GetCookies, network.GetCookiesParams{URLs: []string{PortalURL + "/"}})
	if err != nil {
		return time.Time{}, false, err
	}
	for _, c := range res.Cookies {
		switch c.Name {
		case sessionCookie:
			if c.Expires > 0 {
				expires = time.Unix(int64(c.Expires), 0)
			}
		case rememberCookie:
			remember = true
		}
	}
	return expires, remember, nil
}

func (s *Session) refreshCookieState() (expires time.Time, remember bool, err error) {
	tab, err := s.tab()
	if err != nil {
		return time.Time{}, false, err
	}
	expires, remember, err = sessionCookies(tab)
	if err != nil {
		return
	}
	s.stateMu.Lock()
	s.expires, s.remember = expires, remember
	s.stateMu.Unlock()
	return
}

// renewLoop vigila la caducidad cada minuto y renueva a tiempo.
func (s *Session) renewLoop() {
	for {
		time.Sleep(time.Minute)
		expires, remember, err := s.refreshCookieState()
		if err != nil {
			continue
		}
		s.stateMu.RLock()
		lastTry := s.lastRenewTry
		s.stateMu.RUnlock()
		if !renewDue(time.Now(), expires, lastTry, remember, s.LoggedIn(), renewBefore()) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := s.Renew(ctx); err != nil {
			logger.Log("[WebSession] ❌ No se pudo renovar la sesión: %v", err)
		}
		cancel()
	}
}

// Renew obtiene una sesión nueva de 24 h con la PLC, sin iniciar sesión: borra
// JSESSIONID y recarga la web, que reentra sola. Mientras dura, ninguna otra
// llamada usa la pestaña ni las cookies.
func (s *Session) Renew(ctx context.Context) error {
	s.renewMu.Lock()
	defer s.renewMu.Unlock()
	s.stateMu.Lock()
	s.lastRenewTry = time.Now()
	s.stateMu.Unlock()

	tab, err := s.tab()
	if err != nil {
		return err
	}
	old, remember, err := sessionCookies(tab)
	if err != nil {
		return err
	}
	if !remember {
		return errNoRemember
	}
	logger.Log("[WebSession] 🔄 Renovando la sesión web (caduca %s)...", old.Local().Format("02/01 15:04"))
	if _, err := chromedp.Call(tab, network.DeleteCookies, network.DeleteCookiesParams{Name: sessionCookie, Domain: ".micloud.movistar.es", Path: "/"}); err != nil {
		return fmt.Errorf("borrar %s: %w", sessionCookie, err)
	}
	if err := chromedp.Do(tab, chromedp.Navigate(PortalURL+"/")); err != nil {
		return fmt.Errorf("recargar la web: %w", err)
	}

	deadline := time.Now().Add(renewWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		expires, _, err := sessionCookies(tab)
		if err != nil || !expires.After(old) {
			continue
		}
		r, err := s.eval(tab, "GET", "/sapi/profile/role?action=get", nil, "")
		if err != nil || r.Status != 200 {
			continue
		}
		s.stateMu.Lock()
		s.expires, s.lastRenewAt, s.renewFails = expires, time.Now(), 0
		s.stateMu.Unlock()
		s.setLoggedIn(true)
		logger.Log("[WebSession] ✔ Sesión web renovada sin iniciar sesión; caduca %s.", expires.Local().Format("02/01 15:04"))
		return nil
	}
	s.setLoggedIn(false)
	s.stateMu.Lock()
	s.renewFails++
	fails := s.renewFails
	s.stateMu.Unlock()
	if fails < renewMaxFails {
		return fmt.Errorf("la web no reentró con la PLC (intento %d); se reintenta en %s sin borrar nada", fails, renewRetryAfter)
	}
	return fmt.Errorf("la web no reentró con la PLC tras %d intentos seguidos; se sigue reintentando cada %s. Si la sesión dejó de valer, vuelve a entrar desde el panel en /web-relogin/", fails, renewRetryAfter)
}
