package webdav

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Soporte de la cabecera X-OC-Mtime (convención de ownCloud/Nextcloud).
//
// Los clientes WebDAV que quieren conservar la fecha de modificación original
// envían en el PUT la cabecera "X-OC-Mtime: <segundos Unix>". rclone la envía
// con vendor=rclone, owncloud o nextcloud; también la usan los clientes de
// sincronización de ownCloud/Nextcloud. Si el servidor la aplica, responde con
// "X-OC-Mtime: accepted".
//
// Sin la cabecera, el comportamiento no cambia: la fecha es la de subida.

const ocMtimeHeader = "X-OC-Mtime"

type ocMtimeKey struct{}

// parseOCMtime interpreta el valor de X-OC-Mtime (segundos Unix, con decimales
// opcionales). Devuelve false si falta, no es numérico o está fuera de rango.
func parseOCMtime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	// Rechazar valores absurdos: posteriores a un día en el futuro o tan grandes
	// que no caben en una fecha razonable (p. ej. milisegundos por error).
	if secs > float64(time.Now().Add(24*time.Hour).Unix()) {
		return time.Time{}, false
	}
	sec := int64(secs)
	nsec := int64((secs - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC(), true
}

// withOCMtime lee X-OC-Mtime de un PUT y, si es válida, la guarda en el
// contexto de la petición y anuncia al cliente que se aplicará.
func withOCMtime(w http.ResponseWriter, r *http.Request) *http.Request {
	t, ok := parseOCMtime(r.Header.Get(ocMtimeHeader))
	if !ok {
		return r
	}
	w.Header().Set(ocMtimeHeader, "accepted")
	return r.WithContext(context.WithValue(r.Context(), ocMtimeKey{}, t))
}

// ocMtimeFromContext devuelve la fecha pedida por el cliente, o el valor cero.
func ocMtimeFromContext(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	if t, ok := ctx.Value(ocMtimeKey{}).(time.Time); ok {
		return t
	}
	return time.Time{}
}
