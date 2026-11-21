package webdav

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/notifier"
)

// Avisos por Telegram de errores que el cliente no ve (los reintenta en silencio):
// el límite de carpetas de la cuenta y una racha de subidas fallidas.
const (
	uploadErrWindow = 10 * time.Minute
	uploadErrAlert  = 10 // errores de escritura en la ventana para avisar
)

var uploadErrs struct {
	sync.Mutex
	at []time.Time
}

// noteRequestError se llama con cada error de una petición WebDAV.
func noteRequestError(method string, err error, now time.Time) {
	msg := err.Error()
	if strings.Contains(msg, "FOL-1035") {
		_ = notifier.Alert("folder_limit", "🚫", "Movistar: límite de carpetas alcanzado",
			"La cuenta no admite más carpetas (FOL-1035): las subidas que necesiten una carpeta nueva fallan hasta que se borren carpetas.", msg)
	}
	switch method {
	case "PUT", "MKCOL", "MOVE", "COPY":
	default:
		return
	}
	uploadErrs.Lock()
	keep := uploadErrs.at[:0]
	for _, t := range uploadErrs.at {
		if now.Sub(t) < uploadErrWindow {
			keep = append(keep, t)
		}
	}
	uploadErrs.at = append(keep, now)
	n := len(uploadErrs.at)
	uploadErrs.Unlock()
	if n >= uploadErrAlert {
		_ = notifier.Alert("upload_errors", "❌", "Las subidas a Movistar están fallando",
			fmt.Sprintf("%d errores de escritura en los últimos %d minutos.", n, int(uploadErrWindow.Minutes())), msg)
	}
}
