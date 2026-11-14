# 🌐 Modo web (opcional)

En el modo web el proxy no usa el inicio de sesión del cliente de escritorio
(OAuth `omh windows client`), sino **la misma sesión que la web oficial**
(`micloud.movistar.es`): un Chromium sin pantalla dentro del contenedor guarda la
sesión y las llamadas a SAPI se hacen desde esa pestaña, igual que la web.

Por ahora es **solo para Movistar Cloud** (`micloud.movistar.es`); no está probado
con O2 Cloud.

Úsalo si el emparejamiento por SMS del cliente de escritorio falla o CloudFront lo
bloquea (`403`), o si prefieres que el tráfico sea idéntico al de la web. El modo
normal no cambia y sigue siendo el recomendado si te funciona: su imagen ocupa
~20 MB; la del modo web, varios cientos de MB por Chromium.

## Puesta en marcha

```bash
docker compose -f docker-compose.web.yml up -d --build
```

1. Abre el panel (`http://IP_DE_TU_SERVIDOR:5000`). La tarjeta **Sesión web**
   sustituye a la de vinculación.
2. Pulsa **🔐 Abrir inicio de sesión**: verás la página de Movistar Cloud del
   Chromium del contenedor y podrás usarla con el ratón y el teclado.
3. Entra con **«Inicia sesión con e-mail y contraseña»** y marca **«No cerrar
   sesión»** (ver *Renovación automática*). También puedes entrar con el móvil y
   el SMS, pero entonces la sesión no se renueva sola.
4. La tarjeta pasa a **Activa** y el servidor WebDAV empieza a funcionar.

La sesión se guarda en el volumen de `/app/tokens` (perfil de Chromium en
`/app/tokens/webprofile`) y sobrevive a reinicios y actualizaciones.

## Cómo funciona

| Operación | Cómo la hace el proxy |
|---|---|
| Llamadas a SAPI (listados, carpetas, renombrar, borrar…) | Desde la pestaña de la web, con sus cookies, la `validationkey` y la cabecera `X-deviceid: web-…` |
| Subidas | Directas a `upload.micloud.movistar.es` con las cookies de la sesión, como el formulario de la web: `acceptasynchronous=true` solo para archivos de más de 200 MiB (`minAsyncUploadFileSizeBytes` de la web) |
| Validación tras subir | Si SAPI responde `MED-1017` (el archivo aún se está validando) al moverlo, se consulta `/media?action=get-validation-status`, como la web, en vez de reintentar a ciegas |
| Descargas | Por la URL firmada del archivo (admite `Range`, sin cookies). Si caduca (dura unas horas), se pide otra y se reintenta |
| Borrado | `/media/<tipo>?action=delete&softdelete=true` (archivos) y `/media/folder?action=softdelete` (carpetas), con el cuerpo JSON de la web. Si SAPI lo rechaza, se usa la llamada genérica de siempre |
| `COPY` de WebDAV | Copia en la nube (`/media/file?action=copy`), sin descargar ni volver a subir. Si no es posible, la copia normal |
| Concurrencia | 2 subidas (`defaultConcurrentUploads` de la web) y 4 descargas simultáneas |

## Renovación automática de la sesión

La sesión de la web (`JSESSIONID`) **caduca a las 24 h del inicio de sesión**,
aunque haya actividad. Si se entró con **«No cerrar sesión»**, Movistar guarda
además una cookie de larga duración (`PLC`, ~90 días) con la que la web vuelve a
entrar sola.

El proxy aprovecha eso: una hora antes de la caducidad (`WEB_SESSION_RENEW_BEFORE`)
borra la cookie de sesión y recarga la web, que entra sola con una sesión nueva de
24 h, sin contraseña ni código. Mientras lo hace, el resto de llamadas esperan:
la `PLC` cambia en cada uso y, si dos peticiones la usaran a la vez, Movistar la
anularía. Por la misma razón las subidas directas no la envían.

