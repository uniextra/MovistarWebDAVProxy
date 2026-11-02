# Changelog

Todos los cambios notables en **Movistar / O2 Cloud WebDAV Proxy** se documentan en este archivo.

## [v2.1.0] - 2026-11-02

### 🔒 Seguridad y Hardening
- **Autenticación HTTP Basic Real (`WEBDAV_USER` / `WEBDAV_PASSWORD` y `WEBUI_USER` / `WEBUI_PASSWORD`):**
  - Validación de credenciales en tiempo constante (`crypto/subtle` + SHA-256) tanto en el servidor WebDAV como en el Panel Web (excepto `/health`).
  - Modo de actualización retrocompatible: si el contenedor se actualiza sin variables de autenticación definidas, arranca mostrando un aviso `[WARNING]` en lugar de entrar en bucle de reinicio (salvo que se active `REQUIRE_AUTH=true`).
  - Rate-limiting en memoria contra ataques de fuerza bruta por IP (bloqueo `429 Too Many Requests` tras 10 intentos fallidos por minuto, compatible con IPv4 e IPv6 mediante `net.SplitHostPort`).
- **Protección del Panel Web (`internal/webui`):**
  - Restricción estricta a método `POST` (`405 Method Not Allowed`), validación `Content-Type: application/json`, comprobación de `Origin`/`Host` contra CSRF (`403 Forbidden`) y límite de cuerpo de 1 MB (`http.MaxBytesReader`) en todos los endpoints mutadores.
  - Cabeceras de seguridad HTTP (`Content-Security-Policy`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`) y respuesta `404 Not Found` para rutas desconocidas fuera de `/`.
  - Eliminada la exposición en claro de `telegram_bot_token` en `/verify_phone` (ahora devuelve `telegram_configured` y `telegram_token_masked`).
- **Persistencia y Enmascarado de Credenciales (`internal/config`, `internal/logger`, `internal/notifier`):**
  - Validación de formato de `bot_token` y `chat_id` de Telegram, rechazo de caracteres de control/saltos de línea (`\n`, `\r`, `\x00`) y escapado seguro de comillas en los archivos `.env`.
  - Archivo único por cuenta (`account_<phone>.env`) con permisos `0600` sobre directorio `0700` y limpieza automática de `.env` duplicados legacy para proteger la rotación de tokens (RTR).
  - Enmascarado de números de teléfono, `device_id` y tokens en logs (`logger.Mask`), y sanitización de errores `*url.Error` en el cliente de Telegram y SAPI.
- **Flujo OAuth2 Endurecido (`internal/auth`, `internal/webui`):**
  - Validación en tiempo constante del parámetro `state` OAuth2 (cuando se proporciona la URL de redirección), caducidad de sesiones de login pendientes a los 10 minutos y límite de memoria acotado.
  - Validación estricta por expresión regular precompilada en `ExtractAuthCode` y comprobación de errores de `crypto/rand.Read`.

### 🚀 Robustez y Contenedor
- **Actualización a Go 1.24 y `golang.org/x/net`:** Migración desde Go 1.22 (EOL) a Go 1.24 (`golang:1.24-alpine3.21`) y actualización de `golang.org/x/net` para mitigar CVEs conocidos en HTTP/2.
- **Ejecución No-Root con Migración Transparente:** El contenedor se ejecuta bajo el usuario `app` (`UID 10001`). Incluye `docker-entrypoint.sh` con `su-exec` para ajustar automáticamente la propiedad del volumen `/app/tokens` al actualizar desde versiones anteriores que corrían como `root`.
- **Límites y Timeouts HTTP:**
  - Configuración de `ReadHeaderTimeout` e `IdleTimeout` en ambos servidores HTTP y `ReadTimeout`/`WriteTimeout` en el panel web.
  - Límite real de tamaño de subida configurable mediante `MAX_UPLOAD_MB` (por defecto `4096` MB) con respuesta `413 Request Entity Too Large` y soporte de `WEBDAV_TMPDIR`.
  - `HEALTHCHECK` integrado en el binario (`/app/movistar-proxy -healthcheck`) y apagado ordenado esperando a transferencias `PUT` en curso.
- **Tests y CI:** Suite completa de tests unitarios y de integración (`config`, `auth`, `webdav`, `webui`) y workflow de CI con `go test -race`, `go vet`, `govulncheck` y escaneo Trivy.
