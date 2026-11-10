# Changelog

Todos los cambios notables en **Movistar / O2 Cloud WebDAV Proxy** se documentan en este archivo.

## [v2.2.15] - 2026-11-10

### 🛡️ Restauración del Transporte TLS Estándar, Eliminación de Pausas Artificiales WAF y Mapeo MED-1020 (Issues #17, #23 y #26)

- **Causa Raíz de los Bloqueos WAF 403 en Descargas Concurrentes / Ráfagas (Issues #23 y #26):**
  - Mediante pruebas de estrés controladas en vivo (30 peticiones concurrentes combinando descargas `/sapi/download/file` en streaming y consultas `/sapi/media`), se demostró que la combinación introducida en `v2.2.12` (`MaxVersion: tls.VersionTLS12` + `ForceAttemptHTTP2: true`) provocaba un **100% de bloqueos `HTTP 403` en AWS CloudFront WAF Bot Control** en cuanto 2 streams HTTP/2 se solapaban sobre TLS 1.2 (por ejemplo, mientras un archivo seguía descargándose en `DownloadFileStreamRange` tras liberarse el semáforo de cabeceras).
  - Se restaura el transporte TLS estándar de Go (`TLS 1.3/1.2` automático como en `v2.2.8`) y se elimina toda la maquinaria de pausas globales artificiales de 26s (`wafCooldownUntil`) y *pacing* forzado que la app oficial no utiliza, manteniendo la concurrencia 1:1 (`SAPI_MAX_CONCURRENT = 1` y semáforos WebDAV a `1`). En la misma prueba de estrés en vivo de 30 peticiones rápidas, el transporte restaurado obtuvo **30/30 (`100% HTTP 200 OK`) y 0 bloqueos WAF**.
- **Mapeo Explícito de `MED-1020` a `HTTP 413 Request Entity Too Large` y Documentación de Proxy Inverso (Issue #17):**
  - Se comprobó en vivo que el límite exacto por archivo en Telefónica SAPI es de `4 GiB` ($2^{32} = 4.294.967.296$ bytes), devolviendo el código interno `MED-1020` a partir de `4 GiB + 1 byte`. Si SAPI devuelve `MED-1020`, el proxy responde ahora directamente con `413 Request Entity Too Large` sin reintentar la subida desde disco.
  - Se documenta en `README.md` la configuración necesaria para **Nginx Proxy Manager (NPM) / Nginx** (`client_max_body_size 0;` y `proxy_request_buffering off;`) y el límite de 2 GiB de clientes Linux basados en `GVFS` (`gvfsd-dav`).

## [v2.2.14] - 2026-11-10

### 🛡️ Alineación 1:1 de Concurrencia con el Cliente Oficial de Escritorio de Windows (Issues #23 y #26)

- **Serialización Completa de Transferencias y Peticiones SAPI (Concurrencia = 1):**
  - Tras la investigación exhaustiva por ingeniería inversa sobre el binario nativo del cliente oficial (`Movistar Cloud-sync.exe` / Funambol Sync Engine), se demostró que el cliente oficial transfiere archivos y consulta metadatos en **serie estricta** (un único archivo a la vez mediante `Sync::ParallelPropagation::uploadFiles` / `downloadFiles` bloqueante).
  - Se eliminan los desajustes entre la capa WebDAV local y la capa SAPI:
    - Semáforos WebDAV alineados: `uploadSem = 1`, `downloadSem = 1`, `globalTransferSem = 1`, `uploadBufferSem = 1`.
    - Semáforo SAPI por defecto alineado: `SAPI_MAX_CONCURRENT = 1`.
  - Esto evita encolamientos invisibles, cancela el consumo inútil de buffers RAM para transferencias en espera y asegura un flujo idéntico al del cliente oficial de Windows, impidiendo que AWS CloudFront WAF detecte ráfagas anómalas.

## [v2.2.13] - 2026-11-10

### 🛡️ Caché en RAM de Descargas de Micro-Archivos (Backrest / restic), Pacing Conservador y Silencio Total en Pausa Manual (Issue #23)

- **Caché en Memoria para Descargas de Archivos Pequeños (`<= 256 KB`):**
  - Clientes como **Backrest / restic** y **Synology HyperBackup** descargan repetidamente los mismos archivos pequeños de configuración y claves (`config`, `keys/*`, `locks/*`, `_Syno_TaskConfig`, `version_info`) varias veces en pocos segundos durante las fases de verificación y desbloqueo de repositorio, disparando la regla por endpoint de `/sapi/download/file` en AWS CloudFront WAF.
  - Ahora, cuando `WebDAVFileReader` descarga íntegramente un archivo pequeño (`<= 256 KB`), almacena automáticamente su contenido en la caché en memoria (`uploadCache`), sirviendo las siguientes lecturas `GET` desde RAM en `0 ms` sin realizar llamadas adicionales a `/sapi/download/file` (con invalidación automática inmediata ante cualquier `PUT`, `DELETE` o `MOVE`).
- **Ajuste de Pacing y Concurrencia por Defecto contra Reglas de Tasa WAF:**
  - Se incrementa el espaciado por defecto entre peticiones SAPI (`SAPI_PACING_MS`) de `50 ms` a `150 ms` (~6,5 req/s máx.) y se reduce la concurrencia por defecto (`SAPI_MAX_CONCURRENT`) de `3` a `2`, manteniendo las ráfagas de exploración inicial de directorios (`PROPFIND`) siempre por debajo del umbral de CloudFront.
- **Silencio Absoluto durante la Pausa Manual del Usuario (`/stop`):**
  - Cuando el usuario pausa manualmente el proxy desde el Panel de Control Web, tanto el worker `KeepAlive` en segundo plano como el endpoint `/storage_info` de la WebUI omiten cualquier petición saliente hacia Movistar/O2 Cloud hasta que el usuario pulse explícitamente **Reanudar** (`/start`).

## [v2.2.12] - 2026-11-10

### 🛡️ Solución Definitiva a Bloqueos AWS CloudFront WAF por Huella TLS (JA3/JA4) y Optimización para HyperBackup (Issues #23 y #26)

- **Alineación de Huella TLS 1.2 + HTTP/2 con el Cliente Oficial de Windows (Causa Raíz de los 403 en el Arranque):**
  - Se identificó mediante análisis de tráfico que AWS CloudFront WAF Bot Control bloqueaba con `HTTP 403 (The request could not be satisfied)` el `ClientHello` híbrido TLS 1.3/1.2 por defecto de `crypto/tls` en Go (firma JA3/JA4 catalogada como cliente automatizado cuando se combina con `User-Agent: omh windows client 32.0.7.1`).
  - Se configura explícitamente `TLS 1.2` (`MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12`) junto con `ForceAttemptHTTP2: true` tanto en el transporte SAPI como en los clientes de login/renovación OAuth, igualando la negociación TLS del cliente oficial de escritorio de Windows y obteniendo respuestas `HTTP/2.0 200 OK` de forma inmediata y consistente desde el arranque.
- **Eliminación del Efecto Convoy (Estampida) y Pacing Global Mutex:**
  - Las peticiones que entran en pausa por enfriamiento WAF liberan ahora su slot de concurrencia (`sapiSem`) antes de dormir y lo re-adquieren de forma escalonada mediante un *pacer* global (`paceRequest`, configurable vía `SAPI_PACING_MS`, por defecto 50ms), impidiendo que múltiples goroutines despierten en el mismo milisegundo y re-disparen un bloqueo.
- **Renovación de TTL al Servir Caché Previa (*Stale-While-Error*) y Cero Consultas Redundantes:**
  - Cuando `getSubfolders`, `getMedia` o `GetStorageSpace` sirven datos de caché previa ante un error transitorio de red, se actualiza la marca temporal de la entrada en caché para evitar que cada segmento de ruta de HyperBackup vuelva a consultar SAPI repetidamente.
  - Ampliado el TTL de caché de directorios y medios a 120 segundos (manteniendo invalidación instantánea en memoria ante operaciones `PUT`, `MKCOL`, `DELETE` y `MOVE`).
  - El endpoint `/storage_info` del panel web comparte ahora la caché de cuota de `MovistarFS`, y `KeepAlive` omite sondeados si hay una ventana de enfriamiento WAF activa.

## [v2.2.11] - 2026-11-10

### 🛡️ Propagación de Errores de Listado (Issue #24) y Eliminación de Spam de Cuota (Issue #23)

- **Prevención de Falsos Listados Vacíos ante Errores de Red / WAF (Issue #24):**
  - Cuando SAPI falla al listar subcarpetas o archivos en una caché fría (sin datos previos en memoria), el proxy ya no devuelve un `HTTP 207 Multi-Status` con 0 elementos.
  - En su lugar, el error se propaga como `HTTP 500 Internal Server Error`, impidiendo que clientes de backup (Duplicati, restic, Synology HyperBackup, etc.) asuman erróneamente que los archivos remotos fueron eliminados (`MissingRemoteFiles`).
  - Añadido test de integración exhaustivo `TestPropfindDoesNotReturnEmptySuccessWhenProviderFails`.
  - Gracias a @Erdk2 por el análisis detallado y el caso de prueba reproducible en el Issue #24.

- **Deduplicación (*Singleflight*) y Caché Autónoma de Cuota (Issue #23):**
  - La consulta de espacio y cuota (`GetStorageSpace`) ahora utiliza *singleflight* y se almacena en caché durante 5 minutos, protegiendo SAPI de decenas de llamadas redundantes generadas por cada `PROPFIND` entrante al inyectar la cuota RFC 4331.
  - Si SAPI devuelve 403 o error de red, se reutiliza la cuota previa o un fallback de 5 minutos, evitando reintentar inmediatamente contra el WAF.
  - Se desacopla la cuota de la invalidación de archivos (`InvalidateCache` ya no descarta la cuota al crear o borrar ficheros).

## [v2.2.10] - 2026-11-09

### 🛡️ Solución Definitiva a Bloqueos AWS CloudFront WAF 403 (Issue #23)

- **Control de Concurrencia Saliente y Pacing hacia SAPI:**
  - Limitación estricta de peticiones concurrentes de metadatos salientes hacia Movistar/O2 Cloud SAPI (por defecto máx. 3 concurrentes, configurable vía `SAPI_MAX_CONCURRENT`) con espaciado (*pacing*) de 25-30ms para amortiguar ráfagas masivas de Synology HyperBackup, restic o backrest, evitando que el WAF de CloudFront interprete el tráfico como un ataque de denegación de servicio.
- **Circuit Breaker / Pausa Global de Seguridad (WAF Cooldown):**
  - Si CloudFront WAF activa un bloqueo HTTP 403, el proxy activa un silencio global de 30-40 segundos (`wafCooldownUntil`) deteniendo todas las peticiones salientes hacia SAPI.
  - Al no recibir ningún paquete durante la ventana, AWS WAF drena su contador de tasa (*rate-based rule*) y levanta el bloqueo de la IP automáticamente sin colapsar los reintentos.
- **Protección de Sesión en `KeepAlive` y `RefreshSession`:**
  - `KeepAlive` solo considera caducada la sesión si recibe un `HTTP 401 Unauthorized`. Los bloqueos 403 de CloudFront, 429 de tasa o errores transitorios ya no provocan falsos refrescos OAuth ni revocación accidental de credenciales.
  - `RefreshSession` preserva intactos los tokens en disco si el login SAPI es bloqueado por CloudFront WAF, impidiendo el bloqueo permanente del usuario.
- **Deduplicación en Vuelo (*Singleflight*) y Caché Resiliente en WebDAV:**
  - Deduplicación de peticiones concurrentes a la misma carpeta: si entran varias peticiones simultáneas para el mismo directorio, se ejecuta una única llamada a SAPI y las demás aguardan ese mismo resultado.
  - Fallback a caché previa (*serve stale*): si SAPI experimenta un bloqueo o rate limit temporal y la caché expiró recientemente, se sirve la información en memoria en lugar de fallar la petición WebDAV.
  - TTL de caché ampliado a 60 segundos por defecto.
- **Agradecimientos:**
  - A @jofera, @frjf y @uannight por los logs detallados y pruebas que permitieron diagnosticar la dinámica del bloqueo en el Issue #23.

## [v2.2.9] - 2026-11-09

### 🛡️ Resiliencia ante AWS CloudFront WAF y Validación de Creación de Carpetas

- **Resiliencia ante Bloqueos CloudFront WAF 403 (Issue #23):**
  - Se detectan e interceptan respuestas HTTP `403 Forbidden` generadas por el WAF de AWS CloudFront (`The request could not be satisfied`) ante ráfagas de peticiones de clientes de backup como Synology HyperBackup o restic.
  - Se aplica retroceso exponencial suave con jitter (hasta 5 reintentos con tope de 8s) en `DoRequest` y `UploadFileToFolder`, permitiendo que las ráfagas se procesen sin interrumpir las tareas de backup.
- **Validación Estricta de Errores en `CreateFolder` (Issue #21):**
  - Detección y parseo explícito de objetos de error en la respuesta de la API SAPI (como `FOL-1035: Maximum folder count reached for user`), impidiendo que el proxy devuelva éxito silencioso.
  - Validación de que el ID devuelto no sea `0`. Si SAPI no devuelve un ID válido o falla la creación, se propaga el error correspondiente al cliente WebDAV en lugar de crear carpetas fantasma con ID 0 que provocaban posteriores errores `MED-1030: Folder not found`.
- **Agradecimientos:**
  - A @basty68 por reportar detalladamente el comportamiento de `FOL-1035` en el Issue #21.
  - A @bytevictor por la investigación técnica y trazas de CloudFront WAF en el Issue #23.

## [v2.2.8] - 2026-11-07

### ⚡ Subida en Flujo Directo y Compatibilidad Windows WebClient
- **Subida en Flujo Concurrente (*Concurrent Spool + Stream*):**
  - Para subidas mayores a la cuota en RAM (`WEBDAV_BUFFER_MB`), el proxy ahora transmite concurrentemente los datos hacia Movistar/O2 Cloud SAPI conforme entran desde el cliente, guardando en paralelo una copia en `/tmp` por si hiciera falta reintentar.
  - Elimina los tiempos muertos de espera tras enviar el último byte (el archivo ya está transmitido a la nube en tiempo real), evitando que Windows WebClient cancele la petición con error por su timeout de 60 segundos (`SendReceiveTimeoutInSec`).
- **Soporte `PROPPATCH` sin truncado (`DeadPropsHolder`):**
  - Corrección de un fallo crítico donde la petición `PROPPATCH` enviada por Windows Explorer inmediatamente tras un `PUT` (para fijar atributos NTFS / Win32) abría el archivo con `os.O_RDWR` y truncaba el archivo recién subido a 0 bytes.
  - Los lectores y manejadores WebDAV implementan ahora `DeadPropsHolder`, respondiendo `200 OK` a las propiedades Win32 sin abrir escritores ni sobrescribir archivos en la nube.
- **Liberación de Bloqueos Huérfanos (`resilientLockSystem`):**
  - Implementación de un sistema de bloqueos resiliente que rastrea tokens y rutas para liberar automáticamente bloqueos WebDAV (`LOCK`) colgados cuando una subida se corta o se envía un `DELETE` incondicional, impidiendo errores de tipo `webdav: locked` o "el proceso no tiene acceso al archivo porque está bloqueado".
- **Documentación de Registro de Windows:**
  - Se añade `SendReceiveTimeoutInSec = 3600` a las recomendaciones oficiales de configuración de Windows WebClient en `README.md` y `docs/network-drive.md`.

## [v2.2.7] - 2026-11-06

### 🪟 Documentación y Solución Definitiva de Montaje en Windows (Issue #18)
- **Resolución del problema de reconexión persistente en Windows WebClient:**
  - Diagnóstico exhaustivo del comportamiento por diseño de Microsoft ([KB 2673544](https://learn.microsoft.com/en-US/troubleshoot/windows-client/networking/cannot-automatically-reconnect-dav-share)), por el cual el cliente nativo de Windows envía contraseñas vacías al reconectar tras reiniciar, provocando bucles de autenticación y bloqueos por límite de tasa (HTTP 429).
  - Guía oficial paso a paso y script PowerShell para montaje silencioso al inicio de sesión con clave protegida mediante Windows DPAPI (`ConvertTo-SecureString`) y parámetro `/persistent:no`.
  - Opciones de despliegue mediante Tarea Programada (`schtasks`) o carpeta de Inicio (`shell:startup`) sin ventanas emergentes.
  - Actualización de `README.md`, `docs/network-drive.md` y actualización del cuadro de honor en `CONTRIBUTORS.md` en agradecimiento a @aarizaga90.

## [v2.2.6] - 2026-11-06

### 🛡️ Integridad de Datos y Robustez en Re-subidas (`moveByReupload`) (PR #20)
- **Confirmación estricta del destino antes de borrar el origen:**
  - En renombrados con cambio de extensión que requieren re-subida, el archivo original en Movistar/O2 Cloud ya no se elimina inmediatamente tras el acuse del POST de subida.
  - Se verifica que el archivo de destino exista en SAPI (`GetMediaByID`), tenga un ID nuevo válido y que su tamaño coincida exactamente con el original.
  - Para archivos transmitidos en flujo (no cacheados en memoria), se re-descarga el archivo subido calculando y comparando su hash SHA-256 contra el origen. Si no coincide o se agota el tiempo de espera, la operación aborta y el archivo de origen se conserva intacto.
- **Protección frente a lecturas con cuerpo vacío de SAPI (`getBodyGuard`):**
  - Si Movistar/O2 Cloud responde a peticiones `GET`/`HEAD` con código `200`/`206` y `Content-Length > 0` pero no entrega ningún byte (sucede ocasionalmente con archivos recién subidos que aún no han sido indexados por el backend), el proxy intercepta la respuesta y devuelve un error `503 Service Unavailable` completo con mensaje descriptivo, evitando que clientes WebDAV (Duplicati, Synology, GVFS, etc.) acepten archivos truncados de 0 bytes como válidos.
- **Bloqueo ordenado por ruta (`acquireFolderLock`):**
  - Serialización de operaciones `MOVE`, `PUT` y `DELETE` sobre el mismo fichero mediante ordenación lexicográfica de bloqueos para prevenir condiciones de carrera e interbloqueos (*deadlocks*).
- *Agradecimiento a @aarizaga90 por la implementación, pruebas y contribución en el PR #20.*

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