El panel muestra cuándo caduca la sesión y si la renovación está activa. Si la
renovación falla, el panel lo indica y basta con volver a iniciar sesión.

> Si entras con el móvil y el SMS no hay `PLC`: la sesión durará 24 h y habrá que
> volver a entrar.

## Papelera

Todo lo que borra el proxy va a la papelera de Movistar Cloud, y la papelera
cuenta para el límite de carpetas de la cuenta. Con
`WEB_TRASH_EMPTY_EVERY=6h` el proxy la vacía periódicamente (`/media/trash?action=empty`,
el botón de la web). Está **desactivado por defecto**: vaciarla borra para siempre
todo lo que contenga. Actívalo si lo que se borra son sobre todo temporales (por
ejemplo, los trozos del `chunker` de rclone).

## Opciones de la cuenta

El panel muestra las opciones que la web lee al cargar
(`/system/information`): tamaño máximo por archivo, límite de elementos por
operación al borrar o mover carpetas (error `FOL-1036`) y caducidad automática de
la papelera. En una cuenta de Movistar Cloud (noviembre de 2026) eran: 4096 MB por
archivo, 1000 elementos por operación de carpeta y 86 400 min (60 días) en la
papelera.

## Variables de entorno del modo web

| Variable | Por defecto | Descripción |
|---|---|---|
| `WEB_SESSION` | `false` (`true` en `Dockerfile.web`) | Activa el modo web. |
| `CHROME_PATH` | autodetectar (`/usr/bin/chromium` en la imagen) | Ejecutable de Chromium/Chrome. |
| `CHROME_NO_SANDBOX` | `false` (`true` en la imagen) | Lanza Chromium sin sandbox (necesario como root en contenedores). |
| `WEB_SESSION_RENEW_BEFORE` | `1h` | Margen antes de la caducidad de 24 h para renovar la sesión. |
| `WEB_TRASH_EMPTY_EVERY` | *(desactivado)* | Vaciado periódico de la papelera (p. ej. `6h`; mínimo `1m`). |
| `WEBDAV_MAX_DOWNLOADS` | `1` (`4` en modo web) | Descargas simultáneas a la nube. |
| `WEBDAV_MAX_UPLOADS` | `1` (`2` en modo web) | Subidas simultáneas a la nube. |
| `SAPI_MAX_CONCURRENT` | `1` (en modo web, descargas + subidas + 2) | Llamadas simultáneas a SAPI. |

## Uso con rclone `chunker` + `crypt`

Configuración probada con varios TB de datos:

```ini
[movistar]
type = webdav
url = http://IP_DE_TU_SERVIDOR:5050
vendor = owncloud        # envía X-OC-Mtime: se conserva la fecha de modificación
user = admin
pass = ...

[movistar-chunk]
type = chunker
remote = movistar:datos
chunk_size = 1Gi
name_format = *-chunk###   # SIN punto: ver nota
hash_type = none
transactions = norename

[movistar-crypt]
type = crypt
remote = movistar-chunk:
```

**Nota sobre la longitud de los nombres:** Movistar Cloud admite nombres de hasta
255 caracteres y recorta los más largos sin avisar. `crypt` (codificación
`base32`, la estándar) rellena cada nombre a bloques de 16 bytes y lo alarga un
60 %, así que un nombre original de 144 bytes o más (unos 140 caracteres sin
acentos) ya no cabe. El proxy rechaza esos nombres con `400` para
que rclone lo informe como error; si te pasa, acorta el nombre o usa
`filename_encoding = base64` (nombres más cortos) en un remoto `crypt` nuevo.

**Nota sobre `name_format`:** con el formato por defecto (`*.rclone_chunk.###`)
el chunker renombra cada trozo cambiando su *extensión*, y SAPI no permite cambiar
la extensión de un archivo (`MED-1050`): el proxy tiene que volver a subirlo
entero. Con `*-chunk###` el renombrado es instantáneo.
