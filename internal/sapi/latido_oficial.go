package sapi

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"movistar-webdav-proxy/internal/logger"
)

// Latido del cliente oficial de Windows (capturas del 17 y 18/11/2026, 1,5 h de
// tráfico): todas por GET y sin cuerpo.
//
//   - Cada 60 s: system/information?action=get (sin validationkey).
//   - Cada 300 s: subscription/plan?action=get, subscription?action=get,
//     profile/role?action=get, profile/client?action=get-update-info&component=windows
//     y media?action=get-storage-space&softdeleted=true.
//   - Al arrancar, además: login/oauth, features, profile?action=get y
//     profile/fields?action=list.
//
// El proxy ya consulta profile/changes por su cuenta (sincronización de metadatos).

// OfficialHeartbeatEnabled indica si el proxy debe latir como la app (perfil desktop).
func (c *Client) OfficialHeartbeatEnabled() bool {
	return c.officialClient()
}

func (c *Client) officialGet(pathAndQuery string) error {
	req, err := http.NewRequest("GET", c.baseURL+pathAndQuery, nil)
	if err != nil {
		return err
	}
	resp, err := c.DoRequest(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d", strings.SplitN(pathAndQuery, "?", 2)[0], resp.StatusCode)
	}
	return nil
}

// OfficialMinuteBeat es el latido de cada minuto (system/information).
func (c *Client) OfficialMinuteBeat() error {
	_, err := c.GetSystemInfo()
	return err
}

// OfficialFiveMinuteBeat es la ronda de cada 5 minutos. Devuelve el primer error,
// pero hace todas las llamadas (como la app, que no se detiene por una).
func (c *Client) OfficialFiveMinuteBeat() error {
	var first error
	for _, pq := range []string{
		"/subscription/plan?action=get",
		"/subscription?action=get",
		"/profile/role?action=get",
		"/profile/client?action=get-update-info&component=windows",
	} {
		if err := c.officialGet(pq); err != nil && first == nil {
			first = err
		}
		if c.IsSessionExpired() {
			return first
		}
	}
	if _, err := c.GetStorageSpace(); err != nil && first == nil {
		first = err
	}
	return first
}

// OfficialStartup imita el arranque de la app: login/oauth con los tokens guardados
// y las lecturas de perfil que hace después. Los fallos se registran sin más: el
// resto del proxy sigue su curso y la renovación normal se encarga de la sesión.
func (c *Client) OfficialStartup() {
	if !c.officialClient() || !c.HasCredentials() {
		return
	}
	start := time.Now()
	if ok, _ := c.refreshSessionInternal(false); !ok {
		logger.Log("[SAPI] ⚠️ login/oauth al arrancar no renovó la sesión; se sigue con la guardada.")
	}
	for _, pq := range []string{"/features", "/profile?action=get", "/profile/fields?action=list"} {
		if err := c.officialGet(pq); err != nil {
			logger.Log("[SAPI] ℹ️ Arranque como la app: %v", err)
		}
	}
	logger.Log("[SAPI] 🖥️ Arranque como el cliente oficial completado (%s)", time.Since(start).Round(time.Millisecond))
}
