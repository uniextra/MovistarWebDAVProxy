# Configuración Avanzada, Seguridad y Red 🔧🔒

Esta guía reúne toda la referencia técnica del proxy: variables de entorno completas, alertas de Telegram, arquitectura de autenticación y seguridad, configuración de proxies inversos (Nginx/NPM/Traefik), red Docker, filtro de basura del SO y limitaciones técnicas de la API de Movistar / O2 Cloud.

---

## 📋 1. Tabla Completa de Variables de Entorno

### 🔐 Autenticación y Acceso

| Variable | Por defecto | Descripción |
| :--- | :--- | :--- |
| `WEBDAV_USER` | *(vacío)* | Usuario para clientes WebDAV (y para el panel web si `WEBUI_USER` no se define). |
| `WEBDAV_PASSWORD` | *(vacío)* | Contraseña en texto plano para clientes WebDAV (necesaria si algún cliente usa NTLMv2). |
| `WEBDAV_PASSWORD_HASH` | *(vacío)* | Alternativa a `WEBDAV_PASSWORD`: hash SHA-256 en hexadecimal (ej. `sha256:e3b0c44...`). |
| `WEBUI_USER` | `WEBDAV_USER` | Usuario específico para el panel de control web (`:5000`). |
| `WEBUI_PASSWORD` | `WEBDAV_PASSWORD` | Contraseña específica para el panel de control web. |
| `WEBUI_PASSWORD_HASH` | *(vacío)* | Alternativa a `WEBUI_PASSWORD`: hash SHA-256 en hexadecimal para el panel web. |
| `REQUIRE_AUTH` | `false` | Si es `true`, el servidor aborta el arranque si no se han configurado credenciales. |
| `AUTH_DISABLED` | `false` | Si es `true`, desactiva explícitamente la autenticación tanto en WebDAV como en el panel web. |
| `WEBDAV_NTLM` | `auto` | Controla el soporte NTLMv2: en `auto` se ofrece a clientes Windows cuando hay `WEBDAV_PASSWORD`. Admite `true`/`always`/`1` o `false`/`0`/`no`/`disabled`. |

### 🚪 Puertos, Rutas y Almacenamiento

| Variable | Por defecto | Descripción |
| :--- | :--- | :--- |
| `PORT` | `5000` | Puerto interno del servidor del Panel Web. |
| `WEBDAV_PORT` | `8080` | Puerto interno del servidor WebDAV. |
| `WEBDAV_HOST_PORT` | `5050` | Puerto público de WebDAV mostrado en el panel; en Docker Compose toma el valor de `WEBDAV_PORT`. |
| `TOKENS_DIR` | `/app/tokens` | Directorio donde se almacenan las credenciales OAuth2 (`0700`) y la caché SQLite. |
| `TOKENS_ENCRYPTION_KEY` | *(vacío)* | Clave opcional para cifrar en reposo (**AES-256-GCM**) los tokens y secretos guardados en `account_<tel>.env`. |
| `WEBDAV_TMPDIR` | `/tmp` *(o `TMPDIR`)* | Directorio para archivos temporales durante subidas mayores al búfer RAM. Si montas un volumen en disco (ej. `/app/tmp`), define `WEBDAV_TMPDIR=/app/tmp` para usarlo. |
| `TZ` | `Europe/Madrid` | Zona horaria del contenedor (usada en marcas de tiempo de logs y alertas). |

### 🔔 Notificaciones de Telegram (PR #53)

| Variable | Por defecto | Descripción |
| :--- | :--- | :--- |
| `TELEGRAM_BOT_TOKEN` | *(vacío)* | Token del bot de Telegram para alertas operativas y de sesión. |
| `TELEGRAM_CHAT_ID` | *(vacío)* | Identificador del chat o canal de Telegram que recibirá las alertas. |

