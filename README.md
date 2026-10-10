# Movistar Cloud WebDAV Proxy ☁️🔐

[![Docker](https://img.shields.io/badge/Docker-Ready-blue?logo=docker)](https://www.docker.com/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![WebDAV](https://img.shields.io/badge/WebDAV-RFC%204918%20%7C%20RFC%204331-green)](https://tools.ietf.org/html/rfc4918)

Un servidor proxy WebDAV en Docker que traduce las peticiones WebDAV estándar de herramientas de copia de seguridad (**Duplicati**, **Synology HyperBackup**, etc.) hacia la API propietaria de **Movistar Cloud España (sección Ficheros)** en tiempo real.

Permite utilizar tu almacenamiento ilimitado de Movistar Cloud como destino seguro y transparente para tus copias de seguridad de servidores, NAS y equipos personales.

---

## 🌟 Características Principales

* 🔄 **Traducción Completa de Métodos WebDAV (RFC 4918):**
  * `PROPFIND` (listado con paginación automática sin límites de 200 items de Movistar).
  * `PUT` (subida de archivos con búfer temporal en disco para soportar archivos de cualquier tamaño sin agotar la RAM).
  * `GET` y `HEAD` (descargas con decodificación de chunks y compresión gzip en bruto para evitar fallos de *Hash Mismatch* en verificaciones de integridad).
  * `MKCOL` (creación de carpetas con resolución inmediata de ID y compatibilidad con directorios anidados de tareas `.hbk`).
  * `DELETE` (eliminación limpia de archivos y carpetas).
  * `MOVE` y `COPY` (renombrado y movimiento nativo en la nube, esencial para el ciclo `.tmp` -> `.zip.aes` de Duplicati).
  * `LOCK` y `UNLOCK` (gestión de bloqueos en memoria mediante WsgiDAV).
* 📊 **Soporte de Cuotas RFC 4331:**
  * Informa espacio disponible (`quota-available-bytes`) y usado para que los asistentes de Synology DSM y herramientas de backup validen el destino sin errores de *"Cuota insuficiente"*.
* 🛡️ **Tolerancia a Eventual Consistency y Colisiones:**
  * Oculta los retrasos de indexación de la CDN de Movistar mediante una memoria intermedia inteligente y reintentos automáticos.
  * Elimina archivos obsoletos antes de sobrescribir para prevenir que Movistar duplique archivos con sufijos ` (1)`.
  * Filtro de normalización al vuelo para garantizar que Synology siempre reciba nombres canónicos.
* 🚦 **Semáforo de Concurrencia (Máx. 3 transferencias):**
  * Limita estrictamente las subidas y descargas pesadas simultáneas a 3 conexiones concurrentes para trabajar en la zona óptima de la API de Movistar Cloud sin bloqueos por saturación (*Rate Limits*), manteniendo los hilos de control siempre fluidos.
* 🔑 **Autenticación Permisiva para Clientes:**
  * Puedes introducir cualquier usuario y contraseña en tu cliente WebDAV (Synology DSM o Duplicati obligan a rellenar estos campos); el proxy intercepta y autoriza la petición internamente contra las cookies activas de tu cuenta.
* 📱 **Tokens Nativos de Dispositivo Android (OAuth 2.0 Permanente):**
  * Simula ser un dispositivo Android nativo registrado (`omh android client` con identificador único de dispositivo `X-deviceid` y `refresh_token`).
  * **Sesión independiente y multi-dispositivo:** Ya no te desconecta si inicias sesión por la web en otro ordenador; el proxy mantiene su token de dispositivo dedicado sin conflicto.
* 💓 **Keep-Alive y Renovación Automática:**
  * Si un `access_token` caduca, el proxy lo renueva silenciosamente usando el `refresh_token` de dispositivo. Si se usan cookies web de respaldo, un hilo demonio realiza un *heartbeat* cada 5 minutos.
* 🖥️ **Asistente de Login Gráfico noVNC Integrado:**
  * Permite iniciar sesión fácilmente recibiendo el código SMS en una ventana de navegador virtual (Chromium en Xvfb + noVNC) desde el puerto 5000; el proxy intercepta el flujo OAuth nativo, registra el dispositivo en Movistar Cloud y arranca el WebDAV.
* 🚀 **Auto-reinicio Autónomo:**
  * Si el servidor se reinicia o el contenedor se actualiza, el proxy detecta automáticamente las credenciales guardadas en `/app/tokens` y arranca el servicio WebDAV en milisegundos sin requerir intervención manual.

---

## 🐳 Despliegue con Docker

### Opción 1: Docker Compose (Recomendado)

Crea un archivo `docker-compose.yml`:

```yaml
version: '3.8'

services:
  movistar-webdav-proxy:
    image: uniextra/movistar-webdav-proxy:latest
    build: .
    container_name: movistar-webdav-proxy
    restart: unless-stopped
    ports:
      - "5000:5000"      # Panel Web de gestión y login
      - "5050:8080"      # Puerto WebDAV (Usa 5050 en el host para no chocar con Portainer 8080)
      - "8084:8084"      # VNC / noVNC para login visual por SMS
    volumes:
      - movistar_tokens:/app/tokens

volumes:
  movistar_tokens:
    name: movistar_webdav_tokens
```

Inicia el servicio:
```bash
docker compose up -d
```

### Opción 2: Docker CLI

```bash
docker run -d \
  --name movistar-webdav-proxy \
  --restart unless-stopped \
  -p 5000:5000 \
  -p 5050:8080 \
  -p 8084:8084 \
  -v movistar_webdav_tokens:/app/tokens \
  uniextra/movistar-webdav-proxy:latest
```

---

## 🔑 Primer Inicio y Autenticación (Vinculación de Dispositivo)

1. Abre tu navegador web y accede al panel de control: `http://IP_DE_TU_SERVIDOR:5000`.
2. Introduce tu número de teléfono móvil asociado a Movistar Cloud y pulsa **Iniciar Login**.
3. Se abrirá una ventana flotante con el asistente visual noVNC.
4. Introduce el código SMS de verificación que recibirás en tu móvil.
5. El proxy interceptará el código OAuth, intercambiará el token con la API de Movistar, registrará el dispositivo nativo (`WebDAV Proxy Android`) y guardará los tokens persistentes en `/app/tokens`.
6. En el panel verás el estado en verde: `📱 Dispositivo Nativo Android` y el servicio WebDAV arrancará de inmediato en el puerto `8080` (o `5050` según tu mapeo). ¡A partir de este momento, puedes iniciar sesión por la web en cualquier otro ordenador sin afectar a este proxy!

---

## ⚙️ Configuración en Clientes de Backup

### 1. Configuración en Duplicati

En el asistente de destino de Duplicati:

| Parámetro | Valor |
| :--- | :--- |
| **Tipo de Almacenamiento** | `WebDAV` |
| **Servidor** | IP de tu servidor (ej: `192.168.1.100`) |
| **Puerto** | `5050` (o el puerto externo que hayas mapeado al `8080` del contenedor) |
| **Ruta / Carpeta** | Nombre de la carpeta de destino (ej: `Backup_Duplicati` o `/Backup_Duplicati/`) |
| **Conexión integrada (SSL)** | Desactivado (salvo que uses un reverse proxy HTTPS externo) |
| **Nombre de usuario** | Cualquier valor (ej: `test` o `admin`) |
| **Contraseña** | Cualquier valor (ej: `test` o `password`) |

> [!TIP]
> Puedes pulsar **"Probar conexión"** y Duplicati verificará permisos y creará la carpeta automáticamente si no existía.

---

### 2. Configuración en Synology HyperBackup

En el asistente de HyperBackup en DSM:

1. Selecciona la opción **WebDAV**.
2. Rellena los datos de conexión:
   * **Dirección del servidor:** `http://IP_DE_TU_HOST_DOCKER:5050`
   * **Nombre de usuario:** `admin` (o cualquier usuario)
   * **Contraseña:** `admin` (o cualquier clave)
   * **Carpeta compartida:** Selecciona una carpeta dedicada para HyperBackup (ej: `HyperBackup_NAS`).
   * **Directorio:** Nombre del repositorio (ej: `Server_Backup.hbk`).
3. Finaliza el asistente. La tarea quedará en estado **Online** y lista para ejecutarse de forma programada o manual.

> [!IMPORTANT]
> **Recomendación:** Mantén carpetas separadas en Movistar Cloud para Duplicati y HyperBackup (por ejemplo, `/Duplicati_Server/` y `/HyperBackup_NAS/`) para que cada herramienta gestione sus índices de forma aislada.

---

## 🧪 Tests y Validación

El proyecto incluye una suite de pruebas unitarias y de integración que verifica la conformidad del protocolo WebDAV:

```bash
python test_webdav_suite.py
```

Valida:
* Concurrencia de streams de descarga.
* Normalización de sufijos ` (1)` y extensiones `.hbk`.
* Reporte de cuotas RFC 4331.
* Rechazo de respuestas JSON con código 200 ante errores de API.

---

## 📄 Licencia

Este proyecto está bajo la Licencia [MIT](LICENSE).
