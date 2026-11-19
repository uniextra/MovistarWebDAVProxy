package auth

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"movistar-webdav-proxy/internal/config"
)

// Perfil de cliente con el que el proxy se presenta ante SAPI.
//
// Capturando el tráfico del cliente oficial de Windows de O2 (v32.1.3,
// "O2 Cloud.exe", noviembre de 2026) se ve que, frente a SAPI, se presenta así:
//
//   - HTTP/1.1 (Qt 5), nunca HTTP/2.
//   - User-Agent "omh windows client 4.0.1".
//   - X-deviceid "fol-<base64>" (con '+', '/', '=') y "platform":"windows" en la
//     cabecera Authorization: oauth.
//   - Cabeceras: Accept "*/*", Accept-Language "es-ES,en,*", x-request-id (UUID por
//     petición); sin Referer ni X-Requested-With; cookie solo JSESSIONID y
//     validationkey en la URL.
//
// CloudFront exige esa coherencia: con HTTP/1.1, User-Agent "omh windows client …"
// y un X-deviceid "fol-…" la petición llega a SAPI, pero la misma petición por
// HTTP/2, o con User-Agent de navegador o "DESKTOP", se corta en el borde
// (401 vacío, Server: CloudFront). Un identificador "web-…" con User-Agent de
// navegador también pasa, que es el perfil "browser".
//
// SAPI_CLIENT_PROFILE elige el perfil:
//   - "desktop" (por defecto fuera del modo web): imita al cliente oficial.
//   - "browser": navegador (Chrome) por HTTP/2 con identificador "web-…".
//
// En modo web (WEB_SESSION=true) el perfil por defecto es "browser".
const (
	ProfileDesktop = config.ProfileDesktop
	ProfileBrowser = config.ProfileBrowser
)

// ClientProfile devuelve el perfil activo (ver config.ClientProfile).
func ClientProfile() string { return config.ClientProfile() }

// IsDesktopProfile indica si el proxy debe imitar al cliente oficial de escritorio.
func IsDesktopProfile() bool { return config.IsDesktopProfile() }

// DesktopAcceptLanguage es el Accept-Language que envía el cliente oficial (Qt).
const DesktopAcceptLanguage = "es-ES,en,*"

// NewRequestID devuelve un UUID v4 para la cabecera x-request-id, como el cliente oficial.
func NewRequestID() string {
	b := make([]byte, 16)
	mustRandRead(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// generateDesktopDeviceID crea un identificador con el formato del cliente oficial:
// "fol-" + base64 estándar de 16 bytes (24 caracteres, con '=' final).
func generateDesktopDeviceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand.Read failed: " + err.Error())
	}
	return "fol-" + base64.StdEncoding.EncodeToString(b)
}

// desktopDeviceIDFrom convierte un identificador "web-…" ya guardado en uno "fol-…"
// estable (siempre el mismo para el mismo origen), para no cambiar de dispositivo en
// cada arranque.
func desktopDeviceIDFrom(devID string) string {
	sum := md5.Sum([]byte("fol:" + devID))
	return "fol-" + base64.StdEncoding.EncodeToString(sum[:])
}
