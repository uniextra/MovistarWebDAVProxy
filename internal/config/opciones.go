package config

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Opciones del perfil desktop: el proxy hablando con O2 Cloud como el cliente
// oficial de Windows. Están todas aquí, cada una con su variable de entorno y su
// valor por defecto, y documentadas en docs/configuration.md. Se leen al usarlas,
// así que no hay que reiniciar nada más que el proceso para cambiarlas.
//
// Los valores por defecto que imitan a la app (latido, validación) salen de las
// capturas de noviembre de 2026; cambiarlos aleja al proxy del comportamiento del
// cliente oficial.

// Perfil con el que el proxy se presenta ante SAPI (SAPI_CLIENT_PROFILE).
const (
	ProfileDesktop = "desktop"
	ProfileBrowser = "browser"
)

// ClientProfile devuelve el perfil activo: "desktop" (por defecto), o "browser" con
// SAPI_CLIENT_PROFILE=browser o en modo web (WEB_SESSION=true).
func ClientProfile() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SAPI_CLIENT_PROFILE"))) {
	case "browser", "web", "chrome":
		return ProfileBrowser
	case "desktop", "windows":
		return ProfileDesktop
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("WEB_SESSION")), "true") {
		return ProfileBrowser
	}
	return ProfileDesktop
}

// IsDesktopProfile indica si el proxy imita al cliente oficial de escritorio.
func IsDesktopProfile() bool {
	return ClientProfile() == ProfileDesktop
}

// officialClientMode: SAPI_OFFICIAL_CLIENT = auto (por defecto) | true | false.
func officialClientMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SAPI_OFFICIAL_CLIENT"))) {
	case "1", "true", "yes", "on":
		return "on"
	case "0", "false", "no", "off":
		return "off"
	}
	return "auto"
}

// OfficialClientFor indica si, con el perfil desktop, el proxy habla como el
// cliente oficial (subida en dos pasos, reanudación, latido, relectura, seguimiento
// de la validación, borrados y metadatos como la app) para ese proveedor. Por
// defecto solo con O2: todo eso se ha comprobado contra O2 Cloud y nunca contra
// Movistar Cloud. SAPI_OFFICIAL_CLIENT=true lo activa también con Movistar (para
// probarlo) y false lo desactiva siempre.
func OfficialClientFor(provider string) bool {
	if !IsDesktopProfile() {
		return false
	}
	switch officialClientMode() {
	case "on":
		return true
	case "off":
		return false
	}
	return strings.EqualFold(strings.TrimSpace(provider), "o2")
}

// OfficialClientForHost es OfficialClientFor según el servidor al que se conecta
// (para la huella TLS hacia los servidores de subida y descarga).
func OfficialClientForHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	provider := "movistar"
	if h == "o2online.es" || strings.HasSuffix(h, ".o2online.es") {
		provider = "o2"
	}
	return OfficialClientFor(provider)
}

// StreamUploadsFor (WEBDAV_STREAM_UPLOAD): subir a O2 mientras el archivo llega del
// cliente, como la app. Sin la variable, sí donde se habla como la app
// (OfficialClientFor: O2 en el perfil desktop) y no en el resto, como antes.
func StreamUploadsFor(provider string) bool {
	if v, ok := envBoolSet("WEBDAV_STREAM_UPLOAD"); ok {
		return v
	}
	return OfficialClientFor(provider)
}

// CaseInsensitiveNamesFor (WEBDAV_CASE_INSENSITIVE): buscar nombres sin distinguir
// mayúsculas. O2 Cloud las distingue («A.txt» y «a.txt» son dos archivos), y
// responder a «a.txt» con «A.txt» hacía que rclone diera por subido un archivo que no
// lo estaba o, en un cambio solo de mayúsculas, borrara el origen. Sin la variable,
// nombres exactos donde se habla como la app (comprobado con O2) y, en el resto,
// sin distinguir, como antes.
func CaseInsensitiveNamesFor(provider string) bool {
	if v, ok := envBoolSet("WEBDAV_CASE_INSENSITIVE"); ok {
		return v
	}
	return !OfficialClientFor(provider)
}

// RefreshAfterUpload (SAPI_REFRESH_AFTER_UPLOAD, por defecto true): releer cada
// archivo subido, como la app, para guardar su URL de descarga firmada.
func RefreshAfterUpload() bool { return envBool("SAPI_REFRESH_AFTER_UPLOAD", true) }

