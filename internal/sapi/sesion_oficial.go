package sapi

import (
	"net/http"
	"strings"
	"time"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/logger"
)

// La app oficial nunca da la sesión por perdida por un login fallido: ante un 401
// vuelve a hacer login/oauth en la siguiente pasada, y solo pide volver a entrar
// cuando O2 rechaza de verdad las credenciales. El proxy, en cambio, marcaba la
// sesión como caducada al primer login fallido y dejaba de intentarlo hasta que se
// revinculaba la cuenta (issue #60: un fallo a las 09:16 y 401 en todo hasta la tarde).
//
// Ahora, en el perfil desktop, un login fallido se trata como transitorio y se
// reintenta con espera creciente; solo se da la sesión por caducada si sigue
// fallando durante desktopLoginGiveUpAfter y al menos desktopLoginGiveUpTries veces.
// Los fallos que no dicen nada de las credenciales (sin respuesta, 5xx, 429, el 403
// del cortafuegos o el 401 vacío del borde de CloudFront) nunca la dan por caducada.
var (
	desktopLoginBackoff     = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}
	desktopLoginGiveUpTries = 4
	desktopLoginGiveUpAfter = 30 * time.Minute
)

// desktopLoginFailed registra un login/oauth fallido. Devuelve true si se trata como
// transitorio (la sesión se conserva y se reintentará); false si hay que darla por
// caducada.
func (c *Client) desktopLoginFailed(res *auth.SAPILoginResult) bool {
	now := time.Now()
	definitive := res != nil && isCredentialRejection(res)

	c.mu.Lock()
	if c.loginFailures == 0 {
		c.loginFailingSince = now
	}
	c.loginFailures++
	n := c.loginFailures
	since := c.loginFailingSince
	wait := desktopLoginBackoff[len(desktopLoginBackoff)-1]
	if n-1 < len(desktopLoginBackoff) {
		wait = desktopLoginBackoff[n-1]
	}
	giveUp := definitive && n >= desktopLoginGiveUpTries && now.Sub(since) >= desktopLoginGiveUpAfter
	if !giveUp {
		c.loginRetryAt = now.Add(wait)
		c.refreshTransient = true
	}
	c.mu.Unlock()

	if giveUp {
		logger.Log("[!] ⚠️ O2 rechaza el login desde hace %s (%d intentos).", now.Sub(since).Round(time.Minute), n)
		return false
	}
	logger.Log("[OAuth] ⏳ Login en SAPI fallido (%s); se conserva la sesión y se reintenta en %s (fallo %d).", describeLoginFailure(res), wait, n)
	return true
}

// isCredentialRejection: SAPI (no el borde de CloudFront) responde 401/403 con cuerpo.
func isCredentialRejection(res *auth.SAPILoginResult) bool {
	if res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusForbidden {
		return false
	}
	edge := strings.Contains(strings.ToLower(res.Server), "cloudfront") || strings.Contains(strings.ToLower(res.XCache), "cloudfront")
	if res.StatusCode == http.StatusForbidden && edge {
		return false // cortafuegos
	}
	if edge && strings.TrimSpace(res.BodyPreview) == "" && strings.TrimSpace(res.RawResponse) == "" {
		return false // 401 vacío del borde (visto tras ~11 h con otra huella)
	}
	return true
}

func describeLoginFailure(res *auth.SAPILoginResult) string {
	if res == nil {
		return "sin login"
	}
	if res.StatusCode == 0 {
		if res.Error != nil {
			return "sin respuesta: " + res.Error.Error()
		}
		return "sin respuesta"
	}
	d := "HTTP " + http.StatusText(res.StatusCode)
	if res.Server != "" {
		d += ", " + res.Server
	}
	return d
}
