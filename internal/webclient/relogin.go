package webclient

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"movistar-webdav-proxy/internal/logger"
)

// Volver a iniciar sesión sin cerrar la actual: el inicio de sesión se hace en una
// pestaña de un contexto aislado del mismo Chromium (sus propias cookies), así la
// sesión en uso sigue sirviendo mientras tanto y no se cierra en Movistar. Cuando
// la pestaña aislada tiene sesión con «No cerrar sesión» (cookie PLC), sus cookies
// pasan a la pestaña principal, que recarga la web con ellas.

type relogin struct {
	mu      sync.Mutex
	tab     context.Context
	cancel  context.CancelFunc
	adopted time.Time // cuándo se adoptó la última sesión nueva
}

// ReloginView devuelve la vista remota del inicio de sesión aislado, para el panel.
func (s *Session) ReloginView() *LoginView {
	return &LoginView{tab: s.reloginTab, LoggedIn: s.reloginDone, alwaysLogin: true}
}

// reloginDone indica si ya se adoptó una sesión nueva en los últimos 10 minutos (para la página).
func (s *Session) reloginDone() bool {
	s.re.mu.Lock()
	defer s.re.mu.Unlock()
	return !s.re.adopted.IsZero() && time.Since(s.re.adopted) < 10*time.Minute
}

// reloginTab abre (o devuelve) la pestaña aislada con la página de inicio de sesión.
func (s *Session) reloginTab() (context.Context, error) {
	s.re.mu.Lock()
	defer s.re.mu.Unlock()
	if s.re.tab != nil && s.re.tab.Err() == nil {
		return s.re.tab, nil
	}
	if !s.re.adopted.IsZero() && time.Since(s.re.adopted) < 10*time.Minute {
		// Recién adoptada: la página sigue pidiendo la pantalla; se le enseña la
		// principal en vez de abrir otro inicio de sesión aparte.
		return s.tab()
	}
	main, err := s.tab()
	if err != nil {
		return nil, err
	}
	tab, cancel := chromedp.NewContext(main, chromedp.WithNewBrowserContext())
	// La pestaña se crea con el contexto propio, sin plazo: si la primera acción va con
	// un contexto con plazo, chromedp ata la pestaña a él y la cierra al vencer.
	if err := chromedp.Do(tab); err != nil {
		cancel()
		return nil, err
	}
	ctx, c2 := context.WithTimeout(tab, 60*time.Second)
	defer c2()
	if err := chromedp.Do(ctx, chromedp.Navigate(PortalURL+"/login")); err != nil {
		cancel()
		return nil, err
	}
	s.re.tab, s.re.cancel, s.re.adopted = tab, cancel, time.Time{}
	logger.Log("[WebSession] 🔑 Abierto un inicio de sesión aparte (la sesión actual sigue en uso)")
	go s.watchRelogin(tab)
	return tab, nil
}

// watchRelogin espera a que la pestaña aislada tenga sesión con PLC y la adopta.
// Se cierra sola a los 30 minutos si no se completa.
func (s *Session) watchRelogin(tab context.Context) {
	deadline := time.Now().Add(30 * time.Minute)
	fails := 0
	for time.Now().Before(deadline) && tab.Err() == nil {
		time.Sleep(3 * time.Second)
		_, remember, err := sessionCookies(tab)
		if err != nil || !remember {
			continue
		}
		if err := s.adoptRelogin(tab); err != nil {
			// Cada intento recarga la web principal: no se insiste indefinidamente.
			if fails++; fails >= 3 {
				logger.Log("[WebSession] ❌ No se pudo pasar la sesión nueva a la principal tras %d intentos: %v. Vuelve a abrir /web-relogin/.", fails, err)
				break
			}
			logger.Log("[WebSession] ⚠️ No se pudo pasar la sesión nueva a la principal: %v; se reintenta", err)
			time.Sleep(10 * time.Second)
			continue
		}
		break
	}
	s.re.mu.Lock()
	if s.re.tab == tab {
		s.re.cancel()
		s.re.tab, s.re.cancel = nil, nil
	}
	s.re.mu.Unlock()
}

// adoptRelogin copia las cookies de Movistar de la pestaña aislada a la principal y
// recarga la web con ellas. Mientras dura no sale ninguna otra llamada.
func (s *Session) adoptRelogin(from context.Context) error {
	res, err := chromedp.Call(from, network.GetCookies, network.GetCookiesParams{URLs: []string{PortalURL + "/", "https://upload.micloud.movistar.es/"}})
	if err != nil {
		return err
	}
	var params []*network.CookieParam
	for _, c := range res.Cookies {
		p := &network.CookieParam{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite}
		if !c.Session && c.Expires > 0 {
			p.Expires = cdp.TimeSinceEpoch(c.Expires)
		}
		params = append(params, p)
	}
	if len(params) == 0 {
		return errors.New("la pestaña aislada no tiene cookies")
	}

	s.renewMu.Lock()
	defer s.renewMu.Unlock()
	main, err := s.tab()
	if err != nil {
		return err
	}
	if _, err := chromedp.Call(main, network.SetCookies, network.SetCookiesParams{Cookies: params}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(main, renewWait)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Navigate(PortalURL+"/")); err != nil {
		return err
	}
	for ctx.Err() == nil {
		r, err := s.eval(main, "GET", "/sapi/profile/role?action=get", nil, "")
		if err == nil && r.Status == 200 {
			expires, remember, _ := sessionCookies(main)
			s.stateMu.Lock()
			s.expires, s.remember, s.renewFails = expires, remember, 0
			s.stateMu.Unlock()
			s.setLoggedIn(true)
			s.re.mu.Lock()
			s.re.adopted = time.Now()
			s.re.mu.Unlock()
			logger.Log("[WebSession] ✔ Sesión nueva en uso (No cerrar sesión: %v); caduca %s.", remember, expires.Local().Format("02/01 15:04"))
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	return errors.New("la web no respondió con la sesión nueva")
}