> [!TIP]
> **Funcionamiento desde el segundo cero:** Desde la versión **v2.5.6** (PR #53), el notificador de Telegram es global y carga `TELEGRAM_BOT_TOKEN` y `TELEGRAM_CHAT_ID` automáticamente al iniciar el proceso, sin necesidad de abrir el panel web previamente. Si además guardas credenciales desde el panel web en `/app/tokens`, estas actualizan las activas al cargar la cuenta; si posteriormente desvinculas la cuenta o limpias esos campos en la web, el notificador vuelve a usar automáticamente las variables de entorno definidas en Docker.

### ⚡ Rendimiento, Concurrencia y Red

| Variable | Por defecto | Descripción |
| :--- | :--- | :--- |
| `MAX_UPLOAD_MB` | `4096` | Tamaño máximo permitido por archivo en subidas `PUT` (en MB). |
| `WEBDAV_BUFFER_MB` | `32` | Tamaño máximo del búfer en RAM (MB) antes de hacer spool a disco (óptimo para Duplicati y Restic). |
| `WEBDAV_MAX_DOWNLOADS` | `1` en el perfil `desktop`; `4` en el resto | Descargas simultáneas hacia la nube. Como el cliente oficial, que transfiere de una en una y no baja mientras sube. |
| `WEBDAV_MAX_UPLOADS` | `1` en el perfil `desktop`; `2` en el resto | Subidas simultáneas hacia la nube. Como el cliente oficial: en la medición del 19/11/2026 subió 3 archivos de 1 GB por una sola conexión con `upload.*`, uno tras otro (~30 MB/s). |
| `UPLOAD_RESPONSE_TIMEOUT_SEC` | `300` | Tiempo máximo de espera (segundos) tras enviar el archivo hasta que el servidor de subida responde. |
| `SYNC_INTERVAL_SEC` | `300` en `desktop`, `60` en `browser` | Intervalo de sincronización incremental de metadatos (mínimo `15` segundos). En el perfil `desktop` es la cadencia del cliente oficial (consulta los cambios cada 5 minutos). |
| `WEBDAV_STREAM_UPLOAD` | `true` (perfil `desktop`) | Sube a la nube mientras el archivo aún llega del cliente, como el cliente oficial: la respuesta llega justo después del último byte, sin esperas largas que agoten el tiempo del cliente (rclone corta a los 5 min). La subida al host de subidas no ocupa el hueco de `SAPI_MAX_CONCURRENT`, así que varias subidas a la vez no se bloquean entre sí. Sin la variable, activado donde el proxy habla como la app (perfil `desktop`, ver `SAPI_OFFICIAL_CLIENT`); con `SAPI_OFFICIAL_CLIENT=false`, como antes. |
| `WEBDAV_CASE_INSENSITIVE` | `false` (perfil `desktop`) | La nube distingue mayúsculas («Foto.jpg» y «foto.jpg» son dos archivos) y el proxy también: busca nombres exactos (si no, rclone podía dar por subido un archivo que no lo estaba o borrar el origen en un cambio solo de mayúsculas). Con `true`, si no hay un archivo con el nombre exacto se usa el que solo difiere en mayúsculas, útil con el Explorador de Windows o el Finder; no lo actives con rclone ni restic. Sin la variable, exactos donde el proxy habla como la app (perfil `desktop`) y, con `SAPI_OFFICIAL_CLIENT=false`, sin distinguir, como antes. |
| `SHUTDOWN_TIMEOUT_SEC` | `30` | Tiempo máximo de espera (segundos) para un apagado ordenado de subidas en curso. |
| `SAPI_MAX_CONCURRENT` | `1` *(ver nota)* | Llamadas concurrentes máximas a SAPI. En [Modo Web](web-mode.md) toma por defecto `WEBDAV_MAX_DOWNLOADS + WEBDAV_MAX_UPLOADS + 2`. |
| `SAPI_PACING_MS` | `100` | Pausa mínima en milisegundos entre peticiones secuenciales a SAPI. |
| `TRUSTED_PROXIES` | *(vacío)* | Lista separada por comas de IPs/CIDRs de proxies inversos de confianza para evaluar `X-Forwarded-For` / `X-Real-IP`. |
| `BRUTE_FORCE_EXCLUDED_IPS` | `127.0.0.1,::1,172.16.0.0/12` | IPs/subredes exentas de bloqueo HTTP `429` por intentos fallidos (cubre las redes bridge de Docker `172.17.0.0/16`, `172.18.0.0/16`, etc.; mantiene Basic Auth activo). |
| `LOG_LEVEL` | *(vacío)* | Si se establece en `debug`, habilita trazas detalladas `[DEBUG]` en el registro del panel. |

### 🕵️ Huella TLS, User-Agent y Modo Web

| Variable | Por defecto | Descripción |
| :--- | :--- | :--- |
| `SAPI_CLIENT_PROFILE` | `desktop` (`browser` en Modo Web) | Cómo se presenta el proxy ante SAPI. `desktop` imita al cliente oficial de Windows: HTTP/1.1, `User-Agent` `omh windows client 4.0.1`, `X-deviceid: fol-…`, `"platform":"windows"`, sus mismas cabeceras, y renovación de la sesión a través de SAPI (sin canjear el `refresh_token` en `apiseg`). `browser` mantiene el comportamiento anterior: navegador Chrome por HTTP/2 con `X-deviceid: web-…`. Ver la sección 4. |
| `DESKTOP_USER_AGENT` | `omh windows client 4.0.1` | `User-Agent` del cliente de escritorio: lo usan el canje del código OAuth en `apiseg.telefonica.es` y, en el perfil `desktop`, las llamadas a SAPI. |
| `SAPI_USER_AGENT` | *(según perfil)* | Fuerza el `User-Agent` enviado a SAPI. Por defecto: `DESKTOP_USER_AGENT` en el perfil `desktop`, `Mozilla/5.0 ... Chrome/133.0.0.0 ...` en `browser`. *(No usar `DESKTOP`: CloudFront lo rechaza con `401`.)* |
| `OAUTH_REFRESH_FALLBACK` | `false` | Solo perfil `desktop`: si SAPI rechaza la sesión, permite como último recurso canjear el `refresh_token` en `apiseg` (el cliente oficial no lo hace nunca). |
| `UTLS_CLIENT_HELLO` | *(según perfil)* | Huella TLS (`ClientHello`) hacia SAPI. Por defecto: `o2` (la del cliente oficial de Windows) en el perfil `desktop`, `chrome_auto` en `browser`. Otros valores para pruebas: `golang`, `chrome_auto`, `firefox_auto`, `safari_auto`, `ios`, `android`, `edge`, `chrome_120`, `randomized`. Ver la sección 4. |
| `WEB_SESSION` | `false` | Activa el [Modo Web](web-mode.md) (requiere la imagen `Dockerfile.web`, donde viene a `true`). Sus variables específicas (`WEB_SESSION_RENEW_BEFORE`, `WEB_TRASH_EMPTY_EVERY`, `CHROME_PATH`, `CHROME_NO_SANDBOX`) se detallan en esa guía. |

---

## 🔔 2. Sistema de Alertas por Telegram

Con `TELEGRAM_BOT_TOKEN` y `TELEGRAM_CHAT_ID` configurados en tu `docker-compose.yml` (o desde el panel web), el proxy envía notificaciones automáticas ante los siguientes eventos:

* 🚀 **Arranque del servicio:** Confirma que el contenedor ha iniciado y muestra los puertos de escucha de WebDAV y Panel Web.
* ⚠️ **Cuenta no vinculada o sesión web pendiente:** Si arranca sin tokens guardados (o en Modo Web sin sesión iniciada), avisa inmediatamente para que accedas al panel y completes el inicio de sesión.
* ❌ **Fallos críticos de inicialización:** Notifica antes de detenerse si falla la inicialización del cliente SAPI o el arranque de Chromium en Modo Web.
* ⚠️ **Sesión caducada:** Avisa cuando la sesión de Movistar / O2 Cloud expira y requiere revinculación (protegida con control antispam de **1 hora** por tipo de alerta para evitar mensajes repetitivos).

---

## 🔍 3. Diagnóstico de User-Agent en los Logs

En el modo estándar (cuando `WEB_SESSION` no está activo), el proxy registra explícitamente al arrancar el valor activo de `SAPI_USER_AGENT` y `DESKTOP_USER_AGENT`, indicando si se está utilizando el valor por defecto o una versión personalizada por variable de entorno:

```text
[Auth] 🔧 Perfil de cliente SAPI: desktop (como el cliente oficial de Windows: HTTP/1.1, X-deviceid fol-, sin renovar en apiseg). SAPI_CLIENT_PROFILE=browser para el perfil de navegador.
[Auth] 🔧 SAPI_USER_AGENT usando valor por defecto: omh windows client 4.0.1
[Auth] 🔧 DESKTOP_USER_AGENT usando valor por defecto: omh windows client 4.0.1
```

Esto facilita comprobar de inmediato qué cabeceras se están enviando si necesitas diagnosticar un rechazo `401` o `403` en el inicio de sesión o en llamadas a la API.

---

## 🖥️ 4. Perfil de cliente de escritorio

El perfil por defecto fuera del Modo Web (`SAPI_CLIENT_PROFILE=desktop`) reproduce el tráfico del cliente oficial de Windows de O2 (`O2 Cloud.exe` 32.1.3, instalador publicado en `/sapi/system/information` → `clientspathinfo.windowspath`), capturado con Fiddler:

* **Transporte:** HTTP/1.1 hacia SAPI (el cliente es Qt 5).
* **Huella TLS:** el `ClientHello` del cliente oficial (Qt 5 + OpenSSL 1.1.1), capturado sin intermediario con `tcpdump` (la captura con Fiddler muestra el HTTP, pero el `ClientHello` que ve CloudFront es el de Fiddler): JA3 `57fbe0aefee44901190849b0e877a5e1`, 512 bytes, TLS 1.3 a 1.0, grupos x25519, P-256, x448, P-521 y P-384, y **sin ALPN** (el servidor responde en HTTP/1.1). El proxy lo reproduce con uTLS (`UTLS_CLIENT_HELLO=o2`, valor por defecto en este perfil). Las subidas y descargas por HTTP/1.1 (`upload.*` y URLs firmadas) usan la misma huella, como la app; con `SAPI_OFFICIAL_CLIENT=false`, la pila TLS estándar de Go (ver más abajo).
* **Cabeceras:** `User-Agent: omh windows client 4.0.1`, `x-deviceid: fol-<base64>`, `Accept: */*`, `Accept-Language: es-ES,en,*`, `x-request-id` (UUID por petición), `Authorization: oauth …` con `"platform":"windows"`, cookie `JSESSIONID` y `validationkey` en la URL. Sin `Referer`, `X-Requested-With` ni `X-devicename`.
* **Renovación:** el cliente envía sus tokens aunque `lastrefreshdate + expiresin` haya vencido; SAPI responde `200` con una cabecera `Authorization: oauth …` (en la captura, con los mismos tokens y sin `lastrefreshdate`) y el cliente pone `lastrefreshdate` a la hora de esa respuesta. No contacta nunca con `apiseg.telefonica.es` para renovar.

CloudFront exige esa coherencia: un `X-deviceid` `fol-…` llega a SAPI con `User-Agent` `omh windows client …` **por HTTP/1.1**, pero se corta (`401`, `Server: CloudFront`, 0 bytes) si llega por HTTP/2, con `User-Agent` de navegador o con `DESKTOP`.

La huella TLS también cuenta: con la de Chrome (`chrome_auto`) y el `User-Agent` del cliente de Windows, CloudFront acabó devolviendo esos `401` vacíos a todas las peticiones tras unas 11 horas de funcionamiento normal, mientras que con las mismas credenciales y la huella `golang` la sesión se renovó sin problemas. Con `o2` el proxy presenta la misma huella que la aplicación oficial.


### Hablar como el cliente oficial

Además de presentarse como la app, en el perfil `desktop` el proxy **hace las cosas como la app**: sube en dos pasos (`save-metadata` y el contenido en bruto), reanuda las subidas cortadas, renombra, mueve, crea y borra con las mismas peticiones, late con la misma cadencia, relee cada archivo subido y sigue su validación.

> [!NOTE]
> Movistar Cloud y O2 Cloud son el mismo servicio, así que el proxy se comporta igual con los dos (comprobado con ambos). Si algo falla, `SAPI_OFFICIAL_CLIENT=false` vuelve al comportamiento anterior: subida multipart y operaciones de antes.

Todas estas opciones se leen en `internal/config/opciones.go`. Las marcadas con *(app)* imitan los tiempos del cliente oficial: cambiarlas aleja al proxy de su comportamiento.

| Variable | Por defecto | Descripción |
| :--- | :--- | :--- |
| `SAPI_OFFICIAL_CLIENT` | `true` | `true`: hablar como la app, con Movistar y con O2. `false`: nunca (subida multipart y operaciones de antes). `auto`, el valor anterior, equivale a `true`. |
| `SAPI_REFRESH_AFTER_UPLOAD` | `true` | Tras cada subida, esperar a que O2 valide el archivo y releerlo (`media?action=get`) para guardar su URL de descarga firmada, como la app. En segundo plano. Sin ella, la primera descarga de cada archivo subido pasa por la URL por defecto, que O2 rechaza (`COM-1005`), y necesita una consulta más. |
| `SAPI_UNSUPPORTED_AS_FILE` | `false` | Si O2 rechaza una imagen, audio o vídeo porque su contenido no corresponde a la extensión (`MED-1050`, por ejemplo un `.jpg` dañado), volver a subirlo como archivo genérico (`upload/file`) para no perderlo. La app no lo hace: deja el archivo sin subir. Solo para archivos nuevos; una sobrescritura sigue rechazada. |
| `SAPI_VALIDATION_FIRST_SEC` | `10` *(app)* | Segundos hasta la primera consulta de validación tras subir. |
| `SAPI_VALIDATION_MAX_SEC` | `60` *(app)* | Espera máxima entre consultas de validación (crecen desde la primera hasta aquí). No hay límite de tiempo total: se sigue hasta que O2 termina. |
| `SAPI_VALIDATION_MAX_FAILURES` | `60` | Consultas de validación seguidas que pueden fallar antes de dejar de seguir un archivo (~1 h). |
| `SAPI_UPLOAD_ATTEMPTS` | `8` | Intentos de envío del contenido tras un corte (cada uno reanuda donde se quedó). |
| `SAPI_UPLOAD_RETRY_MAX_SEC` | `60` | Espera máxima entre intentos de envío (2, 5, 10, 20, 30, 45, 60 s…). Con los valores por defecto aguanta unos 3 minutos sin red. |
| `SAPI_HEARTBEAT_SEC` | `60` *(app)* | Cada cuánto se consulta `system/information`. |
| `SAPI_HEARTBEAT_FULL_SEC` | `300` *(app)* | Cada cuánto se hace la ronda completa (plan, suscripción, rol, versión, espacio). |
| `SAPI_LOGIN_RETRY_FIRST_SEC` | `30` | Tras un login fallido, espera hasta el siguiente; se dobla en cada fallo. |
| `SAPI_LOGIN_RETRY_MAX_SEC` | `600` | Tope de esa espera. |
| `SAPI_LOGIN_GIVEUP_REJECTIONS` | `4` | Rechazos de credenciales necesarios para dar la sesión por caducada (pedir revincular)… |
| `SAPI_LOGIN_GIVEUP_SEC` | `1800` | …durante al menos estos segundos. Los cortes, 5xx, 403 del WAF y 401 vacíos de CloudFront nunca cuentan. |
| `SAPI_ESCAPE_NAMES` | `true` | Guardar los nombres de archivo que O2 no admite cambiando un punto por `%2E` en la nube. Ver «Nombres que O2 no admite» más abajo. |
| `WEBDAV_REAL_EMPTY_FILES` | `true` | Los archivos de 0 bytes se suben a la nube como cualquier otro, en vez de emularse en SQLite y en el manifiesto `.movistar-webdav-proxy/empty-files.json`. Ver «Archivos vacíos» más abajo. `false`: la emulación de antes. |
| `WEBDAV_EMPTY_MANIFEST_RETRY_SEC` | `120` | Si no se pudo subir el manifiesto de archivos vacíos, se reintenta tras estos segundos. |
| `WEBDAV_COPY_POLL_SEC` | `10` | Copia en la nube (`COPY`) sin respuesta: cada cuánto se busca si apareció. |
| `WEBDAV_COPY_WAIT_BASE_SEC` | `120` | Espera mínima por esa copia… |
| `WEBDAV_COPY_WAIT_MBPS` | `10` | …más un segundo por cada estos MB del archivo… |
| `WEBDAV_COPY_WAIT_MAX_SEC` | `600` | …hasta este máximo. Si no aparece, `503` para que el cliente reintente. |

Los valores en segundos admiten decimales (`0.5`). Un valor vacío o no válido usa el de por defecto.

**Nombres que O2 no admite.** O2 rechaza los archivos cuyo nombre empieza por punto (`.env`, `.gitignore`), los que terminan en `.tmp` (en mayúsculas o minúsculas) y quita el punto final de los que terminan en punto. Para poder guardarlos, el proxy escribe ese punto como `%2E` en la nube y lo vuelve a poner al leer: `.env` se guarda como `%2Eenv`, `x.tmp` como `x%2Etmp` y `x.` como `x%2E`. Por WebDAV se ven con su nombre real; en la web o la app de O2, con `%2E`. Las carpetas no lo necesitan. Un archivo subido por otra vía con `%2E` en esas posiciones se verá con el punto. Las sondas de Duplicati (`duplicati-*.tmp`) y los archivos de sistema (`.DS_Store`, `desktop.ini`) se siguen absorbiendo sin subirlos.

**Archivos vacíos.** O2 guarda archivos de 0 bytes como cualquier otro (registro con tamaño 0 y envío vacío); la app oficial no los sube porque los ignora, no porque O2 no los admita. Por eso el proxy los sube de verdad (`WEBDAV_REAL_EMPTY_FILES`): están en la nube y una copia que los usa (Hyper Backup guarda en ellos el estado de la copia) se puede recuperar desde otro equipo y otro proxy sin depender de nada local. Los vacíos de bloqueo de Hyper Backup (`lock_*`, `@reader`…), que viven segundos, siguen solo en el proxy. Al arrancar, los vacíos que seguían emulados (de una versión anterior o del manifiesto) se suben una vez; el manifiesto queda solo con los que no se hayan podido subir. Si en `Control/@writer` falta el archivo de estado de Hyper Backup (`v1.N.-1.0.0.none.none.cinfo`), el proxy lo repone con la versión de `Config/version_info.db.N` (antes ponía siempre `v1.1`, y Hyper Backup respondía «The backup data on the cloud is older than the NAS's cache»); si solo hay un `v1.1` puesto así por una versión anterior y `Config` dice otra cosa, añade el correcto.

**Archivos que O2 no admite (`MED-1050`).** O2 comprueba que el contenido de las imágenes (`jpg`, `png`, `heic`…) corresponda a su extensión; el de los audios y vídeos no lo comprueba (un `.mp3` o un `.mp4` cualquiera se guarda tal cual). Si una imagen no corresponde, rechaza el archivo. Con un `.heic` dañado O2 no responde `MED-1050` sino `MED-1000`, el mismo código que da cuando falla toda la API; por eso el proxy lo reintenta una vez y, si se repite, lo trata igual. Los `.heif` se suben como archivo genérico, porque O2 no los admite como imagen ni siquiera con una foto real. La app oficial lo marca como no admitido y no lo reintenta; el proxy hace lo mismo y responde **`415 Unsupported Media Type`** (sin `Retry-After`), para que el cliente lo señale y siga con lo demás en vez de reintentarlo sin fin. Con `SAPI_UNSUPPORTED_AS_FILE=true` se sube como archivo genérico.
---

## 🔒 5. Seguridad y Autenticación Detallada

* **Panel Web con Login Moderno (`:5000`):**
  * Formulario de inicio de sesión compatible con autocompletado de gestores de contraseñas (*Bitwarden*, *1Password*, *KeePass*, *Google Chrome*, *Apple Keychain*).
  * Sesiones basadas en cookies `HttpOnly` / `SameSite`, cabeceras de seguridad (`Content-Security-Policy`, `Cross-Origin-Opener-Policy`, `Cross-Origin-Resource-Policy`, `Permissions-Policy`), validación de origen frente a CSRF y cierre de sesión explícito (`/logout`).
* **Clientes WebDAV (`:5050` -> `:8080`, RFC 4918):**
  * Compatibilidad estándar con **HTTP Basic Auth** y **NTLMv2** (para *Duplicati*, *Synology HyperBackup*, *Rclone*, *Windows WebClient*, *macOS Finder*).
  * **Validación en Tiempo Constante:** Tanto el panel web como WebDAV validan usuario y contraseña usando `subtle.ConstantTimeCompare` sobre hashes SHA-256 para prevenir ataques de temporización.
  * **Normalización de usuarios Windows:** Los prefijos de dominio habituales de Windows WebClient (`DOMAIN\usuario` o `\usuario`) y formatos UPN (`usuario@dominio`) se normalizan automáticamente a `usuario`, ignorando espacios accidentales.
  * **Preservación intacta de contraseñas:** La contraseña se evalúa exactamente tal como se envía, permitiendo contraseñas complejas con dos puntos (`:`) o espacios internos.
  * **Desafío RFC 4918 (`401 Unauthorized`):** Toda petición WebDAV no autenticada o con credenciales erróneas recibe un `401 Unauthorized` junto a `WWW-Authenticate: Basic realm="WebDAV"`.
  * **Descubrimiento Windows WebClient:** La petición inicial `OPTIONS /` sin credenciales devuelve `200 OK` con `MS-Author-Via: DAV` y cabeceras `DAV: 1, 2`, permitiendo al explorador de Windows identificar el servicio antes de solicitar credenciales en `PROPFIND /`.
* **Contenedor sin privilegios (`non-root`):**
  * La imagen oficial ejecuta el servidor bajo el usuario sin privilegios `app` (`UID 10001`). En actualizaciones sobre volúmenes existentes, el script `docker-entrypoint.sh` ajusta los permisos de `/app/tokens` (y de `WEBDAV_TMPDIR` si está definido) antes de soltar privilegios con `su-exec`.
* **Cifrado de tokens en reposo:**
  * Si defines `TOKENS_ENCRYPTION_KEY`, los secretos almacenados en `account_<telefono>.env` (`MOVISTAR_JSESSIONID`, `MOVISTAR_VALIDATIONKEY`, `MOVISTAR_REFRESH_TOKEN`, `MOVISTAR_ACCESS_TOKEN`, `MOVISTAR_TELEGRAM_BOT_TOKEN`) se cifran con **AES-256-GCM**.

---

## 🌐 6. Topologías de Red Docker y Proxies Inversos

* **No exponer directamente a Internet:** Si necesitas acceder desde fuera de tu red local, coloca siempre un proxy inverso con **TLS/HTTPS** delante (Caddy, Nginx, Traefik) o utiliza un túnel VPN (WireGuard / Tailscale).
* **Docker Bridge (por defecto):** Mediante `BRUTE_FORCE_EXCLUDED_IPS` (`127.0.0.1,::1,172.16.0.0/12`), el loopback y las subredes puente estándar de Docker (`172.17.0.0/16`, `172.18.0.0/16`, etc.) quedan exentas del bloqueo por fuerza bruta (`HTTP 429`). Esto evita que varios clientes compartiendo el gateway de Docker queden bloqueados accidentalmente. **La autenticación Basic Auth se exige y valida en todo momento; la exclusión nunca elude la contraseña.**
* **Proxy Inverso (Nginx, Nginx Proxy Manager, Traefik, Caddy, Synology DSM Reverse Proxy):**
  * Al configurar `TRUSTED_PROXIES` (ej. `127.0.0.1,172.18.0.0/16`), el proxy extrae la IP real del cliente de `X-Forwarded-For` o `X-Real-IP`. Si la petición proviene de un origen no confiable, las cabeceras de reenvío se descartan estrictamente para evitar spoofing.
  * ⚠️ **Importante para subidas grandes (`413 Request Entity Too Large`):** Si sitúas el contenedor detrás de **Nginx** o **Nginx Proxy Manager (NPM)**, desactiva el límite de tamaño de cuerpo y el buffering en la pestaña *Advanced* (*Custom Nginx Configuration*) para no cortar subidas WebDAV de varios GB:
    ```nginx
    client_max_body_size 0;
    proxy_request_buffering off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    ```
  * **Nota para clientes Linux GVFS (Thunar / Nautilus `davs://`):** `gvfsd-dav` / `libsoup` presenta una limitación histórica de entero con signo de 32 bits (**2 GiB**) por archivo; para subir archivos de entre 2 GB y 4 GB desde Linux utiliza **Rclone** o `curl`.
* **Docker Host (`network_mode: host`):** En Linux nativo permite observar la IP real directa del socket. En Docker Desktop (Windows/macOS), debido a la máquina virtual interna, la topología recomendada es el modo bridge con mapeo de puertos o proxy inverso.

---

## 🧹 7. Filtro Inteligente de Basura del SO (*DevNull Absorber*)

Al conectar la unidad en red, los sistemas operativos intentan crear archivos de indexación, miniaturas y papeleras que ensucian la nube. El proxy integra un absorbedor en memoria que responde con éxito al cliente pero evita subir estos elementos a Movistar / O2 Cloud:

* **Windows:** `desktop.ini`, `autorun.inf`, `autorun.ini`, `thumbs.db`, `ehthumbs.db`, `ehthumbs_vista.db`, carpetas `$RECYCLE.BIN` y `System Volume Information`.
* **macOS:** `.DS_Store`, archivos AppleDouble `._*`, `.Spotlight-V100`, `.Trashes`, `.fseventsd`, `.TemporaryItems`, `.DocumentRevisions-V100`, `.apdisk`, `.VolumeIcon.icns`.
* **Linux:** Carpetas `.Trash-*` y archivos `.directory` de KDE.

---

## ⚠️ 8. Limitaciones Técnicas de Movistar / O2 Cloud (SAPI)

Aunque Movistar Cloud y O2 Cloud ofrecen **almacenamiento total ilimitado**, la infraestructura de Telefónica (SAPI) impone ciertas restricciones técnicas:

1. **📦 Límite de tamaño por archivo individual (4 GB / 4096 MB):**
   * La API SAPI rechaza cualquier archivo individual que supere los **4 GB (4096 MB)**.
   * **Duplicati** y **Synology HyperBackup** no se ven afectados porque fragmentan las copias en volúmenes o bloques pequeños.
   * Si necesitas subir archivos individuales mayores de 4 GB mediante **Rclone**, configura el overlay `chunker` siguiendo la [Guía de Clientes de Backup](backup-clients.md#-configuración-avanzada-archivos-de-más-de-4-gb-chunker-y-cifrado-crypt).
2. **🚦 Concurrencia y Protección frente a CloudFront WAF (`403`):**
   * El proxy controla la concurrencia mediante semáforos configurables (`WEBDAV_MAX_DOWNLOADS=4`, `WEBDAV_MAX_UPLOADS=2`, `SAPI_MAX_CONCURRENT=1` en modo estándar) y un *circuit breaker* por servidor (`waf_breaker.go`) que pausa temporalmente las peticiones si CloudFront responde `403`, devolviendo `503 + Retry-After` a los clientes en lugar de lanzar ráfagas de reintentos.
3. **📏 Longitud máxima de nombres (255 caracteres):**
   * Movistar Cloud recorta silenciosamente los nombres que superan los **255 caracteres**. Para evitar pérdidas o ficheros ilegibles (especialmente con `rclone crypt`), el proxy rechaza con `HTTP 400` cualquier operación `PUT`, `MKCOL`, `MOVE` o `COPY` cuyo nombre de destino exceda los 255 caracteres.
4. **🔤 Caracteres reservados en nombres de archivo:**
   * Evita caracteres incompatibles con URLs o APIs REST (`\`, `:`, `*`, `?`, `"`, `<`, `>`, `|`) en nombres de archivos y directorios.
