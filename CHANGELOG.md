# Changelog

Todos los cambios notables en **Movistar / O2 Cloud WebDAV Proxy** se documentan en este archivo.

## [v2.2.5] - 2026-11-05

### 🔒 Seguridad: Corrección de Bypass de Autenticación en NTLMv2 (GHSA-fpxr-64gm-wxvf)
- **Ligar sesiones NTLM a la conexión TCP real (`net.Conn`):**
  - Se sustituye el indexado de sesiones autenticadas por `r.RemoteAddr` (par IP:puerto) por un identificador unívoco de conexión mediante `http.Server.ConnContext`.
  - Evita que clientes no autenticados puedan heredar la sesión NTLM de otro usuario cuando un proxy inverso (Nginx, Traefik, Caddy, etc.) agrupa y reutiliza conexiones TCP upstream.
- **Invalidación inmediata en cierre de conexión (`ConnState`):**
  - Implementación de `http.Server.ConnState` para llamar automáticamente a `InvalidateConn(id)` al pasar la conexión a `StateClosed` o `StateHijacked`.
- **Desactivación de herencia de sesión NTLM tras proxies de confianza (`RemoteIsTrustedProxy`):**
  - Ante peticiones provenientes de `TRUSTED_PROXIES`, la autenticación se exige de forma estricta por petición sin confiar en la autenticación por conexión TCP upstream.
- **Suite de tests exhaustiva:** Cobertura con 7 tests unitarios en `internal/webdav/ntlm_connreuse_test.go`.
- *Agradecimiento especial a @aarizaga90 por el reporte de seguridad responsable y la contribución del parche.*

## [v2.2.4] - 2026-11-05

### 🛡️ Permisos y Resiliencia en Almacenamiento Temporal (`WEBDAV_TMPDIR`)
- **Auto-asignación de permisos en `docker-entrypoint.sh`:**
  - Se añade inicialización automática para directorios temporales personalizados (`WEBDAV_TMPDIR`, ej. `/app/tmp` mapeado desde el host).
  - Al arrancar como root, el contenedor crea y asigna la propiedad a `app:app` (UID `10001`) con permisos `700`.
  - Si el volumen montado desde el host restringe permisos a nivel de sistema de ficheros e impide el acceso al usuario `app`, el contenedor conmuta de forma segura y transparente a ejecución root sin provocar errores `405 Method Not Allowed` / `permission denied` en runtime.
- **Telemetría y Diagnóstico de Archivos Temporales:**
  - En caso de fallo al crear el archivo temporal en disco previo a la subida a Movistar/O2 Cloud (`os.CreateTemp`), se emite un log explícito indicando la ruta y el motivo exacto (permisos o falta de espacio libre en disco).

## [v2.2.3] - 2026-11-05

### 🐧 Compatibilidad Linux Desktop & Reverse Proxies (Issue #15)
- **Desafío NTLM Adaptativo (`shouldOfferNTLM`):**
  - Se restringe el envío de la cabecera `WWW-Authenticate: NTLM` exclusivamente a clientes Windows nativos (`Microsoft-WebDAV-MiniRedir`, `davclnt`, `winhttp`) o clientes que inicien explícitamente un handshake NTLM.
  - Para clientes Linux desktop (GNOME Nautilus/GVFS, KDE Dolphin/KIO, rclone, curl) se envía únicamente `WWW-Authenticate: Basic realm="WebDAV"`.
  - Soluciona el error `Intento de acceso NTLM fallido` al acceder desde Linux a través de reverse proxies (Nginx Proxy Manager, Traefik, Caddy), donde libsoup priorizaba erróneamente NTLM sobre Basic Auth y el proxy no mantenía la afinidad de conexión requerida por NTLM.
  - Se añade la variable de entorno `WEBDAV_NTLM` (`auto` [por defecto], `true`/`always`, `false`/`disabled`) para forzar o desactivar la negociación NTLM si fuera necesario.