// UnsupportedAsFile (SAPI_UNSUPPORTED_AS_FILE, por defecto false): si O2 rechaza una
// imagen, audio o vídeo porque su contenido no corresponde a la extensión (MED-1050),
// volver a subirlo como archivo genérico (upload/file) para que no se pierda. La app
// oficial no lo hace: deja el archivo sin subir. Solo para archivos nuevos.
func UnsupportedAsFile() bool { return envBool("SAPI_UNSUPPORTED_AS_FILE", false) }

// Latido como la app: system/information cada HeartbeatInterval y el resto de
// consultas periódicas cada HeartbeatFullInterval.
func HeartbeatInterval() time.Duration { return envSeconds("SAPI_HEARTBEAT_SEC", 60*time.Second) }
func HeartbeatFullInterval() time.Duration {
	return envSeconds("SAPI_HEARTBEAT_FULL_SEC", 300*time.Second)
}

// Seguimiento de la validación tras subir: primera consulta a los
// ValidationFirstWait, luego cada vez más espaciadas hasta ValidationMaxWait; se deja
// tras ValidationMaxFailures consultas fallidas seguidas.
func ValidationFirstWait() time.Duration {
	return envSeconds("SAPI_VALIDATION_FIRST_SEC", 10*time.Second)
}
func ValidationMaxWait() time.Duration { return envSeconds("SAPI_VALIDATION_MAX_SEC", 60*time.Second) }
func ValidationMaxFailures() int       { return envInt("SAPI_VALIDATION_MAX_FAILURES", 60) }

// Envío del contenido de una subida: UploadAttempts intentos (con reanudación entre
// ellos), con esperas crecientes (2, 5, 10, 20, 30, 45, 60 s…) de hasta
// UploadRetryMaxWait.
func UploadAttempts() int { return envInt("SAPI_UPLOAD_ATTEMPTS", 8) }
func UploadRetryMaxWait() time.Duration {
	return envSeconds("SAPI_UPLOAD_RETRY_MAX_SEC", 60*time.Second)
}

// Login fallido en el perfil desktop: reintento a los LoginRetryFirstWait, que se
// dobla hasta LoginRetryMaxWait; la sesión se da por caducada solo tras
// LoginGiveUpRejections rechazos de credenciales durante al menos LoginGiveUpAfter.
func LoginRetryFirstWait() time.Duration {
	return envSeconds("SAPI_LOGIN_RETRY_FIRST_SEC", 30*time.Second)
}
func LoginRetryMaxWait() time.Duration {
	return envSeconds("SAPI_LOGIN_RETRY_MAX_SEC", 600*time.Second)
}
func LoginGiveUpRejections() int      { return envInt("SAPI_LOGIN_GIVEUP_REJECTIONS", 4) }
func LoginGiveUpAfter() time.Duration { return envSeconds("SAPI_LOGIN_GIVEUP_SEC", 1800*time.Second) }

// EmptyManifestRetryWait (WEBDAV_EMPTY_MANIFEST_RETRY_SEC): si no se pudo subir el
// manifiesto de archivos vacíos, se reintenta tras esta espera.
func EmptyManifestRetryWait() time.Duration {
	return envSeconds("WEBDAV_EMPTY_MANIFEST_RETRY_SEC", 120*time.Second)
}

// Copia en la nube (COPY) sin respuesta: se busca la copia cada CopyPollInterval
// durante CopyWaitBase + tamaño/CopyWaitBytesPerSec, hasta CopyWaitMax.
func CopyPollInterval() time.Duration { return envSeconds("WEBDAV_COPY_POLL_SEC", 10*time.Second) }
func CopyWaitBase() time.Duration     { return envSeconds("WEBDAV_COPY_WAIT_BASE_SEC", 120*time.Second) }
func CopyWaitMax() time.Duration      { return envSeconds("WEBDAV_COPY_WAIT_MAX_SEC", 600*time.Second) }
func CopyWaitBytesPerSec() int64 {
	return int64(envInt("WEBDAV_COPY_WAIT_MBPS", 10)) << 20
}

// envBool lee un sí/no; vacío o no reconocido = def.
func envBool(name string, def bool) bool {
	if v, ok := envBoolSet(name); ok {
		return v
	}
	return def
}

// envBoolSet lee un sí/no; ok = false si la variable está vacía o no se reconoce.
func envBoolSet(name string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// envInt lee un entero positivo; vacío o no válido = def.
func envInt(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}

// envSeconds lee segundos (admite decimales, «0.5»); vacío o no válido = def.
func envSeconds(name string, def time.Duration) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return def
	}
	return time.Duration(f * float64(time.Second))
}
