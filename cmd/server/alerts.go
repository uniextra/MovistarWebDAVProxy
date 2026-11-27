package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/notifier"
	"movistar-webdav-proxy/internal/sapi"
	"movistar-webdav-proxy/internal/webclient"
	"movistar-webdav-proxy/internal/webdav"
)

// healthAlertAfter: un problema tiene que durar esto antes de avisar por Telegram;
// los cortes de unos segundos (la sesión se recupera sola) no avisan.
const healthAlertAfter = 2 * time.Minute

type healthCheck struct {
	key, title, desc, okTitle, okDesc string
	ok                                func() bool
	badSince                          time.Time
	alerted                           bool
}

// step comprueba una vez: avisa si lleva mal más de healthAlertAfter y, si ya avisó,
// avisa también cuando se recupera.
func (c *healthCheck) step(now time.Time) {
	if c.ok() {
		if c.alerted {
			_ = notifier.Alert("", "✅", c.okTitle, c.okDesc, "")
		}
		c.badSince, c.alerted = time.Time{}, false
		return
	}
	if c.badSince.IsZero() {
		c.badSince = now
	}
	if !c.alerted && now.Sub(c.badSince) >= healthAlertAfter {
		c.alerted = true
		_ = notifier.Alert("", "⚠️", c.title, c.desc, "")
	}
}

// watchHealth vigila la sesión web cada 30 s y da por terminadas las copias de
// clientes que dejan de informar.
func watchHealth(ctx context.Context, sess *webclient.Session) {
	var checks []*healthCheck
	if sess != nil {
		checks = append(checks, &healthCheck{
			key: "web_session_lost", title: "Sesión de Movistar Cloud perdida",
			desc:    "El proxy no puede leer ni subir a la nube. Si no se recupera sola en unos minutos, vuelve a entrar desde el panel en /web-relogin/ (no cierra la sesión actual).",
			okTitle: "Sesión de Movistar Cloud recuperada", okDesc: "El proxy vuelve a leer y subir con normalidad.",
			ok: sess.LoggedIn,
		})
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, c := range checks {
				c.step(now)
			}
			webdav.SweepClientCopies(now)
		}
	}
}

// sizeLimit sigue los rechazos por tamaño de Movistar (MED-1048, o una subida que
// Movistar deja de leer): la subida más pequeña rechazada y la más grande aceptada
// desde el primer rechazo acotan el límite que tiene ahora la cuenta.
var sizeLimit struct {
	sync.Mutex
	rejected, maxOK int64
}

// watchUploadSizeLimit avisa por Telegram cuando Movistar rechaza subidas por tamaño:
// limita a veces el tamaño por subida de una cuenta tras mucho volumen, y el
// chunk_size del chunker de rclone (en el cliente) tiene que quedar por debajo.
func watchUploadSizeLimit() {
	sapi.UploadRejected = func(size int64) {
		if size <= 0 {
			return
		}
		sizeLimit.Lock()
		if sizeLimit.rejected == 0 || size < sizeLimit.rejected {
			sizeLimit.rejected = size
		}
		if sizeLimit.maxOK >= sizeLimit.rejected {
			sizeLimit.maxOK = 0
		}
		rejected, maxOK := sizeLimit.rejected, sizeLimit.maxOK
		sizeLimit.Unlock()
		_ = notifier.Alert(fmt.Sprintf("upload_size_limit_%d", rejected>>20), "✂️", "Movistar rechaza subidas por tamaño",
			sizeLimitText(rejected, maxOK), "")
	}
	sapi.UploadAccepted = func(size int64) {
		sizeLimit.Lock()
		if sizeLimit.rejected > 0 && size < sizeLimit.rejected && size > sizeLimit.maxOK {
			sizeLimit.maxOK = size
		}
		sizeLimit.Unlock()
	}
}

func sizeLimitText(rejected, maxOK int64) string {
	limit := fmt.Sprintf("por debajo de %d MiB", rejected>>20)
	if maxOK > 0 {
		limit = fmt.Sprintf("entre %d y %d MiB", maxOK>>20, rejected>>20)
	}
	return fmt.Sprintf("Movistar ha rechazado subidas de %d MiB (MED-1048): ahora mismo la cuenta admite subidas %s. "+
		"Si usas el chunker de rclone, baja su chunk_size por debajo de ese límite (p. ej. 95M para un límite de 100 MiB); "+
		"lo ya subido se sigue leyendo igual.", rejected>>20, limit)
}