### 🛡️ Absorción en Memoria de Sondas Duplicati (Issue #13 / PR #14)
- **Soporte transparente para el botón "Test connection" de Duplicati:**
  - La API SAPI de Telefónica rechaza de forma estricta los archivos temporales con extensión `.tmp` con error `MED-1000: Unknown exception in media handling`.
  - Duplicati ejecuta obligatoriamente una sonda creando, leyendo y borrando el archivo `duplicati-access-privileges-test.tmp`.
  - Se incorpora `DuplicatiProbeStore`, un almacén volátil en memoria RAM (TTL 5 min, límite 10MB) que intercepta y responde a nivel WebDAV (`PUT` -> 201, `GET` -> 200, `PROPFIND` -> 207, `DELETE` -> 204, `LOCK`/`UNLOCK`) sin enviar solicitudes a los servidores de Movistar/O2.

### ⚡ Optimizaciones y Consistencia en Renombrado (`moveByReupload` / PR #12)
- **Re-subida directa desde memoria y confirmación de URL SAPI:**
  - Al cambiar de extensión un archivo recién subido presente en `uploadCache` (RAM) o con tamaño $\le$ 256 KB, se re-sube directamente sin necesidad de descargarlo por red.
  - Para archivos mayores recién subidos, se implementa `waitForDownloadURL` con sondeo activo para garantizar que SAPI ha publicado la URL de descarga antes de proceder al reemplazo.
  - El archivo resultante se registra inmediatamente en `uploadCache` con su nuevo ID de SAPI para lecturas inmediatas consistentes.
  - Verificación estricta de integridad de tamaño de descarga antes de eliminar el archivo origen.

## [v2.2.2] - 2026-11-05

### 🐛 Corrección: Descarga de Archivos JSON y Metadatos de Home Assistant (Issue #11)
- **Soporte transparente para descarga de archivos JSON / JS legítimos:**
  - Se corrigió el falso positivo en `DownloadFileStreamRange` (`internal/sapi/client.go`), donde cualquier respuesta HTTP 200 con `Content-Type: application/json` o `text/javascript` era rechazada incondicionalmente como error de la API SAPI.
  - Ahora se realiza una inspección no destructiva de los primeros bytes mediante `bufio.Reader.Peek()`: solo se descartan las respuestas que efectivamente contengan la estructura de error de SAPI (`{"error": {"code": ...}}`, `error_code`, `status: "error"`) o si el archivo solicitado es un binario (`.tar`, `.zip`, `.iso`, etc.) que no debería retornar JSON.
  - Si el archivo descargado es un JSON de usuario válido (como los metadatos `<backup_id>.metadata.json` generados por el agente de backups de Home Assistant), el flujo se transmite íntegramente al cliente sin cortes ni excepciones `ContentLengthError (received 0 of N bytes)`.
  - Cobertura de tests unitarios exhaustiva en `internal/sapi/download_json_test.go`.

## [v2.2.1] - 2026-11-05

### 🪟 Autenticación NTLMv2 y Reconexión Persistente en Windows
- **Soporte NTLMv2 en Paralelo con HTTP Basic Auth:**
  - Implementación del protocolo de autenticación NTLMv2 (`internal/config/ntlm.go`, `internal/webdav/server.go`) junto a HTTP Basic Auth.
  - Permite la reconexión automática, persistente y silenciosa de unidades de red mapeadas en Windows 10 y Windows 11 (`WebClient` / `DAV Mini-Redirector`) tras el reinicio del sistema, eliminando el fallo recurrente de reconexión sin interacción del usuario.
  - Las cabeceras `WWW-Authenticate: Basic realm="WebDAV"` y `WWW-Authenticate: NTLM` se emiten conjuntamente, manteniendo compatibilidad total y transparente con clientes que utilizan Basic Auth (Linux, macOS, Duplicati, rclone, Synology HyperBackup, Cyberduck, curl, etc.).
