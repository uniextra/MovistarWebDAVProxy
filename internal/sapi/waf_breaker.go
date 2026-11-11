package sapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/logger"
)

// Freno ante el bloqueo del cortafuegos (AWS WAF / CloudFront) de O2 Cloud.
//
// Cuando CloudFront empieza a responder "403 ERROR - The request could not be
// satisfied", el bloqueo es por IP: afecta a todas las peticiones, a todas las cuentas
// que salgan por esa IP, y cada petición que se siga enviando mientras dura cuenta para
// el WAF y lo alarga. Visto en producción (11/11/2026): tras el primer 403, un rclone
// con --retries siguió provocando ~1.500 peticiones fallidas en 15 minutos (MKCOL ->
// crear carpeta -> 403, PUT -> 409) hasta que se paró a mano.
//
// Por eso, al primer 403 del WAF el cliente deja de llamar a O2 durante wafCooldownBase
// (que se duplica en cada bloqueo seguido, hasta wafCooldownMax) y devuelve ErrWAFBlocked
// sin tocar la red. Pasado ese tiempo, la siguiente petición hace de sonda: si funciona,
// se vuelve a la normalidad; si no, se espera el doble.

// ErrWAFBlocked indica que el cliente está en pausa por un bloqueo del WAF de O2.
var ErrWAFBlocked = errors.New("O2 Cloud: cortafuegos (WAF) bloqueando; llamadas a O2 en pausa")

var (
	wafCooldownBase = 10 * time.Minute
	wafCooldownMax  = 2 * time.Hour
)

type wafBreaker struct {
	mu      sync.Mutex
	until   time.Time
	strikes int
	now     func() time.Time
}

func (b *wafBreaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// blockedUntil devuelve hasta cuándo dura la pausa (cero si no hay pausa).
func (b *wafBreaker) blockedUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.clock().Before(b.until) {
		return b.until
	}
	return time.Time{}
}

// trip registra un 403 del WAF y devuelve hasta cuándo queda en pausa.
func (b *wafBreaker) trip() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	if now.Before(b.until) {
		// Respuestas de peticiones que ya estaban en vuelo: no alargan la pausa.
		return b.until
	}
	b.strikes++
	d := wafCooldownBase
	for i := 1; i < b.strikes && d < wafCooldownMax; i++ {
		d *= 2
	}
	if d > wafCooldownMax {
		d = wafCooldownMax
	}
	b.until = now.Add(d)
	logger.Log("[WAF] 🛑 O2 (CloudFront) ha respondido 403: el cortafuegos bloquea esta IP. Pauso TODAS las llamadas a O2 durante %s (hasta las %s, bloqueo %d seguido). Reintentar antes solo alarga el bloqueo.",
		d, b.until.Local().Format("15:04"), b.strikes)
	return b.until
}

// ok registra una respuesta que no es del WAF: el bloqueo ha terminado.
func (b *wafBreaker) ok() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.strikes > 0 {
		logger.Log("[WAF] ✅ O2 vuelve a responder con normalidad.")
	}
	b.strikes = 0
}

// isWAFBlockResponse reconoce la página de error de CloudFront ("The request could not
// be satisfied"), distinta de un 403 propio de SAPI (JSON con código de error). Lee como
// mucho 8 KB del cuerpo y lo restituye para que el llamante lo pueda seguir leyendo.
func isWAFBlockResponse(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		return false
	}
	cloudfront := strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudfront") ||
		strings.Contains(strings.ToLower(resp.Header.Get("X-Cache")), "cloudfront")
	ctype := strings.ToLower(resp.Header.Get("Content-Type"))
	if resp.Body == nil {
		return cloudfront && !strings.Contains(ctype, "json")
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	if bytes.Contains(head, []byte("could not be satisfied")) {
		return true
	}
	return cloudfront && strings.Contains(ctype, "html")
}

// WAFBlockedUntil devuelve hasta cuándo están en pausa las llamadas a O2 por un bloqueo
// del WAF, o el tiempo cero si no hay bloqueo.
func (c *Client) WAFBlockedUntil() time.Time {
	return c.waf.blockedUntil()
}

// ClearWAFPause termina la pausa en curso (sin olvidar los bloqueos seguidos): la
// siguiente petición sale a O2 y hace de sonda. Pensado para pruebas.
func (c *Client) ClearWAFPause() {
	c.waf.mu.Lock()
	c.waf.until = time.Time{}
	c.waf.mu.Unlock()
}

func wafBlockedError(until time.Time, method, p string) error {
	return fmt.Errorf("%w (hasta las %s; no se envía %s %s)", ErrWAFBlocked, until.Local().Format("15:04"), method, p)
}