- **Telemetría Segura de Credenciales (`DescribeCredentialShape`):**
  - Diagnóstico detallado en logs ante intentos fallidos de autenticación (longitud de usuario, presencia de dominio Windows `\`, longitud de contraseña, espacios al inicio/final o caracteres no-ASCII), facilitando la detección de codificaciones o entradas erróneas sin exponer jamás contraseñas ni secretos en texto plano.

## [v2.2.0] - 2026-11-04

### 🚀 Rendimiento, Concurrencia y Caché
- **Arquitectura de Concurrencia Desacoplada (4/4 con Techo Global de 5):**
  - Sustitución del semáforo compartido rígido de 3 hilos por semáforos independientes para subidas (`uploadSem = 4`) y descargas (`downloadSem = 4`), protegidos por un techo conjunto de seguridad (`globalTransferSem = 5`).
  - Optimiza de forma nativa los 4 hilos paralelos de Duplicati y acelera restauraciones sin sobrepasar la cuota por token de la API de Telefónica.
- **Caché Zero-Allocation y Resolución de Rutas O(1):**
  - Índices en memoria indexados por nombre (`byExact` y `byLower`) en la caché de carpetas y multimedia, eliminando la duplicación masiva de slices (`make([]MediaItem, ...)` y `copy()`) en cada resolución de ruta WebDAV.
- **Reutilización de Conexiones HTTP y Resiliencia SAPI:**
  - Configurado `MaxIdleConnsPerHost = 20` (frente al valor por defecto de 2 de Go) eliminando la destrucción y re-negociación continua de sockets TLS hacia `micloud.movistar.es`.
  - Retroceso exponencial adaptativo con jitter ante respuestas HTTP 429 (Too Many Requests) o 503 en SAPI (hasta 3 reintentos antes de abortar).
- **Buffer de Subida PUT en RAM (32 MB):**
  - Añadida variable `WEBDAV_BUFFER_MB` (por defecto 32 MB), permitiendo que el 99% de bloques de Duplicati y restic se procesen directamente en memoria RAM sin generar escrituras temporales en disco.
- **Suite de Pruebas de Interoperabilidad con `emersion/go-webdav`:**
  - Verificación de cumplimiento estricto del estándar RFC 4918 mediante el cliente oficial de referencia `emersion/go-webdav v0.7.0`.

### 🪟 Autenticación Multiplataforma y Redes Docker
- **Normalización de Credenciales Windows:**
  - Limpieza automática de prefijos de dominio habituales en Windows WebClient (`DOMAIN\usuario` y `\usuario` $\rightarrow$ `usuario`) y recorte de espacios en blanco accidentales.
  - Preservación exacta de contraseñas complejas que incluyan caracteres de dos puntos (`:`) o espacios internos.
- **Desafío RFC 4918 y Descubrimiento WebClient:**
  - Respuestas HTTP `401 Unauthorized` con el encabezado estándar `WWW-Authenticate: Basic realm="WebDAV"`.
  - Tratamiento especial para la petición exploratoria inicial `OPTIONS /` sin credenciales (devolviendo `200 OK` con `MS-Author-Via: DAV` y `DAV: 1, 2`), permitiendo al Explorador de Windows identificar el servicio antes de solicitar credenciales en `PROPFIND /`.
- **Detección Segura de IP y Anti-Spoofing (`TRUSTED_PROXIES`):**
  - Prioriza la IP real del socket (`RemoteAddr`) y solo evalúa `X-Forwarded-For` o `X-Real-IP` cuando la conexión procede de un proxy inverso explícitamente configurado.
- **Exclusión de Subredes Docker contra Bloqueos 429 (`BRUTE_FORCE_EXCLUDED_IPS`):**
  - Exclusión automática de subredes puente de Docker (`172.16.0.0/12`) y loopback del limitador de fuerza bruta, evitando que un cliente con clave errónea bloquee la pasarela compartida de Docker para toda la red local. La autenticación Basic Auth se sigue exigiendo en todo momento.

### 🐛 Correcciones Críticas de Compatibilidad
- **Mover y Renombrar Audio, Vídeo e Imágenes (PR #8):**
  - Incorporada la operación adecuada de SAPI según el tipo de archivo (`upload/audio`, `upload/video`, `upload/picture` o `upload/file`), evitando que fotos y música fallen silenciosamente al renombrarse o moverse con errores `MED-1005`.
  - Detección de errores devueltos en el cuerpo JSON de SAPI con códigos HTTP 2xx y reintentos ante archivos en validación (`MED-1017`).
- **Renombrado con Cambio de Extensión (`moveByReupload` - Issue #9):**
  - Si la extensión de un archivo cambia al renombrarse (ej. `.tmp` $\rightarrow$ `.zip.aes` en Duplicati), el proxy realiza automáticamente una descarga y re-subida con el nuevo nombre antes de borrar el original, sorteando la limitación de SAPI que provocaba errores `MED-1050` o forzaba la extensión original.
- **Persistencia de Notificaciones Telegram en WebUI (Issue #10):**
  - Solucionado el fallo al guardar la configuración de Telegram que solicitaba un teléfono válido aunque la cuenta ya estuviera vinculada, haciendo fallback automático a la cuenta activa.

## [v2.1.2] - 2026-11-03

### 🐛 Correcciones y Mejoras de Compatibilidad
- **Lectura Inmediata en O2 Cloud (PR #2, Issue #5):**
  - Consulta a SAPI por ID (`media?action=get`) para entradas recién subidas cuya respuesta no incluía `url` de descarga, evitando el error `COM-1005 Unsupported operation` y descargas de 0 bytes en herramientas como `restic check` o `rclone check --download`.
- **Conservación de Timestamp Original con `X-OC-Mtime` (PR #3):**
  - Soporte completo para la cabecera estándar de facto `X-OC-Mtime` (segundos Unix) utilizada por `rclone` y clientes Nextcloud/ownCloud.
  - Envío de `modificationdate` a SAPI y respuesta `X-OC-Mtime: accepted`.
  - Separación en `MemoryFile` entre `ModTime` (fecha servida al cliente) y `CachedAt` (caducidad de la caché en memoria).
- **Protección contra Subidas Cortadas e Incompletas (Issue #4):**
  - Incorporado `uploadGuard` en el cuerpo de peticiones `PUT`. Si la conexión con el cliente WebDAV se interrumpe o la cantidad de bytes recibidos no coincide con el `Content-Length` anunciado, se limpia el temporal y se aborta la subida sin modificar ni corromper archivos existentes en la nube.
- **Corrección de Colisiones de Nombres Parecidos (Issue #4):**
  - Eliminada la heurística agresiva de expresiones regulares para sufijos ` (1)`, permitiendo que archivos como `foto.jpg` y `foto (1).jpg` se traten como archivos completamente independientes al buscar, listar (`Readdir`) y borrar.
- **Corrección en Borrado de Carpetas en la Nube (Issue #7):**
  - Añadido parámetro obligatorio `&id=<folder_id>` en la llamada SAPI `POST /media/folder?action=softdelete`. Anteriormente SAPI devolvía código interno `COM-1011: Missing required parameter` provocando que las carpetas eliminadas por WebDAV no se borraran realmente en la nube de Movistar/O2.
  - Validación explícita de errores devueltos por SAPI al borrar carpetas y archivos, evitando reportar éxito falso.
- **Deadlock de RWMutex en Listado de Directorios:**
  - Corregido auto-deadlock en `WebDAVDirReader.Readdir` que congelaba el servidor WebDAV al solicitar `getMedia()` mientras se retenía un `RLock`.
- **Compatibilidad con Windows WebClient:**
  - Soporte para nombres de usuario con prefijo de dominio (`PC\usuario` o `.\usuario`) al validar Basic Auth.
  - Prevención de error `http: read on closed response body` en peticiones rechazadas con `HTTP 401`.
- **Documentación y Docker:**
  - Actualizadas las instrucciones y ejemplos de `README.md` y `docker-compose.yml` para usar `:latest` por defecto.

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
  - Cifrado en reposo opcional con AES-256-GCM mediante la variable de entorno `TOKENS_ENCRYPTION_KEY` (`enc:v1:...`), manteniendo retrocompatibilidad con archivos existentes en texto plano.
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
